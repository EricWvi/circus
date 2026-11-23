package ledger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/objfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/sys/unix"
)

func TestCommitMainValidatesRenderedFiles(t *testing.T) {
	m, db := openLedger(t, nil, "project")
	opts := CommitOptions{Author: "import", Message: "Seed"}
	d := Decision{Title: "<<", Body: []byte("a\r\n正文")}
	first := commit(t, m, "project", []Decision{d}, opts)
	got, err := m.GetDecision(t.Context(), "project", d.Number, first)
	check(t, err)
	if got.Title != d.Title || !bytes.Equal(got.Body, d.Body) {
		t.Fatalf("round trip = %+v", got)
	}
	header, err := Render(Decision{Title: d.Title})
	check(t, err)
	oversized := d
	oversized.Body = bytes.Repeat([]byte("b"), MaxDecisionFileBytes-len(header)+1)
	for _, tc := range []struct {
		name      string
		decisions []Decision
		path      string
		message   string
	}{
		{"file size including header", []Decision{oversized}, decisionPath(d), "256 KiB"},
		{"file is also a directory", []Decision{d, {Number: 1, Dir: FileName(d.Number, d.Title), Title: "nested"}},
			FileName(d.Number, d.Title) + "/D1-nested.md", "conflicts with file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head, err := m.CommitMain(t.Context(), "project", tc.decisions, opts)
			var invalid *ValidationError
			if head != "" || !errors.As(err, &invalid) {
				t.Fatalf("invalid commit = %q, %v", head, err)
			}
			expectProblem(t, invalid.Problems, tc.path, tc.message)
		})
	}
	for _, r := range []rune{'\r', '\u0085', '\u2028', '\u2029', '\n', '\t', '\x00', '\x7f', '\u009f'} {
		d := Decision{Title: "left" + string(r) + "right"}
		_, err := m.CommitMain(t.Context(), "project", []Decision{d}, opts)
		var invalid *ValidationError
		if !errors.As(err, &invalid) {
			t.Fatalf("accepted title with %U: %v", r, err)
		}
		expectProblem(t, invalid.Problems, decisionPath(d), "single UTF-8 line")
	}
	for _, dir := range []string{"a b", "a:b", "a\\b", "a\u200b", "emoji😀", "a\x00"} {
		bad := Decision{Title: "name", Dir: dir}
		expectProblem(t, ValidateTree(decisionFiles(t, bad)), decisionPath(bad), "use only letters")
	}
	good := Decision{Title: "name", Dir: "文档/в_1.e\u0301"}
	if problems := ValidateTree(decisionFiles(t, good)); len(problems) != 0 {
		t.Fatalf("valid directory: %+v", problems)
	}
	head, err := db.GetLedgerHead(t.Context(), "project")
	check(t, err)
	if head != first {
		t.Fatalf("invalid changes moved the head to %s", head)
	}
}

func TestCommitMainRepairsStaleIndex(t *testing.T) {
	for _, staging := range []bool{false, true} {
		t.Run(fmt.Sprintf("staging=%t", staging), func(t *testing.T) {
			ctx := t.Context()
			m, db := openLedger(t, nil, "project")
			d := Decision{Number: 4, Title: "first"}
			opts := CommitOptions{Author: "import", Message: "Seed"}
			first := commit(t, m, "project", []Decision{d}, opts)
			var middle string
			if staging {
				check(t, m.Remove(ctx, "project"))
				d.Number = 5
			} else {
				m.st = failAdvance{LedgerOps: db, err: store.ErrBusy}
				d.Title = "middle"
				var err error
				middle, err = m.CommitMain(ctx, "project", []Decision{d}, opts)
				if middle == "" || !errors.Is(err, store.ErrBusy) {
					t.Fatalf("failure after moving main = %q, %v", middle, err)
				}
				m.st = db
			}
			d.Title = "last"
			last := commit(t, m, "project", []Decision{d}, opts)
			index, err := db.GetLedgerIndex(ctx, "project")
			check(t, err)
			if len(index.Entries) != 1 || index.Head != last || index.Entries[0].Title != d.Title ||
				index.Entries[0].Commit != last || index.NextNumber != d.Number+1 {
				t.Fatalf("repaired index = %+v", index)
			}
			page, _, err := m.ListDecisionHistory(ctx, "project", d.Number, "", 10)
			check(t, err)
			if staging {
				if len(page) != 1 || !page[0].Added {
					t.Fatalf("new repository history = %+v", page)
				}
			} else if len(page) != 3 || page[0].Commit != last || page[1].Commit != middle || page[2].Commit != first {
				t.Fatalf("replayed history = %+v", page)
			}
		})
	}
}

