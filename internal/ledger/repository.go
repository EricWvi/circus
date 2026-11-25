package ledger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"golang.org/x/sys/unix"
)

const (
	mainRef        = plumbing.ReferenceName("refs/heads/main")
	repoSuffix     = ".git"
	initPrefix     = ".init-"
	refPrefix      = ".main-"
	trashName      = ".trash"
	maxCommitBytes = 64 << 10
	maxTreeBytes   = (store.MaxLedgerDecisions + 1) * (255 + 32)
)

// Repository I/O is bounded across projects, with no shared Git object
// cache. The queue for each project is released when its last user leaves.
type Manager struct {
	root   string
	st     store.LedgerOps
	now    func() time.Time
	logger *slog.Logger
	slots  chan struct{}
	mu     sync.Mutex
	locks  map[string]*projectLock
	broken map[string]*projectFault
	faults objectFaults
	// retryBudget bounds one check of a broken project.
	retryBudget time.Duration
	// Checks run on checkCtx, which Close cancels, and checks counts them.
	checkCtx   context.Context
	stopChecks context.CancelFunc
	checkSlots chan struct{}
	// deleting counts the deletions in progress per project.
	deleting map[string]int
	checks   sync.WaitGroup
	closed   bool
}

// objectFaults remembers objects whose reads failed. A commit that reuses
// one of them from its parent verifies it in full and rewrites it from the
// caller's bytes when it is damaged.
type objectFaults struct {
	mu     sync.Mutex
	hashes map[plumbing.Hash]struct{}
}

const maxObjectFaults = 4096

func (f *objectFaults) add(hash plumbing.Hash) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hashes == nil || len(f.hashes) == maxObjectFaults {
		f.hashes = make(map[plumbing.Hash]struct{})
	}
	f.hashes[hash] = struct{}{}
}

func (f *objectFaults) has(hash plumbing.Hash) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.hashes[hash]
	return ok
}

func (f *objectFaults) remove(hash plumbing.Hash) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.hashes, hash)
}

// A project whose repository could not be read is checked again on a read
// after its retry delay, which doubles after each failed check. A
// transient error, such as EIO, therefore clears without a restart.
type projectFault struct {
	err      error
	retry    time.Time
	delay    time.Duration
	checking bool // a check is running; readers return err meanwhile
	// stale marks an index that is empty while the repository has a main
	// line, as after a refused project delete. Readers keep the index as
	// it is until the check has rebuilt it.
	stale bool
}

const (
	minProjectRetry = time.Second
	maxProjectRetry = time.Minute
	// A check may rebuild the index from the whole history, so it runs
	// with its own budget rather than the reader's deadline.
	projectRetryBudget = 10 * time.Minute
	// At most this many checks run at once across projects. A reader that
	// finds them all busy returns the recorded error and tries later.
	maxProjectChecks = 2
)

type projectLock struct {
	ready chan struct{}
	users int
}

// New opens the private ledger directory beside the SQLite database.
// Reconcile should be called before the server starts accepting requests.
func New(dbPath string, st store.LedgerOps, now func() time.Time, logger *slog.Logger) (*Manager, error) {
	if dbPath == "" {
		return nil, errors.New("ledger: database path is required")
	}
	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(filepath.Dir(abs), "ledger")
	if err := privateDir(root); err != nil {
		return nil, fmt.Errorf("ledger: create repository directory: %w", err)
	}
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	checkCtx, stopChecks := context.WithCancel(context.Background())
	return &Manager{root: root, st: st, now: now, logger: logger, slots: make(chan struct{}, 4), locks: make(map[string]*projectLock),
		retryBudget: projectRetryBudget, checkCtx: checkCtx, stopChecks: stopChecks,
		checkSlots: make(chan struct{}, maxProjectChecks)}, nil
}

func privateDir(dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", dir)
	}
	if info.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

// ValidProjectID accepts project IDs that can name one repository directory.
func ValidProjectID(id string) bool {
	return store.CheckID("project id", id) == nil && id != "." && id != ".." &&
		filepath.IsLocal(id) && utf8.ValidString(id) && !strings.ContainsAny(id, "/\\\x00")
}

