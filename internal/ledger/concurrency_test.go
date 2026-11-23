package ledger

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/go-git/go-git/v5/plumbing"
	"golang.org/x/sys/unix"
)

func TestConcurrentCommitMainAcrossProcesses(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), wait.Timeout)
	defer cancel()
	atCommit := make(chan struct{})
	release := make(chan struct{})
	m, db := openLedger(t, nil, "project")
	d := Decision{Title: "Original"}
	opts := CommitOptions{Author: "import", Message: "Seed"}
	first := commit(t, m, "project", []Decision{d}, opts)
	m.now = func() time.Time {
		close(atCommit)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return baseTime.Add(time.Second)
	}
	parent := make(chan error, 1)
	go func() {
		d.Title = "Winner"
		_, err := m.CommitMain(ctx, "project", []Decision{d}, opts)
		parent <- err
	}()
	defer func() {
		close(release)
		check(t, <-parent)
		index, err := db.GetLedgerIndex(t.Context(), "project")
		check(t, err)
		if index.Head == first || len(index.Entries) != 1 || index.Entries[0].Title != "Winner" {
			t.Errorf("unexpected winning index: %+v", index)
		}
	}()
	select {
	case <-atCommit:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	path := filepath.Join(filepath.Dir(m.root), "circus.db")
	binary, err := os.Executable()
	check(t, err)
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestLedgerWriterProcess$")
	cmd.Env = append(os.Environ(), "CIRCUS_LEDGER_TEST_DB="+path, "GORACE=atexit_sleep_ms=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("competing process: %v\n%s", err, out)
	}
}

func TestLedgerWriterProcess(t *testing.T) {
	path := os.Getenv("CIRCUS_LEDGER_TEST_DB")
	if path == "" {
		t.Skip("helper for the cross-process test")
	}
	ctx := t.Context()
	st, err := sqlite.Open(ctx, path, sqlite.Options{})
	check(t, err)
	defer st.Close()
	m, err := New(path, st, nil, nil)
	check(t, err)
	head, err := m.CommitMain(ctx, "project", []Decision{{Title: "Competing writer"}},
		CommitOptions{Author: "import", Message: "Competing update"})
	if head != "" || !errors.Is(err, store.ErrBusy) {
		t.Fatalf("competing commit = %q, %v", head, err)
	}
}

// Hiding a historical Git object only during index installation verifies
// that the replacement stream reads prepared records, even on fast disks.
type hideObjectOnReplace struct {
	store.LedgerOps
	path string
}

func (s hideObjectOnReplace) ReplaceLedgerIndex(ctx context.Context, id, indexed, head string, retainPrefix bool, entries []store.LedgerEntry, versions iter.Seq2[store.LedgerVersion, error]) error {
	hidden := s.path + ".hidden"
	if err := os.Rename(s.path, hidden); err != nil {
		return err
	}
	err := s.LedgerOps.ReplaceLedgerIndex(ctx, id, indexed, head, retainPrefix, entries, versions)
	return errors.Join(err, os.Rename(hidden, s.path))
}

func TestConcurrentLedgerRebuildLeavesStoreAvailable(t *testing.T) {
	testLedgerReplayStoreLatency(t, 128, 24)
}

func TestLargeLedgerRebuildLeavesStoreAvailable(t *testing.T) {
	testLedgerReplayStoreLatency(t, store.MaxLedgerDecisions, 160)
}

func testLedgerReplayStoreLatency(t *testing.T, decisions, commits int) {
	t.Helper()
	ctx := t.Context()
	m, db := openLedger(t, nil, "project")
	ds := make([]Decision, decisions)
	for i := range ds {
		ds[i] = Decision{Number: uint32(i), Title: fmt.Sprintf("Entry %d", i)}
	}
	opts := CommitOptions{Author: "import", Message: "Seed"}
	old := commit(t, m, "project", ds, opts)
	ds[0].Body = []byte("alternate")
	tip := commit(t, m, "project", ds, opts)
	r, err := openRepository(m.repoPath("project"))
	check(t, err)
	defer r.storage.Close()
	first, err := r.commit(plumbing.NewHash(old))
	check(t, err)
	second, err := r.commit(plumbing.NewHash(tip))
	check(t, err)
	head := second.Hash
	for i := range commits {
		tree := first.TreeHash
		if i%2 == 1 {
			tree = second.TreeHash
		}
		head = gitCommit(t, r, head, tree)
	}
	publishFixture(t, r, second.Hash, head)
	m.st = hideObjectOnReplace{LedgerOps: db, path: filepath.Join(r.dir, "objects", old[:2], old[2:])}
	const maxHold = 2 * time.Second
	finish := sampleStoreLatency(t, db, maxHold)
	ds[0].Body = []byte("next")
	committed, err := m.CommitMain(ctx, "project", ds, opts)
	got := finish()
	check(t, err)
	t.Logf("%d decisions and %d commits: %d concurrent reads, max latency %s, max CPU time during a read %s", decisions, commits+2, got.count, got.max, got.busy)
	indexed, err := db.GetLedgerHead(ctx, "project")
	check(t, err)
	if indexed != committed {
		t.Fatalf("indexed head %q, want %q", indexed, committed)
	}
}