type countReplacements struct {
	store.LedgerOps
	calls int
}

func (s *countReplacements) ReplaceLedgerIndex(ctx context.Context, id, indexed, head string, retainPrefix bool, entries []store.LedgerEntry, versions iter.Seq2[store.LedgerVersion, error]) error {
	s.calls++
	return s.LedgerOps.ReplaceLedgerIndex(ctx, id, indexed, head, retainPrefix, entries, versions)
}

func TestReconcileSkipsCurrentIndexAndClearsStaleLocks(t *testing.T) {
	m, db := openLedger(t, nil, "current")
	ctx := t.Context()
	commit(t, m, "current", []Decision{{Title: "Current"}}, CommitOptions{Author: "import", Message: "Seed"})
	dir := m.repoPath("current")
	r, err := openRepository(dir)
	check(t, err)
	defer r.storage.Close()
	temp := filepath.Join(dir, refPrefix+"abandoned")
	object := filepath.Join(dir, "objects", objectTempPrefix+"abandoned")
	for _, file := range []string{temp, object, r.mainLockPath()} {
		check(t, os.WriteFile(file, []byte("stale"), 0o600))
	}
	before, err := db.GetLedgerIndex(ctx, "current")
	check(t, err)
	guard := &countReplacements{LedgerOps: db}
	m.st = guard
	check(t, m.Reconcile(ctx))
	after, err := db.GetLedgerIndex(ctx, "current")
	check(t, err)
	if guard.calls != 0 || after.Head != before.Head || !slices.Equal(before.Entries, after.Entries) {
		t.Fatalf("current index rewritten: %d calls, before %+v, after %+v", guard.calls, before, after)
	}
	for _, file := range []string{temp, object, r.mainLockPath()} {
		if _, err := os.Lstat(file); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("startup left %q: %v", file, err)
		}
	}
}

func gitCommit(t *testing.T, r *repository, parent, tree plumbing.Hash) plumbing.Hash {
	t.Helper()
	sig := object.Signature{Name: "import", Email: "import@circus.invalid", When: baseTime}
	c := &object.Commit{TreeHash: tree, ParentHashes: []plumbing.Hash{parent}, Author: sig, Committer: sig, Message: "Fixture\n"}
	obj := r.storage.NewEncodedObject()
	check(t, c.Encode(obj))
	h, err := r.putObject(obj)
	check(t, err)
	return h
}

func writeRawSnapshot(t *testing.T, r *repository, parent plumbing.Hash, name string, mode filemode.FileMode, file, table []byte) plumbing.Hash {
	t.Helper()
	b, err := r.putBlob(file)
	check(t, err)
	index, err := r.putBlob(table)
	check(t, err)
	tree := &object.Tree{Entries: []object.TreeEntry{
		{Name: name, Mode: mode, Hash: b},
		{Name: indexName, Mode: filemode.Regular, Hash: index},
	}}
	obj := r.storage.NewEncodedObject()
	check(t, tree.Encode(obj))
	hash, err := r.putObject(obj)
	check(t, err)
	head := gitCommit(t, r, parent, hash)
	publishFixture(t, r, parent, head)
	return head
}

func publishFixture(t *testing.T, r *repository, parent, head plumbing.Hash) {
	t.Helper()
	unlock, err := r.lockMain()
	check(t, err)
	defer unlock()
	moved, err := r.setMain(parent, head)
	check(t, err)
	if !moved {
		t.Fatal("fixture main did not move")
	}
}

