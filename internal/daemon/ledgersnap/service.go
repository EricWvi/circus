package ledgersnap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/circushome"
	"github.com/EricWvi/circus/internal/daemon/workspace"
	"github.com/EricWvi/circus/internal/ledger/snapshot"
	"golang.org/x/sys/unix"
)

const (
	// MaxCalls bounds the ledger calls running at once on a daemon.
	MaxCalls = 2
	// StagingTTL is how long staging may stay untouched before any ledger
	// call removes it.
	StagingTTL = time.Hour
	// maxChunkBytes and maxChunkFiles bound one prepare_ledger chunk, and
	// the edits of one rebase_ledger.
	maxChunkBytes = 8 << 20
	maxChunkFiles = 4096
	// maxIDBytes bounds a snapshot id.
	maxIDBytes = 128
	// maxDetailBytes bounds error_detail and a problem's message.
	maxDetailBytes = 1024
	// maxProblems bounds CollectLedgerResult.problems.
	maxProblems = 1024
	// maxStagingScan is how many names one directory read returns.
	maxStagingScan = 1024

	manifestsDir = ".manifests"
	stagingDir   = ".staging"
	// reservedID is the circus ledger CLI's directory beside the snapshots.
	reservedID = "cli"
)

type errorCode = circusv1.LedgerSnapshotError

const (
	errInvalidRequest   = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_INVALID_REQUEST
	errStateDirUnknown  = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_STATE_DIR_UNKNOWN
	errUnsupportedOS    = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_UNSUPPORTED_OS
	errPathConflict     = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_PATH_CONFLICT
	errPermissionDenied = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_PERMISSION_DENIED
	errNoSpace          = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_NO_SPACE
	errAbsent           = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_ABSENT
	errStale            = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_STALE
	errBusy             = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_BUSY
	errIO               = circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_IO
)

// Options configures the ledger tools. Zero values use the host.
type Options struct {
	// Home overrides the daemon user's home directory.
	Home string
	// Root overrides the ledger directory, <state>/circus/ledger.
	Root string
	// Logger receives diagnostics.
	Logger *slog.Logger
}

// Tool is one ledger device call; it satisfies daemon.Tool.
type Tool interface {
	Name() string
	Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error)
}

// Service holds what the four ledger tools share: the call slots.
type Service struct {
	opts  Options
	slots chan struct{}
	// afterApply, for tests, runs between a rebase's edits and its rebuild.
	afterApply func()
	// beforeEdit, for tests, runs after an edit's file was checked again
	// and before the edit is applied.
	beforeEdit func(path string)
}

// New constructs the ledger tools' shared state.
func New(opts Options) *Service {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Service{opts: opts, slots: make(chan struct{}, MaxCalls)}
}

// Tools returns prepare_ledger, collect_ledger, rebase_ledger and
// remove_ledger.
func (s *Service) Tools() []Tool {
	return []Tool{&prepareTool{s}, &collectTool{s}, &rebaseTool{s}, &removeTool{s}}
}

// failure is a FAILED outcome's error and detail.
type failure struct {
	code   errorCode
	detail string
}

func fail(code errorCode, format string, args ...any) *failure {
	return &failure{code: code, detail: clip(fmt.Sprintf(format, args...))}
}

// clip bounds s to maxDetailBytes of valid UTF-8.
func clip(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= maxDetailBytes {
		return s
	}
	cut := maxDetailBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// fsFailure classifies a file system error of step.
func fsFailure(step string, err error) *failure {
	code := errIO
	switch {
	case errors.Is(err, unix.ENOSPC), errors.Is(err, unix.EDQUOT):
		code = errNoSpace
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		code = errPermissionDenied
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.EISDIR):
		code = errPathConflict
	}
	return fail(code, "%s: %v", step, err)
}

func codeText(code errorCode) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(code.String(), "LEDGER_SNAPSHOT_ERROR_"), "_", " "))
}