func (m *Manager) acquire(ctx context.Context, id string) (func(), error) {
	if !ValidProjectID(id) {
		return nil, fmt.Errorf("%w: invalid ledger project id", store.ErrInvalid)
	}
	m.mu.Lock()
	lock := m.locks[id]
	if lock == nil {
		lock = &projectLock{ready: make(chan struct{}, 1)}
		m.locks[id] = lock
	}
	lock.users++
	m.mu.Unlock()

	drop := func() {
		m.mu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(m.locks, id)
		}
		m.mu.Unlock()
	}
	select {
	case lock.ready <- struct{}{}:
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
	select {
	case m.slots <- struct{}{}:
		return func() {
			<-m.slots
			<-lock.ready
			drop()
		}, nil
	case <-ctx.Done():
		<-lock.ready
		drop()
		return nil, ctx.Err()
	}
}

// CheckProject returns the error that made the project's repository
// unreadable, if any. Once the retry delay has passed, one caller starts a
// check of the repository, which may rebuild a stale index, and waits for
// it while ctx allows. Only one check of a project runs at a time, at most
// maxProjectChecks across projects, each detached from ctx within its own
// budget, so a reader that gives up does not cancel it. The error clears
// when the check succeeds.
//
// An index that is empty while the repository has a main line, as left by
// a refused project delete, is checked the same way, but readers are not
// refused meanwhile. The caller must not hold the project's queue.
func (m *Manager) CheckProject(ctx context.Context, id string) error {
	if err := m.noticeClearedIndex(ctx, id); err != nil {
		return err
	}
	m.mu.Lock()
	fault := m.broken[id]
	if fault == nil {
		m.mu.Unlock()
		return nil
	}
	err, stale, now := fault.err, fault.stale, time.Now()
	if stale {
		err = nil
	}
	if fault.checking || now.Before(fault.retry) || m.closed || stale && m.deleting[id] > 0 {
		m.mu.Unlock()
		return err
	}
	select {
	case m.checkSlots <- struct{}{}:
	default:
		m.mu.Unlock()
		return err
	}
	fault.checking = true
	fault.retry = now.Add(fault.delay)
	m.checks.Add(1)
	m.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		defer m.checks.Done()
		defer func() { <-m.checkSlots }()
		done <- m.retryProject(id)
	}()
	select {
	case checkErr := <-done:
		if checkErr == nil || stale {
			return nil
		}
		return m.projectError(id, err)
	case <-ctx.Done():
		return err
	}
}

// Deleting marks a project whose deletion is clearing its ledger index,
// until the returned function is called. Reads meanwhile serve the index
// as it is and start no rebuild of it.
func (m *Manager) Deleting(id string) func() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.deleting == nil {
		m.deleting = make(map[string]int)
	}
	m.deleting[id]++
	return func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.deleting[id]--; m.deleting[id] == 0 {
			delete(m.deleting, id)
		}
	}
}