func TestReadRejectsBadGitTrees(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mode    filemode.FileMode
		file    string
		problem string
	}{
		{"executable", filemode.Executable, "D0-first.md", "invalid file mode"},
		{"symlink", filemode.Symlink, "D0-first.md", "invalid file mode"},
		{"wrong file name", filemode.Regular, "D0-wrong.md", "does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := openLedger(t, nil, "project")
			ctx := t.Context()
			d := Decision{Title: "first"}
			old := commit(t, m, "project", []Decision{d}, CommitOptions{Author: "import", Message: "Seed"})
			r, err := openRepository(m.repoPath("project"))
			check(t, err)
			defer r.storage.Close()
			data, err := Render(d)
			check(t, err)
			head := writeRawSnapshot(t, r, plumbing.NewHash(old), tc.file, tc.mode, data, RenderIndex([]Decision{d}))
			if _, err := m.GetDecision(ctx, "project", 0, head.String()); err == nil {
				t.Fatal("invalid file accepted")
			} else if tc.mode == filemode.Regular {
				var invalid *ValidationError
				if !errors.As(err, &invalid) {
					t.Fatal(err)
				}
				expectProblem(t, invalid.Problems, tc.file, tc.problem)
			} else if !strings.Contains(err.Error(), tc.problem) {
				t.Fatal(err)
			}
		})
	}
}

func TestReconcileIsolatesUnreadableRepositories(t *testing.T) {
	for _, kind := range []string{"bad title", "truncated", "non-bare", "regular file", "bad index", "removed"} {
		t.Run(kind, func(t *testing.T) {
			m, db := openLedger(t, nil, "bad", "good")
			ctx := t.Context()
			d := Decision{Title: "first"}
			opts := CommitOptions{Author: "import", Message: "Seed"}
			old := commit(t, m, "bad", []Decision{d, {Number: 1, Title: "retained"}}, opts)
			commit(t, m, "good", []Decision{d}, opts)
			m.st = failAdvance{LedgerOps: db, err: store.ErrBusy}
			good, err := m.CommitMain(ctx, "good", []Decision{{Title: "new"}}, opts)
			if good == "" || !errors.Is(err, store.ErrBusy) {
				t.Fatalf("expected stale index: %q, %v", good, err)
			}
			m.st = db
			r, err := openRepository(m.repoPath("bad"))
			check(t, err)
			defer r.storage.Close()
			data, err := Render(d)
			check(t, err)
			parent := plumbing.NewHash(old)
			switch kind {
			case "bad title":
				writeRawSnapshot(t, r, parent, "D0-.md", filemode.Regular,
					[]byte("---\nnumber: D0\ntitle: <<\n---\n\n"), RenderIndex([]Decision{d}))
			case "truncated":
				head := writeRawSnapshot(t, r, parent, decisionPath(d), filemode.Regular, data, RenderIndex([]Decision{d})).String()
				file := filepath.Join(r.dir, "objects", head[:2], head[2:])
				check(t, os.Chmod(file, 0o600))
				check(t, os.Truncate(file, 0))
			case "non-bare":
				cfg, err := r.Config()
				check(t, err)
				cfg.Core.IsBare = false
				check(t, r.SetConfig(cfg))
			case "regular file":
				check(t, m.Remove(ctx, "bad"))
				check(t, os.WriteFile(r.dir, []byte("not a repository"), 0o600))
			case "bad index":
				writeRawSnapshot(t, r, parent, decisionPath(d), filemode.Regular, data, []byte("wrong table"))
			case "removed":
				writeRawSnapshot(t, r, parent, decisionPath(d), filemode.Regular, data, RenderIndex([]Decision{d}))
			}
			check(t, os.Mkdir(filepath.Join(m.root, "invalid\\name.git"), 0o700))
			var logs bytes.Buffer
			m.logger = slog.New(slog.NewTextHandler(&logs, nil))
			check(t, m.Reconcile(ctx))
			broken := m.CheckProject(ctx, "bad")
			if broken == nil {
				t.Fatal("bad repository not reported")
			}
			if kind == "bad index" || kind == "removed" || kind == "bad title" {
				var invalid *ValidationError
				if !errors.As(broken, &invalid) {
					t.Fatal(broken)
				}
				switch kind {
				case "bad index":
					expectProblem(t, invalid.Problems, indexName, "generated table")
				case "removed":
					expectProblem(t, invalid.Problems, "D1-retained.md", "cannot be removed")
				case "bad title":
					expectProblem(t, invalid.Problems, "D0-.md", "YAML strings")
				}
			}
			if !strings.Contains(logs.String(), "project_id=bad") || !strings.Contains(logs.String(), "invalid project id") {
				t.Fatalf("missing diagnostics: %s", logs.String())
			}
			badIndex, err := db.GetLedgerIndex(ctx, "bad")
			check(t, err)
			goodHead, err := db.GetLedgerHead(ctx, "good")
			check(t, err)
			if badIndex.Head != old || len(badIndex.Entries) != 2 || goodHead != good {
				t.Fatalf("bad index %+v, good head %s want %s", badIndex, goodHead, good)
			}
		})
	}
}