type latencySamples struct {
	count int
	max   time.Duration // longest call
	busy  time.Duration // most CPU time the process used during one call
	err   error
}

// sampleStoreLatency calls GetUser in a loop until the returned function
// is called, and fails the test if the process used more than limit of CPU
// time during any call. The pool has a single connection, so a call waits
// while another goroutine holds it in a transaction, and the CPU time the
// process uses meanwhile is that transaction's work: a transaction sized
// by the history uses the CPU time of the history. Wall time would also
// count the process being descheduled or stalled on I/O by other load on
// the machine, which says nothing about how long a transaction is.
func sampleStoreLatency(t *testing.T, db store.Store, limit time.Duration) func() latencySamples {
	t.Helper()
	stop := make(chan struct{})
	started := make(chan struct{})
	result := make(chan latencySamples, 1)
	go func() {
		var s latencySamples
		defer func() { result <- s }()
		close(started)
		for {
			select {
			case <-stop:
				return
			default:
			}
			call, cancel := context.WithTimeout(t.Context(), wait.Timeout)
			now, cpu := time.Now(), processCPU(t)
			_, s.err = db.GetUser(call, "owner")
			cancel()
			s.busy = max(s.busy, processCPU(t)-cpu)
			s.max = max(s.max, time.Since(now))
			s.count++
			if s.err != nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	<-started
	return func() latencySamples {
		t.Helper()
		close(stop)
		got := <-result
		if got.err != nil || got.busy > limit || got.count < 2 {
			t.Fatalf("concurrent GetUser samples: %+v", got)
		}
		return got
	}
}

// processCPU returns the user and system CPU time the process has used.
func processCPU(t *testing.T) time.Duration {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil {
		t.Error(err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// An index install of more than 10^5 version rows (2×10^4 under the race
// detector) must not hold the store
// for a time proportional to the history. Here the indexed head is an
// ancestor of main, but too many versions follow it to add them in one
// transaction, so the whole index is replaced.
func TestFullLedgerInstallLeavesStoreAvailable(t *testing.T) {
	ctx := t.Context()
	m, db := openLedger(t, nil, "project")
	ds := make([]Decision, 1024)
	for i := range ds {
		ds[i] = Decision{Number: uint32(i), Title: fmt.Sprintf("Entry %d", i), Body: []byte("a")}
	}
	opts := CommitOptions{Author: "import", Message: "Seed"}
	trees := make([]plumbing.Hash, 2)
	r, err := openRepository(m.repoPath("project"))
	if err == nil && r != nil {
		t.Fatal("repository exists before the first commit")
	}
	var head plumbing.Hash
	for i := range trees {
		for j := range ds {
			ds[j].Body = []byte{byte('a' + i)}
		}
		head = plumbing.NewHash(commit(t, m, "project", ds, opts))
		if r == nil {
			r, err = openRepository(m.repoPath("project"))
			check(t, err)
			defer r.storage.Close()
		}
		c, err := r.commit(head)
		check(t, err)
		trees[i] = c.TreeHash
	}
	// Every further commit changes every decision. The race detector slows
	// SQLite many times over, so it gets about a fifth of the history.
	commits := 100
	if raceEnabled {
		commits = 20
	}
	published := head
	for i := 2; i < commits; i++ {
		head = gitCommit(t, r, head, trees[i%2])
	}
	publishFixture(t, r, published, head)

	const maxHold = 500 * time.Millisecond
	finish := sampleStoreLatency(t, db, maxHold)
	start := time.Now()
	err = m.Reconcile(ctx)
	got := finish()
	check(t, err)
	t.Logf("installed %d versions in %s: %d concurrent reads, max latency %s, max CPU time during a read %s", commits*len(ds), time.Since(start), got.count, got.max, got.busy)
	index, err := db.GetLedgerIndex(ctx, "project")
	check(t, err)
	if index.Head != head.String() || len(index.Entries) != len(ds) {
		t.Fatalf("installed index head %s with %d entries", index.Head, len(index.Entries))
	}
	for _, number := range []uint32{0, uint32(len(ds) - 1)} {
		var rows int
		for after, more := "", true; more; {
			page, next, err := db.ListLedgerHistory(ctx, "project", number, after, store.MaxLedgerHistoryPage)
			check(t, err)
			rows += len(page)
			after, more = page[len(page)-1].Commit, next
		}
		if rows != commits {
			t.Fatalf("D%d has %d versions, want %d", number, rows, commits)
		}
	}
}