// validID reports whether id is a usable snapshot id.
func validID(id string) bool {
	if id == "" || len(id) > maxIDBytes || strings.EqualFold(id, reservedID) {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// call is one ledger call holding a slot and its snapshot's lock.
type call struct {
	s    *Service
	id   string
	root *os.File
	// path is the snapshot's absolute, lexically clean path.
	path string
	lock *workspace.DirLock
}

// begin resolves the ledger directory (creating it when create is set),
// takes a slot and the snapshot's lock, and sweeps stale staging. Without
// create, a missing ledger directory returns a nil call and no failure.
func (s *Service) begin(ctx context.Context, id string, create bool) (*call, *failure) {
	if runtime.GOOS == "windows" {
		return nil, fail(errUnsupportedOS, "")
	}
	if !validID(id) {
		return nil, fail(errInvalidRequest, "invalid snapshot id")
	}
	rootPath, f := s.rootPath()
	if f != nil {
		return nil, f
	}
	root, f := openRoot(rootPath, create)
	if f != nil || root == nil {
		return nil, f
	}
	select {
	case s.slots <- struct{}{}:
	default:
		_ = root.Close()
		return nil, fail(errBusy, "%d ledger calls are already running on this device", MaxCalls)
	}
	lock, err := workspace.LockBeside(root, id)
	if err != nil {
		<-s.slots
		_ = root.Close()
		if errors.Is(err, workspace.ErrLockBusy) {
			return nil, fail(errBusy, "another call on this snapshot is running")
		}
		return nil, fsFailure("lock", err)
	}
	c := &call{s: s, id: id, root: root, path: filepath.Join(rootPath, id), lock: lock}
	s.sweep(ctx, root, id)
	return c, nil
}

func (c *call) end() {
	c.lock.Release()
	<-c.s.slots
	_ = c.root.Close()
}

// rootPath returns the ledger directory's absolute, clean path.
func (s *Service) rootPath() (string, *failure) {
	root := s.opts.Root
	if root == "" {
		var err error
		if root, err = circushome.State(s.opts.Home, "ledger"); err != nil {
			return "", fail(errStateDirUnknown, "%v", err)
		}
	}
	if !filepath.IsAbs(root) || !utf8.ValidString(root) {
		return "", fail(errStateDirUnknown, "the state directory is not an absolute path of valid UTF-8")
	}
	return filepath.Clean(root), nil
}

const dirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

// openRoot opens the ledger directory without following a symlink at it;
// its parents are created (with create) and resolved. Without create, a
// missing directory returns nil and no failure.
func openRoot(rootPath string, create bool) (*os.File, *failure) {
	parent, name := filepath.Dir(rootPath), filepath.Base(rootPath)
	if create {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return nil, fsFailure("create the state directory", err)
		}
	}
	real, err := filepath.EvalSymlinks(parent)
	if errors.Is(err, os.ErrNotExist) && !create {
		return nil, nil
	}
	if err != nil {
		return nil, fsFailure("resolve the state directory", err)
	}
	pfd, err := unix.Open(real, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fsFailure("open the state directory", err)
	}
	defer unix.Close(pfd)
	fd, err := openChildDir(pfd, name, create, 0o700)
	if errors.Is(err, unix.ENOENT) && !create {
		return nil, nil
	}
	if err != nil {
		return nil, fsFailure("open "+rootPath, err)
	}
	return os.NewFile(uintptr(fd), filepath.Join(real, name)), nil
}

// openChildDir opens the directory name inside parentFd without following
// a symlink, creating it with mode when create is set.
func openChildDir(parentFd int, name string, create bool, mode uint32) (int, error) {
	fd, err := unix.Openat(parentFd, name, dirFlags, 0)
	if errors.Is(err, unix.ENOENT) && create {
		if err = unix.Mkdirat(parentFd, name, mode); err == nil {
			err = snapshot.SyncDir(parentFd)
		}
		if err == nil || errors.Is(err, unix.EEXIST) {
			fd, err = unix.Openat(parentFd, name, dirFlags, 0)
		}
	}
	return fd, err
}

// openDir opens name inside the ledger directory as an *os.File; a missing
// directory without create is (nil, nil).
func (c *call) openDir(name string, create bool) (*os.File, error) {
	fd, err := openChildDir(int(c.root.Fd()), name, create, 0o700)
	if errors.Is(err, unix.ENOENT) && !create {
		return nil, nil
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	return os.NewFile(uintptr(fd), filepath.Join(c.root.Name(), name)), nil
}

// snapshotKind is what the ledger directory holds at an id.
type snapshotKind int

const (
	snapshotAbsent snapshotKind = iota
	// snapshotDir is a directory, with or without a manifest.
	snapshotDir
	// snapshotOther is a symlink or anything else that is not a directory.
	snapshotOther
)

func (c *call) snapshotKind() (snapshotKind, error) {
	var st unix.Stat_t
	err := unix.Fstatat(int(c.root.Fd()), c.id, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(err, unix.ENOENT):
		return snapshotAbsent, nil
	case err != nil:
		return 0, err
	case st.Mode&unix.S_IFMT == unix.S_IFDIR:
		return snapshotDir, nil
	}
	return snapshotOther, nil
}

func (c *call) manifestName() string { return c.id + ".json" }

// readManifest reads the snapshot's manifest; snapshot.ErrNoManifest when
// there is none.
func (c *call) readManifest() (*snapshot.Manifest, error) {
	dir, err := c.openDir(manifestsDir, false)
	if err != nil {
		return nil, err
	}
	if dir == nil {
		return nil, snapshot.ErrNoManifest
	}
	defer dir.Close()
	return snapshot.ReadManifest(dir, c.manifestName())
}

// openSnapshot opens the snapshot directory and reads its manifest. A
// missing directory or manifest is ABSENT; anything but a directory is
// PATH_CONFLICT.
func (c *call) openSnapshot() (*os.File, *snapshot.Manifest, *failure) {
	fd, err := unix.Openat(int(c.root.Fd()), c.id, dirFlags, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, nil, fail(errAbsent, "no snapshot at %s", c.path)
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return nil, nil, fail(errPathConflict, "%s is a symlink or not a directory", c.path)
	case err != nil:
		return nil, nil, fsFailure("open the snapshot", err)
	}
	dir := os.NewFile(uintptr(fd), c.path)
	m, err := c.readManifest()
	if errors.Is(err, snapshot.ErrNoManifest) {
		_ = dir.Close()
		return nil, nil, fail(errAbsent, "the snapshot at %s has no manifest", c.path)
	}
	if err != nil {
		_ = dir.Close()
		return nil, nil, fsFailure("read the manifest", err)
	}
	return dir, m, nil
}

// Staging names of an id, inside .staging.
func stateName(id string) string { return id + ".state" }
func clockName(id string) string { return id + ".clock" }
func tmpName(id string) string   { return id + ".tmp" }
func asideName(id string) string { return id + ".aside" }
func pathsName(id string, n uint32) string {
	return fmt.Sprintf("%s.paths.%d", id, n)
}

// recoverAside puts a file that a killed rebase moved aside back into the
// snapshot dir, or drops it when its path holds a newer file (see
// snapshot.Applier.Recover), so that a write of the agent's it may hold
// is collected rather than lost.
func (c *call) recoverAside(dir *os.File) *failure {
	staging, err := c.openDir(stagingDir, false)
	if err != nil {
		return fsFailure("open the staging directory", err)
	}
	if staging == nil {
		return nil
	}
	defer staging.Close()
	recovered, err := snapshot.NewApplier(dir, staging, tmpName(c.id), asideName(c.id)).Recover()
	if err != nil {
		return fsFailure("recover a file an interrupted rebase moved aside", err)
	}
	if recovered {
		c.s.opts.Logger.Info("recovered a file an interrupted rebase moved aside", "snapshot_id", c.id)
	}
	return nil
}

// removeStaging removes the staging entries of id: the staged directory,
// its chunk state and path lists, a replaced snapshot, a rebase's files
// and the clock probe. With keepAside, a file a killed rebase moved aside
// (and its journal) stays for Applier.Recover: it may hold a write of the
// agent's. It reports whether anything was removed.
func removeStaging(ctx context.Context, staging *os.File, id string, keepAside bool) (bool, error) {
	fd, removed := int(staging.Fd()), false
	for _, name := range []string{id, id + ".old"} {
		var st unix.Stat_t
		if err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			return removed, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR {
			if err := unix.Unlinkat(fd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
				return removed, err
			}
		} else if err := workspace.RemoveTree(ctx, fd, name); err != nil {
			return removed, err
		}
		removed = true
	}
	names := []string{stateName(id), stateName(id) + ".tmp", tmpName(id), clockName(id)}
	if !keepAside {
		names = append(names, asideName(id), asideName(id)+".path", asideName(id)+".path.tmp")
	}
	for _, name := range names {
		switch err := unix.Unlinkat(fd, name, 0); {
		case err == nil:
			removed = true
		case !errors.Is(err, unix.ENOENT):
			return removed, err
		}
	}
	// Chunk path lists are numbered from 0 without gaps: a chunk is
	// accepted only after the one before it, and chunk 0 removes them all.
	for n := uint32(0); ; n++ {
		err := unix.Unlinkat(fd, pathsName(id, n), 0)
		if errors.Is(err, unix.ENOENT) {
			break
		}
		if err != nil {
			return removed, err
		}
		removed = true
	}
	if removed {
		return true, staging.Sync()
	}
	return false, nil
}

// eachName calls fn with every name in dir, read in batches through a
// descriptor of its own; it stops early when ctx ends.
func eachName(ctx context.Context, dir *os.File, fn func(name string)) error {
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	d := os.NewFile(uintptr(fd), dir.Name())
	defer d.Close()
	for ctx.Err() == nil {
		names, err := d.Readdirnames(maxStagingScan)
		for _, name := range names {
			fn(name)
		}
		if errors.Is(err, io.EOF) || len(names) == 0 {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}

// sweep removes the staging of other ids untouched for StagingTTL, and
// the manifests of other ids whose snapshot directory is gone, each under
// its id's lock; ids whose lock is held are left for a later call. A file
// a killed rebase moved aside stays while its snapshot exists.
func (s *Service) sweep(ctx context.Context, root *os.File, own string) {
	rootFd := int(root.Fd())
	exists := func(id string) bool {
		var st unix.Stat_t
		return unix.Fstatat(rootFd, id, &st, unix.AT_SYMLINK_NOFOLLOW) == nil
	}
	if fd, err := unix.Openat(rootFd, stagingDir, dirFlags, 0); err == nil {
		staging := os.NewFile(uintptr(fd), filepath.Join(root.Name(), stagingDir))
		// One entry per id: the newest modification time of its staging.
		newest := map[string]int64{}
		_ = eachName(ctx, staging, func(name string) {
			id, _, _ := strings.Cut(name, ".")
			if id == own || !validID(id) {
				return
			}
			var st unix.Stat_t
			if unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW) != nil {
				return
			}
			newest[id] = max(newest[id], int64(st.Mtim.Sec)*1e9+int64(st.Mtim.Nsec)) //nolint:unconvert // int32 on some platforms
		})
		cutoff := time.Now().Add(-StagingTTL).UnixNano()
		for id, mtime := range newest {
			if mtime >= cutoff || ctx.Err() != nil {
				continue
			}
			lock, err := workspace.LockBeside(root, id)
			if err != nil {
				continue
			}
			if _, err := removeStaging(ctx, staging, id, exists(id)); err != nil {
				s.opts.Logger.Warn("ledger staging removal failed", "snapshot_id", id, "error", err)
			}
			lock.Release()
		}
		_ = staging.Close()
	}
	if fd, err := unix.Openat(rootFd, manifestsDir, dirFlags, 0); err == nil {
		manifests := os.NewFile(uintptr(fd), filepath.Join(root.Name(), manifestsDir))
		var names []string
		_ = eachName(ctx, manifests, func(name string) {
			if id, ok := strings.CutSuffix(name, ".json"); ok && id != own && validID(id) && !exists(id) {
				names = append(names, name)
			}
		})
		for _, name := range names {
			id, ok := strings.CutSuffix(name, ".json")
			if !ok || id == own || !validID(id) || exists(id) || ctx.Err() != nil {
				continue
			}
			lock, err := workspace.LockBeside(root, id)
			if err != nil {
				continue
			}
			// Checked again under the lock: a preparation may have
			// installed the directory meanwhile.
			if !exists(id) {
				if _, err := snapshot.RemoveManifest(manifests, name); err != nil {
					s.opts.Logger.Warn("orphaned ledger manifest removal failed", "snapshot_id", id, "error", err)
				}
			}
			lock.Release()
		}
		_ = manifests.Close()
	}
}

// chunkState is what .staging/<id>.state records between chunks.
type chunkState struct {
	BaseCommit string `json:"base_commit"`
	Chunk      uint32 `json:"chunk"`
}

func readSmall(dir *os.File, name string, limit int64) ([]byte, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(data)) > limit {
		err = fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return data, err
}

func readChunkState(staging *os.File, id string) (*chunkState, error) {
	data, err := readSmall(staging, stateName(id), 4096)
	if err != nil {
		return nil, err
	}
	var st chunkState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// writeStagingFile replaces name inside staging through name+".tmp",
// synced, renamed and followed by a sync of staging.
func writeStagingFile(staging *os.File, name string, data []byte) error {
	dirFd, tmp := int(staging.Fd()), name+".tmp"
	if err := unix.Unlinkat(dirFd, tmp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	fd, err := unix.Openat(dirFd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, err = f.Write(data)
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		return err
	}
	if err := unix.Renameat(dirFd, tmp, dirFd, name); err != nil {
		return err
	}
	return staging.Sync()
}

func writeChunkState(staging *os.File, id string, st chunkState) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeStagingFile(staging, stateName(id), data)
}

// writeChunkPaths records the paths a chunk asked for, which the last
// chunk compares with what the file system holds.
func writeChunkPaths(staging *os.File, id string, chunk uint32, paths []string) error {
	data, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	return writeStagingFile(staging, pathsName(id, chunk), data)
}

// maxPathsFileBytes bounds one chunk's path list: 4096 paths of at most
// 4096 bytes, escaped to at most twice that, with separators.
const maxPathsFileBytes = maxChunkFiles * (2*snapshot.MaxPathBytes + 4)

func readChunkPaths(staging *os.File, id string, chunk uint32) ([]string, error) {
	data, err := readSmall(staging, pathsName(id, chunk), maxPathsFileBytes)
	if err != nil {
		return nil, err
	}
	var paths []string
	return paths, json.Unmarshal(data, &paths)
}

func textResult(text string, isError bool) *circusv1.ToolOutput {
	return &circusv1.ToolOutput{IsError: isError, Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: text}}}}
}