func TestMainCompareAndSwap(t *testing.T) {
	m, db := openLedger(t, nil, "project")
	ctx := t.Context()
	first := commit(t, m, "project", []Decision{{Title: "Old"}}, CommitOptions{Author: "import", Message: "Seed"})
	r, err := openRepository(m.repoPath("project"))
	check(t, err)
	defer r.storage.Close()
	old, err := r.commit(plumbing.NewHash(first))
	check(t, err)
	winner := gitCommit(t, r, old.Hash, old.TreeHash)
	loser := gitCommit(t, r, winner, old.TreeHash)
	publishFixture(t, r, old.Hash, winner)

	unlock, err := r.lockMain()
	check(t, err)
	defer unlock()
	moved, err := r.setMain(old.Hash, loser)
	if moved || !errors.Is(err, store.ErrBusy) {
		t.Fatalf("stale reference update = %t, %v", moved, err)
	}
	head, err := r.head()
	check(t, err)
	indexed, err := db.GetLedgerHead(ctx, "project")
	check(t, err)
	if head != winner || indexed != first {
		t.Fatalf("CAS moved Git or index: Git %s, index %s", head, indexed)
	}
}

// A crash after an object got its name but before it reached the disk can
// leave a broken file under that name. A commit that needs the object again
// must not trust the file, but replace it.
func TestCommitMainReplacesBrokenObjects(t *testing.T) {
	for _, kind := range []string{"empty", "truncated", "other content", "same size, other content"} {
		t.Run(kind, func(t *testing.T) {
			m, _ := openLedger(t, nil, "project")
			ctx := t.Context()
			opts := CommitOptions{Author: "import", Message: "Seed"}
			commit(t, m, "project", []Decision{{Number: 1, Title: "x"}}, opts)
			next := Decision{Number: 1, Title: "x", Body: []byte("retry me")}
			data, err := Render(next)
			check(t, err)
			r, err := openRepository(m.repoPath("project"))
			check(t, err)
			defer r.storage.Close()
			blob := plumbing.ComputeHash(plumbing.BlobObject, data)
			file := r.loosePath(blob)
			switch kind {
			case "empty":
				check(t, os.MkdirAll(filepath.Dir(file), 0o755))
				check(t, os.WriteFile(file, nil, 0o444))
			case "truncated":
				check(t, r.writeObject(plumbing.BlobObject, blob, int64(len(data)), [][]byte{data}))
				info, err := os.Stat(file)
				check(t, err)
				check(t, os.Chmod(file, 0o600))
				check(t, os.Truncate(file, info.Size()-1))
			case "other content":
				// A complete object, but not the one its name promises.
				other := []byte("other content")
				check(t, r.writeObject(plumbing.BlobObject, plumbing.ComputeHash(plumbing.BlobObject, other), int64(len(other)), [][]byte{other}))
				check(t, os.MkdirAll(filepath.Dir(file), 0o755))
				id := plumbing.ComputeHash(plumbing.BlobObject, other).String()
				check(t, os.Rename(filepath.Join(r.dir, "objects", id[:2], id[2:]), file))
			case "same size, other content":
				// A valid stream of the right type and size, but whose
				// content has another hash.
				other := bytes.Clone(data)
				other[len(other)-1] ^= 1
				check(t, os.MkdirAll(filepath.Dir(file), 0o755))
				f, err := os.Create(file)
				check(t, err)
				w := objfile.NewWriter(f)
				check(t, w.WriteHeader(plumbing.BlobObject, int64(len(other))))
				_, err = w.Write(other)
				check(t, err)
				check(t, errors.Join(w.Close(), f.Close()))
			}
			head := commit(t, m, "project", []Decision{next}, opts)
			got, err := m.GetDecision(ctx, "project", 1, head)
			check(t, err)
			if !bytes.Equal(got.Body, next.Body) {
				t.Fatalf("body at the new head = %q", got.Body)
			}
			check(t, r.verifyObject(plumbing.BlobObject, blob, int64(len(data))))
			commit(t, m, "project", []Decision{{Number: 1, Title: "x", Body: []byte("fix")}}, opts)
		})
	}
}