// noticeClearedIndex records a project whose indexed head is empty while
// its repository has a main line. Only a project with a repository costs
// a store read.
func (m *Manager) noticeClearedIndex(ctx context.Context, id string) error {
	m.mu.Lock()
	known := m.broken[id] != nil || m.deleting[id] > 0
	m.mu.Unlock()
	if known {
		return nil
	}
	if _, err := os.Lstat(m.repoPath(id)); err != nil {
		return nil
	}
	head, err := m.st.GetLedgerHead(ctx, id)
	if err != nil || head != "" {
		return err
	}
	// A repository may hold only changesets' branches, before the first
	// merge.
	if _, err := os.Lstat(filepath.Join(m.repoPath(id), mainRef.String())); errors.Is(err, os.ErrNotExist) {
		if r, err := m.open(m.repoPath(id)); err == nil && r != nil {
			main, err := r.head()
			_ = r.storage.Close()
			if err == nil && main.IsZero() {
				return nil
			}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.broken == nil {
		m.broken = make(map[string]*projectFault)
	}
	if m.broken[id] == nil {
		m.broken[id] = &projectFault{err: fmt.Errorf("ledger project %q: index is empty", id), delay: minProjectRetry, stale: true}
	}
	return nil
}

// retryProject checks a project again and records the outcome. A failure,
// including running out of budget, doubles the retry delay.
func (m *Manager) retryProject(id string) error {
	ctx, cancel := context.WithTimeout(m.checkCtx, m.retryBudget)
	defer cancel()
	checkErr := m.reconcileProject(ctx, id, false)
	gone := false
	if checkErr != nil {
		// A project deleted meanwhile needs no check.
		_, exists, err := m.st.LedgerProjectName(ctx, id)
		gone = err == nil && !exists
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	fault := m.broken[id]
	if fault != nil {
		fault.checking = false
	}
	switch {
	case checkErr == nil:
		delete(m.broken, id)
		m.logger.InfoContext(ctx, "ledger index checked", "project_id", id)
		return nil
	case gone:
		delete(m.broken, id)
		m.logger.DebugContext(ctx, "ledger check ended: project deleted", "project_id", id, "error", checkErr)
		return nil
	case errors.Is(checkErr, store.ErrBusy):
		// Another writer holds the project; try again after the delay.
		return checkErr
	}
	m.logger.ErrorContext(ctx, "ledger repository still unreadable", "project_id", id, "error", checkErr)
	if fault != nil {
		fault.err = fmt.Errorf("ledger project %q: %w", id, checkErr)
		fault.delay = min(2*fault.delay, maxProjectRetry)
		fault.retry = time.Now().Add(fault.delay)
	}
	return checkErr
}

// Close stops the checks of broken projects and waits for them, or until
// ctx ends. Later reads still report recorded errors but start no checks.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.stopChecks()
	done := make(chan struct{})
	go func() {
		m.checks.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// projectError returns the project's recorded error, or fallback when
// none is recorded any more.
func (m *Manager) projectError(id string, fallback error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if fault := m.broken[id]; fault != nil {
		return fault.err
	}
	return fallback
}

func (m *Manager) setProjectError(id string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.broken, id)
		return
	}
	if m.broken == nil {
		m.broken = make(map[string]*projectFault)
	}
	m.broken[id] = &projectFault{err: fmt.Errorf("ledger project %q: %w", id, err), retry: time.Now().Add(minProjectRetry), delay: minProjectRetry}
}

func (m *Manager) repoPath(id string) string {
	return filepath.Join(m.root, id+repoSuffix)
}

type repository struct {
	*git.Repository
	storage      *filesystem.Storage
	dir          string
	dirtyObjects map[string]struct{}
	faults       *objectFaults
}

// open opens a project's repository, sharing the manager's record of
// objects that failed to read.
func (m *Manager) open(dir string) (*repository, error) {
	r, err := openRepository(dir)
	if r != nil {
		r.faults = &m.faults
	}
	return r, err
}

func gitStorage(dir string) *filesystem.Storage {
	return filesystem.NewStorageWithOptions(osfs.New(dir), cache.NewObjectLRU(4*cache.MiByte), filesystem.Options{
		LargeObjectThreshold: maxCommitBytes,
	})
}

func openRepository(dir string) (*repository, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("ledger: %q is not a directory", dir)
	}
	s := gitStorage(dir)
	r, err := git.Open(s, nil)
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	cfg, err := r.Config()
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	if !cfg.Core.IsBare {
		return nil, errors.Join(errors.New("ledger: repository must be bare"), s.Close())
	}
	return &repository{Repository: r, storage: s, dir: dir}, nil
}

func initRepository(dir string) (*repository, error) {
	s := gitStorage(dir)
	r, err := git.InitWithOptions(s, nil, git.InitOptions{DefaultBranch: mainRef})
	if err != nil {
		return nil, errors.Join(err, s.Close())
	}
	return &repository{Repository: r, storage: s, dir: dir}, nil
}

func (r *repository) head() (plumbing.Hash, error) {
	return r.readRef(mainRef)
}

// readRef returns the commit a reference names directly, or zero when the
// reference does not exist.
func (r *repository) readRef(name plumbing.ReferenceName) (plumbing.Hash, error) {
	ref, err := r.Reference(name, false)
	if errors.Is(err, plumbing.ErrReferenceNotFound) {
		return plumbing.ZeroHash, nil
	}
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if ref.Type() != plumbing.HashReference || ref.Hash().IsZero() {
		if name == mainRef {
			return plumbing.ZeroHash, errors.New("ledger: main must be a direct commit reference")
		}
		return plumbing.ZeroHash, fmt.Errorf("ledger: %s must be a direct commit reference", name)
	}
	return ref.Hash(), nil
}

func commitID(h plumbing.Hash) string {
	if h.IsZero() {
		return ""
	}
	return h.String()
}

func (r *repository) object(kind plumbing.ObjectType, hash plumbing.Hash, limit int64) (plumbing.EncodedObject, error) {
	size, err := r.storage.EncodedObjectSize(hash)
	if err != nil {
		return nil, err
	}
	if size > limit {
		return nil, fmt.Errorf("ledger: %s object %s exceeds %d bytes", kind, hash, limit)
	}
	return r.storage.EncodedObject(kind, hash)
}

func (r *repository) commit(hash plumbing.Hash) (*object.Commit, error) {
	o, err := r.object(plumbing.CommitObject, hash, maxCommitBytes)
	if err != nil {
		return nil, err
	}
	c, err := object.DecodeCommit(r.storage, o)
	if err == nil && c.Hash != hash {
		return nil, fmt.Errorf("ledger: corrupt commit object %s", hash)
	}
	return c, err
}

func (r *repository) tree(hash plumbing.Hash) (*object.Tree, error) {
	o, err := r.object(plumbing.TreeObject, hash, maxTreeBytes)
	if err != nil {
		return nil, err
	}
	tree, err := object.DecodeTree(r.storage, o)
	if err == nil && tree.Hash != hash {
		return nil, fmt.Errorf("ledger: corrupt tree object %s", hash)
	}
	return tree, err
}

func (r *repository) blob(hash plumbing.Hash, limit int64, buffer []byte) (_ []byte, err error) {
	if hash == emptyIndexHash && int64(len(emptyIndex)) <= limit {
		// The empty ledger's index: no commit stores it, so a repository
		// may lack it (one whose main line began with CommitMain, read as
		// the base of a changeset opened on the empty ledger).
		return append(buffer[:0], emptyIndex...), nil
	}
	defer func() {
		if err != nil && r.faults != nil {
			r.faults.add(hash)
		}
	}()
	o, err := r.object(plumbing.BlobObject, hash, limit)
	if err != nil {
		return nil, err
	}
	reader, err := o.Reader()
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	size := o.Size()
	if size < 0 || size > limit {
		return nil, errors.New("ledger: invalid blob size")
	}
	if int64(cap(buffer)) < size {
		buffer = make([]byte, size)
	}
	data := buffer[:size]
	if _, err := io.ReadFull(reader, data); err != nil {
		return nil, err
	}
	var extra [1]byte
	if _, err := io.ReadFull(reader, extra[:]); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("ledger: blob exceeds its declared size")
		}
		return nil, err
	}
	return data, nil
}

// Remove moves a repository out of service before recursively removing it.
// A leftover trash directory is safe to clear at startup.
func (m *Manager) Remove(ctx context.Context, projectID string) error {
	release, err := m.acquire(ctx, projectID)
	if err != nil {
		return err
	}
	defer release()
	unlockRoot, err := m.lockRoot(unix.LOCK_SH)
	if err != nil {
		return err
	}
	defer unlockRoot()
	if err := m.remove(projectID); err != nil {
		return err
	}
	m.setProjectError(projectID, nil)
	return nil
}

func (m *Manager) remove(projectID string) error {
	src := m.repoPath(projectID)
	if _, err := os.Lstat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	trash := filepath.Join(m.root, trashName)
	if err := privateDir(trash); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(trash, "project-")
	if err != nil {
		return err
	}
	if err := os.Rename(src, filepath.Join(dir, "repo.git")); err != nil {
		_ = os.Remove(dir)
		return err
	}
	return os.RemoveAll(dir)
}