// A broken commit or tree object is replaced in the same way.
func TestCommitMainReplacesBrokenTreeAndCommit(t *testing.T) {
	m, db := openLedger(t, nil, "project")
	ctx := t.Context()
	opts := CommitOptions{Author: "import", Message: "Seed"}
	at := baseTime
	m.now = func() time.Time { return at }
	d := Decision{Title: "x"}
	first := commit(t, m, "project", []Decision{d}, opts)
	d.Body = []byte("second")
	m.st = failAdvance{LedgerOps: db, err: store.ErrBusy}
	second, err := m.CommitMain(ctx, "project", []Decision{d}, opts)
	m.st = db
	if second == "" || !errors.Is(err, store.ErrBusy) {
		t.Fatalf("second commit = %q, %v", second, err)
	}
	r, err := openRepository(m.repoPath("project"))
	check(t, err)
	defer r.storage.Close()
	tree := mustTree(t, r, second)
	// Roll main back as if the second commit had never been published, and
	// leave its tree and commit objects empty.
	publishFixture(t, r, plumbing.NewHash(second), plumbing.NewHash(first))
	for _, hash := range []plumbing.Hash{tree, plumbing.NewHash(second)} {
		file := r.loosePath(hash)
		check(t, os.Chmod(file, 0o600))
		check(t, os.Truncate(file, 0))
	}
	if again := commit(t, m, "project", []Decision{d}, opts); again != second {
		t.Fatalf("retried commit %s, want %s", again, second)
	}
	if _, err := m.GetDecision(ctx, "project", 0, second); err != nil {
		t.Fatal(err)
	}
}

type onProjectName struct {
	store.LedgerOps
	once func()
}

func (s *onProjectName) LedgerProjectName(ctx context.Context, id string) (string, bool, error) {
	if s.once != nil {
		s.once()
		s.once = nil
	}
	return s.LedgerOps.LedgerProjectName(ctx, id)
}

// When another process uses the ledger directory at startup, a main.lock
// may be live, so it is kept and named in the log; the startup check holds
// the directory shared, so a third process cannot sweep the lock it takes.
func TestReconcileWithoutExclusiveLock(t *testing.T) {
	m, db := openLedger(t, nil, "project")
	ctx := t.Context()
	opts := CommitOptions{Author: "import", Message: "Seed"}
	commit(t, m, "project", []Decision{{Title: "x"}}, opts)
	r, err := openRepository(m.repoPath("project"))
	check(t, err)
	lock := r.mainLockPath()
	check(t, r.storage.Close())
	check(t, os.WriteFile(lock, nil, 0o600))
	_, err = m.CommitMain(ctx, "project", []Decision{{Title: "y"}}, opts)
	if !errors.Is(err, store.ErrBusy) {
		t.Fatalf("commit with main.lock held = %v", err)
	}

	other, err := New(filepath.Join(filepath.Dir(m.root), "circus.db"), db, nil, nil)
	check(t, err)
	unlockOther, err := other.lockRoot(unix.LOCK_SH)
	check(t, err)
	var logs bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logs, nil))
	check(t, m.Reconcile(ctx))
	if !strings.Contains(logs.String(), "main.lock kept") || !strings.Contains(logs.String(), "project=project") {
		t.Fatalf("missing diagnostics: %s", logs.String())
	}
	if _, err := os.Lstat(lock); err != nil {
		t.Fatalf("a possibly live lock was removed: %v", err)
	}
	check(t, os.Remove(lock))
	var sweep error
	m.st = &onProjectName{LedgerOps: db, once: func() {
		unlockOther()
		_, sweep = other.lockRoot(unix.LOCK_EX)
	}}
	check(t, m.Reconcile(ctx))
	if !errors.Is(sweep, store.ErrBusy) {
		t.Fatalf("exclusive lock during a shared startup check = %v", sweep)
	}
	m.st = db
	commit(t, m, "project", []Decision{{Title: "y"}}, opts)
}

// A project that could not be read is checked again on a read once its
// retry delay has passed, so a transient error does not need a restart.
func TestBrokenProjectIsRetried(t *testing.T) {
	m, _ := openLedger(t, nil, "project")
	ctx := t.Context()
	head := commit(t, m, "project", []Decision{{Title: "x"}}, CommitOptions{Author: "import", Message: "Seed"})
	file := filepath.Join(m.repoPath("project"), "objects", head[:2], head[2:])
	check(t, os.Rename(file, file+".away"))
	check(t, m.Reconcile(ctx))
	expire := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.broken["project"].retry = time.Time{}
	}
	// Within the delay, a read returns the error without another check.
	// After it, each failed check doubles the delay.
	for _, delay := range []time.Duration{minProjectRetry, 2 * minProjectRetry, 4 * minProjectRetry} {
		if _, err := m.GetDecision(ctx, "project", 0, ""); err == nil || errors.Is(err, store.ErrNotFound) {
			t.Fatalf("read of a broken project = %v", err)
		}
		m.mu.Lock()
		got := m.broken["project"].delay
		m.mu.Unlock()
		if got != delay {
			t.Fatalf("retry delay %s, want %s", got, delay)
		}
		expire()
	}
	// Once the delay has passed, the next read checks again and succeeds.
	check(t, os.Rename(file+".away", file))
	check(t, m.CheckProject(ctx, "project"))
	if _, err := m.GetDecision(ctx, "project", 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := m.CheckProject(ctx, "project"); err != nil {
		t.Fatal(err)
	}
}

// blockProjectName stands for a check that cannot finish: it waits until
// its ctx ends.
type blockProjectName struct {
	store.LedgerOps
}

func (s blockProjectName) LedgerProjectName(ctx context.Context, id string) (string, bool, error) {
	<-ctx.Done()
	return "", false, ctx.Err()
}

// The retry of a broken project does not depend on the reader that starts
// it: it outlives a reader that gives up, and running out of its own
// budget doubles the delay like any other failure.
func TestBrokenProjectRetryIsDetached(t *testing.T) {
	m, db := openLedger(t, nil, "project")
	ctx := t.Context()
	head := commit(t, m, "project", []Decision{{Title: "x"}}, CommitOptions{Author: "import", Message: "Seed"})
	file := filepath.Join(m.repoPath("project"), "objects", head[:2], head[2:])
	check(t, os.Rename(file, file+".away"))
	check(t, m.Reconcile(ctx))
	expire := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.broken["project"].retry = time.Time{}
	}
	delay := func() time.Duration {
		m.mu.Lock()
		defer m.mu.Unlock()
		if fault := m.broken["project"]; fault != nil {
			return fault.delay
		}
		return 0
	}

	m.retryBudget = 50 * time.Millisecond
	m.st = blockProjectName{LedgerOps: db}
	expire()
	if err := m.CheckProject(ctx, "project"); err == nil {
		t.Fatal("a check that ran out of budget cleared the error")
	}
	if got := delay(); got != 2*minProjectRetry {
		t.Fatalf("delay after a timed-out check = %s", got)
	}

	// A reader that gives up while the check is still running.
	check(t, os.Rename(file+".away", file))
	release := make(chan struct{})
	m.retryBudget = projectRetryBudget
	m.st = gateProjectName{LedgerOps: db, release: release}
	expire()
	reader, cancel := context.WithCancel(ctx)
	cancel()
	if err := m.CheckProject(reader, "project"); err == nil {
		t.Fatal("a reader that gave up saw the check succeed")
	}
	close(release)
	wait.For(t, func() bool { return delay() == 0 })
	m.st = db
	if _, err := m.GetDecision(ctx, "project", 0, ""); err != nil {
		t.Fatal(err)
	}
}

// gateProjectName holds a check until release is closed.
type gateProjectName struct {
	store.LedgerOps
	release chan struct{}
}

func (s gateProjectName) LedgerProjectName(ctx context.Context, id string) (string, bool, error) {
	<-s.release
	return s.LedgerOps.LedgerProjectName(ctx, id)
}

// A damaged blob that main already refers to is repaired by the next
// commit that supplies its bytes: an empty file at once, other damage
// once a read of it has failed.
func TestCommitMainRepairsParentBlobs(t *testing.T) {
	for _, kind := range []string{"empty", "truncated"} {
		t.Run(kind, func(t *testing.T) {
			m, db := openLedger(t, nil, "project")
			ctx := t.Context()
			opts := CommitOptions{Author: "import", Message: "Seed"}
			d1 := Decision{Number: 1, Title: "x", Body: []byte("one")}
			commit(t, m, "project", []Decision{d1}, opts)
			data, err := Render(d1)
			check(t, err)
			blob := plumbing.ComputeHash(plumbing.BlobObject, data)
			r, err := openRepository(m.repoPath("project"))
			check(t, err)
			defer r.storage.Close()
			file := r.loosePath(blob)
			info, err := os.Stat(file)
			check(t, err)
			check(t, os.Chmod(file, 0o600))
			if kind == "empty" {
				check(t, os.Truncate(file, 0))
			} else {
				check(t, os.Truncate(file, info.Size()-1))
				if _, err := m.GetDecision(ctx, "project", 1, ""); err == nil {
					t.Fatal("read a truncated blob")
				}
			}
			head := commit(t, m, "project", []Decision{d1, {Number: 2, Title: "y"}}, opts)
			check(t, r.verifyObject(plumbing.BlobObject, blob, int64(len(data))))
			got, err := m.GetDecision(ctx, "project", 1, head)
			check(t, err)
			if !bytes.Equal(got.Body, d1.Body) {
				t.Fatalf("repaired body = %q", got.Body)
			}
			// A rebuild reads every blob again.
			check(t, db.ReplaceLedgerIndex(ctx, "project", head, "", false, nil, nil))
			check(t, m.Reconcile(ctx))
			check(t, m.CheckProject(ctx, "project"))
		})
	}
}

// While a check of a broken project cannot finish, readers neither start
// more checks of it nor more than maxProjectChecks across projects, and
// Close stops the running ones at once.
func TestBrokenProjectChecksAreBounded(t *testing.T) {
	projects := []string{"a", "b", "c"}
	m, _ := openLedger(t, nil, projects...)
	ctx := t.Context()
	var releases []func()
	for _, id := range projects {
		commit(t, m, id, []Decision{{Title: "x"}}, CommitOptions{Author: "import", Message: "Seed"})
		m.setProjectError(id, errors.New("transient"))
		// Holding the project's queue stands for a long rebuild.
		release, err := m.acquire(ctx, id)
		check(t, err)
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	// Count only the goroutines the manager starts: the bound is on checks,
	// and runtime.NumGoroutine also counts goroutines that end on their
	// own, such as the commits' SQLite query watchers and the timer that
	// cancels a reader's context, which may not have exited yet.
	base := managerGoroutines()
	read := func(id string) {
		t.Helper()
		m.mu.Lock()
		m.broken[id].retry = time.Time{}
		m.mu.Unlock()
		reader, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
		defer cancel()
		if err := m.CheckProject(reader, id); err == nil {
			t.Fatalf("%s: read of a broken project succeeded", id)
		}
	}
	for range 6 {
		read("a")
	}
	if running := len(m.checkSlots); running != 1 {
		t.Fatalf("%d checks of one project running", running)
	}
	for range 6 {
		for _, id := range projects {
			read(id)
		}
	}
	if running := len(m.checkSlots); running != maxProjectChecks {
		t.Fatalf("%d checks running, want %d", running, maxProjectChecks)
	}
	if extra := managerGoroutines() - base; extra > maxProjectChecks {
		t.Fatalf("%d manager goroutines more than before the reads", extra)
	}
	closing, cancel := context.WithTimeout(ctx, wait.Timeout)
	defer cancel()
	start := time.Now()
	check(t, m.Close(closing))
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Close took %s", took)
	}
	// After Close, reads report the error without starting checks: a
	// check would fail on the closed context and double the delay.
	m.mu.Lock()
	m.broken["c"].retry = time.Time{}
	delay := m.broken["c"].delay
	m.mu.Unlock()
	if err := m.CheckProject(ctx, "c"); err == nil || len(m.checkSlots) != 0 {
		t.Fatalf("read after Close = %v with %d checks", err, len(m.checkSlots))
	}
	m.mu.Lock()
	after := m.broken["c"].delay
	m.mu.Unlock()
	if after != delay {
		t.Fatalf("a read after Close ran a check: delay %s, then %s", delay, after)
	}
}

// managerGoroutines counts the goroutines that a Manager method started.
func managerGoroutines() int {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	creator := "\ncreated by " + reflect.TypeFor[Manager]().PkgPath() + ".(*Manager)."
	return strings.Count(string(buf), creator)
}

// An empty index for a repository with a main line, as left by a refused
// project delete, is rebuilt on a read, without a commit or a restart.
// Readers are not refused while it is rebuilt.
func TestClearedIndexIsRebuiltOnRead(t *testing.T) {
	m, db := openLedger(t, nil, "project")
	ctx := t.Context()
	head := commit(t, m, "project", []Decision{{Title: "x"}}, CommitOptions{Author: "import", Message: "Seed"})
	check(t, db.ReplaceLedgerIndex(ctx, "project", head, "", false, nil, nil))

	release, err := m.acquire(ctx, "project")
	check(t, err)
	reader, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	err = m.CheckProject(reader, "project")
	cancel()
	release()
	check(t, err)
	wait.For(t, func() bool {
		m.mu.Lock()
		left := len(m.broken)
		m.mu.Unlock()
		return left == 0
	})
	if indexed, err := db.GetLedgerHead(ctx, "project"); err != nil || indexed != head {
		t.Fatalf("indexed head %q, %v; want %s", indexed, err, head)
	}
	if _, err := m.GetDecision(ctx, "project", 0, ""); err != nil {
		t.Fatal(err)
	}
}

type countReplace struct {
	store.LedgerOps
	calls atomic.Int64
}

func (s *countReplace) ReplaceLedgerIndex(ctx context.Context, id, indexed, head string, retainPrefix bool, entries []store.LedgerEntry, versions iter.Seq2[store.LedgerVersion, error]) error {
	s.calls.Add(1)
	return s.LedgerOps.ReplaceLedgerIndex(ctx, id, indexed, head, retainPrefix, entries, versions)
}

// Reads during a project deletion serve the index the deletion is
// clearing and start no rebuild of it. A check that finds the project
// deleted ends quietly, without replaying the history.
func TestReadsDuringProjectDeletion(t *testing.T) {
	m, db := openLedger(t, nil, "project")
	ctx := t.Context()
	var logs bytes.Buffer
	var logsMu sync.Mutex
	m.logger = slog.New(slog.NewTextHandler(writerFunc(func(p []byte) (int, error) {
		logsMu.Lock()
		defer logsMu.Unlock()
		return logs.Write(p)
	}), &slog.HandlerOptions{Level: slog.LevelDebug}))
	head := commit(t, m, "project", []Decision{{Title: "x"}}, CommitOptions{Author: "import", Message: "Seed"})
	counter := &countReplace{LedgerOps: db}
	m.st = counter

	done := m.Deleting("project")
	// The deletion hides the index before it deletes the rows.
	check(t, db.ReplaceLedgerIndex(ctx, "project", head, "", false, nil, nil))
	check(t, m.CheckProject(ctx, "project"))
	if _, err := m.GetDecision(ctx, "project", 0, ""); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	recorded := len(m.broken)
	m.mu.Unlock()
	if recorded != 0 || counter.calls.Load() != 0 {
		t.Fatalf("reads during the deletion recorded %d projects and rebuilt %d times", recorded, counter.calls.Load())
	}
	done()

	// A read that noticed the empty index before the deletion started.
	m.setProjectError("project", errors.New("index is empty"))
	m.mu.Lock()
	m.broken["project"].stale, m.broken["project"].retry = true, time.Time{}
	m.mu.Unlock()
	// A deletion that starts afterwards stops it from being checked.
	done = m.Deleting("project")
	check(t, m.CheckProject(ctx, "project"))
	m.mu.Lock()
	recorded = len(m.broken)
	m.mu.Unlock()
	if recorded != 1 || counter.calls.Load() != 0 {
		t.Fatalf("read during the deletion checked a recorded project: %d recorded, %d rebuilds", recorded, counter.calls.Load())
	}
	check(t, db.DeleteProject(ctx, "owner", "project", 2))
	done()
	check(t, m.CheckProject(ctx, "project"))
	m.mu.Lock()
	recorded = len(m.broken)
	m.mu.Unlock()
	logsMu.Lock()
	text := logs.String()
	logsMu.Unlock()
	if recorded != 0 || counter.calls.Load() != 0 || strings.Contains(text, "level=ERROR") || !strings.Contains(text, "project deleted") {
		t.Fatalf("check of a deleted project: %d recorded, %d rebuilds, logs:\n%s", recorded, counter.calls.Load(), text)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
