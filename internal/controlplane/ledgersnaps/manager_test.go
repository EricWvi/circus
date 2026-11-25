package ledgersnaps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/ledgerslots"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/controlplane/workspaces"
	"github.com/EricWvi/circus/internal/daemon/ledgersnap"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

// device runs the daemon's real snapshot tools in-process, online or not.
type device struct {
	root    string
	service *ledgersnap.Service
	tools   map[string]interface {
		Run(context.Context, *pb.DeviceCall) (*pb.DeviceResult, error)
	}
	online atomic.Bool
	// unsupported: the daemon does not run ledger calls (a fault
	// TOOL_UNAVAILABLE).
	unsupported atomic.Bool
	// gate, when set, holds every call until it is closed.
	gate    atomic.Pointer[chan struct{}]
	running atomic.Int32
	peak    atomic.Int32
	// beforeRebase, when set, runs before the n-th rebase call (from 1);
	// loseRebase loses the n-th rebase's answer, after it ran, when it
	// returns true.
	beforeRebase func(n int)
	loseRebase   func(n int) bool
	// rebaseResult, when set, changes each rebase's result.
	rebaseResult func(*pb.RebaseLedgerResult)
	// reject refuses every call as for an unknown device.
	reject   atomic.Bool
	rebases  int
	mu       sync.Mutex
	requests []*pb.DeviceCall
}

func newDevice(t *testing.T) *device {
	d := &device{root: t.TempDir()}
	d.service = ledgersnap.New(ledgersnap.Options{Root: d.root})
	d.tools = map[string]interface {
		Run(context.Context, *pb.DeviceCall) (*pb.DeviceResult, error)
	}{}
	for _, tool := range d.service.Tools() {
		d.tools[tool.Name()] = tool
	}
	d.online.Store(true)
	return d
}

type subscription chan *pb.CallUpdate

func (s subscription) Next(ctx context.Context) (*pb.CallUpdate, error) {
	select {
	case u := <-s:
		return u, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s subscription) Close() error { return nil }

func toolName(c *pb.DeviceCall) string {
	switch c.Tool.(type) {
	case *pb.DeviceCall_PrepareLedger:
		return "prepare_ledger"
	case *pb.DeviceCall_CollectLedger:
		return "collect_ledger"
	case *pb.DeviceCall_RebaseLedger:
		return "rebase_ledger"
	case *pb.DeviceCall_RemoveLedger:
		return "remove_ledger"
	}
	return ""
}

func (d *device) submit(ctx context.Context, _, _ string, call *pb.DeviceCall) (workspaces.Subscription, error) {
	d.mu.Lock()
	d.requests = append(d.requests, call)
	d.mu.Unlock()
	out := make(subscription, 1)
	if !d.online.Load() {
		out <- &pb.CallUpdate{CallId: call.GetCallId(), Update: &pb.CallUpdate_Offline{Offline: &pb.CallUpdate_DeviceOffline{}}}
		return out, nil
	}
	if d.reject.Load() {
		out <- &pb.CallUpdate{CallId: call.GetCallId(), Update: &pb.CallUpdate_Rejected_{Rejected: &pb.CallUpdate_Rejected{
			Code: pb.CallUpdate_Rejected_CODE_UNKNOWN_DEVICE}}}
		return out, nil
	}
	if d.unsupported.Load() {
		out <- &pb.CallUpdate{CallId: call.GetCallId(), Update: &pb.CallUpdate_Result{Result: &pb.DeviceResult{CallId: call.GetCallId(),
			Fault: &pb.DaemonFault{Code: pb.DaemonFault_CODE_TOOL_UNAVAILABLE}}}}
		return out, nil
	}
	n := d.running.Add(1)
	defer d.running.Add(-1)
	for {
		peak := d.peak.Load()
		if n <= peak || d.peak.CompareAndSwap(peak, n) {
			break
		}
	}
	if gate := d.gate.Load(); gate != nil {
		select {
		case <-*gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	lose := false
	if toolName(call) == "rebase_ledger" {
		d.mu.Lock()
		d.rebases++
		n, hook, loseHook := d.rebases, d.beforeRebase, d.loseRebase
		d.mu.Unlock()
		if hook != nil {
			hook(n)
		}
		lose = loseHook != nil && loseHook(n)
	}
	res, err := d.tools[toolName(call)].Run(ctx, call)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	alter := d.rebaseResult
	d.mu.Unlock()
	if r := res.GetRebaseLedger(); r != nil && alter != nil {
		alter(r)
	}
	if lose {
		out <- &pb.CallUpdate{CallId: call.GetCallId(), Update: &pb.CallUpdate_Lost_{Lost: &pb.CallUpdate_Lost{Reason: "test"}}}
		return out, nil
	}
	res.CallId = call.GetCallId()
	out <- &pb.CallUpdate{CallId: call.GetCallId(), Update: &pb.CallUpdate_Result{Result: res}}
	return out, nil
}

func (d *device) isOnline(string) bool { return d.online.Load() }

func (d *device) calls(tool string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n := 0
	for _, c := range d.requests {
		if toolName(c) == tool {
			n++
		}
	}
	return n
}

type waker struct {
	mu    sync.Mutex
	woken map[string]int
}

func (w *waker) Wake(id string, _ ...pb.WorkerEvent_Nudge_Reason) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.woken[id]++
}

func (w *waker) count(id string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.woken[id]
}

type fixture struct {
	t      *testing.T
	st     *sqlite.DB
	ledger *ledger.Manager
	dev    *device
	slots  *ledgerslots.Slots
	m      *Manager
	wake   *waker
	bus    *sessions.Bus
	cancel context.CancelFunc
	done   chan struct{}
	fences map[string]*store.Fence
}

func check(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) *fixture {
	ctx := t.Context()
	path := sqlitetest.Path(t)
	st, err := sqlite.Open(ctx, path, sqlite.Options{})
	check(t, err)
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UnixMilli()
	check(t, st.CreateUser(ctx, store.User{ID: "u1", Name: "u1", CreatedAt: now}))
	check(t, st.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "d1", UserID: "u1", Name: "desk", MachineID: "m1", CreatedAt: now}))
	_, err = st.CreateProject(ctx, store.Project{ID: "p1", UserID: "u1", Name: "Plans", CreatedAt: now, UpdatedAt: now})
	check(t, err)
	lm, err := ledger.New(path, st, nil, nil)
	check(t, err)
	t.Cleanup(func() { _ = lm.Close(context.Background()) })
	_, err = lm.CommitMain(ctx, "p1", []ledger.Decision{{Number: 0, Title: "Storage", Body: []byte("SQLite.\n")}},
		ledger.CommitOptions{Author: "seed", Message: "seed"})
	check(t, err)
	f := &fixture{t: t, st: st, ledger: lm, dev: newDevice(t), slots: ledgerslots.New(ledgerslots.Writes, ledgerslots.WritesPerUser, ledgerslots.MaxWaiters),
		wake: &waker{woken: map[string]int{}}, fences: map[string]*store.Fence{}}
	f.bus = sessions.NewBus(sessions.DefaultBusOptions())
	f.start(st)
	t.Cleanup(f.stop)
	return f
}

// start runs a manager on st.
func (f *fixture) start(st store.Store) {
	f.m = New(Config{Store: st, Ledger: f.ledger, Submit: f.dev.submit, Online: f.dev.isOnline, Slots: f.slots, Bus: f.bus, Waker: f.wake})
	runCtx, cancel := context.WithCancel(context.Background())
	f.cancel, f.done = cancel, make(chan struct{})
	go func() {
		defer close(f.done)
		f.m.Run(runCtx)
	}()
}

// stop stops the manager's background work.
func (f *fixture) stop() {
	f.cancel()
	<-f.done
}

// session creates a session with a snapshot row and waits for it to be
// ready.
func (f *fixture) session(id string) store.SessionLedger {
	f.t.Helper()
	f.create(id)
	return f.waitFor(id, "ready", func(l store.SessionLedger) bool { return l.State == store.SessionLedgerReady })
}

func (f *fixture) get(id string) store.SessionLedger {
	f.t.Helper()
	l, err := f.st.GetSessionLedger(f.t.Context(), id)
	check(f.t, err)
	return l
}

func (f *fixture) waitFor(id, what string, p func(store.SessionLedger) bool) store.SessionLedger {
	f.t.Helper()
	var l store.SessionLedger
	deadline := time.Now().Add(20 * time.Second)
	for {
		var err error
		l, err = f.st.GetSessionLedger(f.t.Context(), id)
		if err == nil && p(l) {
			return l
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("session %s: timed out waiting for %s: %+v (%v)", id, what, l, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// due makes the session's snapshot due, as a turn's end does, and waits
// for its collection.
func (f *fixture) due(id string) store.SessionLedger {
	f.t.Helper()
	f.markDue(id)
	return f.waitFor(id, "collected", func(l store.SessionLedger) bool { return !l.CollectionDue() })
}

func (f *fixture) markDue(id string) {
	f.t.Helper()
	l := f.get(id)
	prev := l.UpdatedAt
	l.DueAt, l.UpdatedAt = time.Now().UnixMilli(), max(time.Now().UnixMilli(), prev+1)
	ok, err := f.st.UpdateSessionLedger(f.t.Context(), l, prev)
	check(f.t, err)
	if !ok {
		f.t.Fatal("the row changed")
	}
	// Queued at once, as a hint is once the manager reads it: settled
	// waits for it.
	f.m.enqueue(id)
}

func writeFile(t *testing.T, dir, p, content string) {
	t.Helper()
	check(t, os.MkdirAll(filepath.Dir(filepath.Join(dir, p)), 0o755))
	check(t, os.WriteFile(filepath.Join(dir, p), []byte(content), 0o644))
}

func readFile(t *testing.T, dir, p string) (string, bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, p))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	check(t, err)
	return string(data), true
}

func decision(number, title, body string) string {
	return "---\nnumber: " + number + "\ntitle: " + title + "\n---\n\n" + body
}

func (f *fixture) notes(id, category string) []store.Input {
	f.t.Helper()
	var out []store.Input
	for _, in := range f.pendingInputs(id) {
		if in.Kind == store.InputNote && in.Note == category {
			out = append(out, in)
		}
	}
	return out
}

var errRollback = errors.New("rollback")

// pendingInputs takes the session's inputs in a transaction it rolls
// back, so they stay pending.
func (f *fixture) pendingInputs(id string) []store.Input {
	f.t.Helper()
	var out []store.Input
	err := f.st.Tx(f.t.Context(), func(ctx context.Context, tx store.Ops) error {
		r, err := tx.AcquireLease(ctx, store.LeaseRequest{SessionID: id, WorkerID: "w1", BootID: "b", ConnID: "c",
			Now: time.Now().UnixMilli(), ExpiresAt: time.Now().UnixMilli() + 60_000})
		if err != nil {
			return err
		}
		out, err = tx.TakeInputs(ctx, store.Fence{SessionID: id, Epoch: r.Lease.Epoch, WorkerID: "w1", BootID: "b", ConnID: "c", Now: time.Now().UnixMilli()})
		if err != nil {
			return err
		}
		return errRollback
	})
	if !errors.Is(err, errRollback) {
		check(f.t, err)
	}
	return out
}

func (f *fixture) worker() {
	f.t.Helper()
	check(f.t, f.st.CreateIdentity(f.t.Context(), store.Identity{Kind: store.KindWorker, ID: "w1", UserID: "u1", Name: "w1", MachineID: "m2", CreatedAt: time.Now().UnixMilli()}))
}

// create creates a session with a snapshot row and hints the manager.
func (f *fixture) create(id string) {
	f.t.Helper()
	ctx := f.t.Context()
	now := time.Now().UnixMilli()
	res, err := f.st.CreateSession(ctx, store.Session{ID: id, ClientRequestID: id, UserID: "u1", ProjectID: "p1", ClaudeSessionID: "c-" + id,
		DefaultDeviceID: "d1", CreatedAt: now}, nil, 100)
	check(f.t, err)
	head, err := f.st.GetLedgerHead(ctx, "p1")
	check(f.t, err)
	check(f.t, f.st.CreateSessionLedger(ctx, NewSessionLedger(res.Session, head)))
	f.m.Kick(id)
}

// held reports whether the session's turns are held.
func (f *fixture) held(id string) bool {
	f.t.Helper()
	sess, err := f.st.GetSession(f.t.Context(), id, time.Now().UnixMilli())
	check(f.t, err)
	return sess.WorkspaceBlocked
}

// fence leases the session to worker w1 (created by worker()).
func (f *fixture) fence(id string) store.Fence {
	f.t.Helper()
	if fc := f.fences[id]; fc != nil {
		fc.Now = time.Now().UnixMilli()
		return *fc
	}
	now := time.Now().UnixMilli()
	r, err := f.st.AcquireLease(f.t.Context(), store.LeaseRequest{SessionID: id, WorkerID: "w1", BootID: "b", ConnID: "c",
		Now: now, ExpiresAt: now + 10*60_000})
	check(f.t, err)
	fc := store.Fence{SessionID: id, Epoch: r.Lease.Epoch, WorkerID: "w1", BootID: "b", ConnID: "c", Now: now}
	f.fences[id] = &fc
	return fc
}

// startTurn starts turn seq (from 1) of the session: it is running.
func (f *fixture) startTurn(id string, seq int64) {
	f.t.Helper()
	_, err := f.st.ReportTurn(f.t.Context(), f.fence(id), store.TurnReport{LocalSeq: seq, Phase: store.TurnStarted, Origin: store.TurnFromInput,
		At: time.Now().UnixMilli()})
	check(f.t, err)
}

// endTurn ends turn seq, and tells the manager as the session service
// does.
func (f *fixture) endTurn(id string, seq int64) {
	f.t.Helper()
	rep, err := f.st.ReportTurn(f.t.Context(), f.fence(id), store.TurnReport{LocalSeq: seq, Phase: store.TurnEnded, Origin: store.TurnFromInput,
		Outcome: store.TurnCompleted, At: time.Now().UnixMilli()})
	check(f.t, err)
	if !rep.LedgerDue {
		f.t.Fatal("the turn's end made no collection due")
	}
	f.m.Due(f.t.Context(), id)
}

// message submits a user message.
func (f *fixture) message(id, text string) {
	f.t.Helper()
	_, err := f.st.SubmitInput(f.t.Context(), store.Input{InputID: "in-" + text, SessionID: id, ClientMsgID: "m-" + text, Kind: store.InputUser,
		Text: text, Priority: store.InputNormal, CreatedAt: time.Now().UnixMilli()})
	check(f.t, err)
}

// settled waits until the manager has no queued or running work for the
// session: what was queued has finished.
func (f *fixture) settled(id string) {
	f.t.Helper()
	wait.For(f.t, func() bool {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		_, running := f.m.running[id]
		return !running && !f.m.queued[id]
	})
}

// changes lists the entries of a changeset as "path<-base_path" (base path
// only when it differs), in order.
func (f *fixture) changes(id string) []string {
	f.t.Helper()
	cs, err := f.ledger.ReadSessionChangeset(f.t.Context(), id)
	check(f.t, err)
	var out []string
	for _, e := range cs.Entries {
		s := e.Path
		if e.BasePath != "" && e.BasePath != e.Path {
			s += "<-" + e.BasePath
		}
		if e.Added {
			s += " added"
		}
		if e.Deleted {
			s += " deleted"
		}
		out = append(out, s)
	}
	return out
}

// offlineMerge collects the snapshot, takes the device away and merges
// the changeset: the rebase waits for the device.
func (f *fixture) offlineMerge(id string) (store.SessionLedger, store.LedgerChangeset) {
	f.t.Helper()
	l := f.due(id)
	f.dev.online.Store(false)
	merged, err := f.m.MergeSession(f.t.Context(), id, l.ChangesetID)
	check(f.t, err)
	if got := f.get(id); got.Pending != store.SessionLedgerRebase {
		f.t.Fatalf("rebase not pending while offline: %+v", got)
	}
	// What the merge's end queued has run.
	f.m.enqueue(id)
	f.settled(id)
	return l, merged
}

// online brings the device back.
func (f *fixture) online() {
	f.dev.online.Store(true)
	f.m.DeviceOnline("u1", "d1")
}

// rebasedAndCollected waits for the rebase and the collection after it.
func (f *fixture) rebasedAndCollected(id string) store.SessionLedger {
	f.t.Helper()
	return f.waitFor(id, "the rebase and the next collection", func(l store.SessionLedger) bool {
		return l.Pending == "" && !l.CollectionDue() && l.CollectedAt > 0
	})
}

// TestSnapshotLifecycle prepares a snapshot, collects it into the
// session's changeset, tells the agent of problems once, merges it from
// the session and renames the new decision on the device.
func TestSnapshotLifecycle(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	if want := filepath.Join(f.dev.root, "s1"); l.Path != want {
		t.Fatalf("ready at %s, want %s", l.Path, want)
	}
	// The session's turns are no longer held: it is woken.
	wait.For(t, func() bool { return f.wake.count("s1") > 0 })
	if body, ok := readFile(t, l.Path, "D0-Storage.md"); !ok || !strings.Contains(body, "SQLite.") {
		t.Fatalf("snapshot D0 %q", body)
	}

	writeFile(t, l.Path, "D?a-Backups.md", decision("D?a", "Backups", "Nightly, per D0.\n"))
	writeFile(t, l.Path, "notes.txt", "x\n")
	l = f.due("s1")
	if l.ChangesetID == "" || l.ChangeCount != 2 || l.ProblemCount != 1 || len(l.Known) != 2 || l.CollectedAt == 0 {
		t.Fatalf("collected %+v", l)
	}
	if notes := f.notes("s1", noteProblems); len(notes) != 1 || !strings.Contains(notes[0].Text, "notes.txt") || !strings.Contains(notes[0].Text, l.Path) {
		t.Fatalf("problem notes %+v", notes)
	}
	// The same problems again: no second note; fixed: none.
	l = f.due("s1")
	if n := len(f.notes("s1", noteProblems)); n != 1 {
		t.Fatalf("%d problem notes", n)
	}
	check(t, os.Remove(filepath.Join(l.Path, "notes.txt")))
	l = f.due("s1")
	if l.ProblemCount != 0 || l.NotedHash != "" || l.ChangeCount != 1 {
		t.Fatalf("after the fix %+v", l)
	}

	merged, err := f.m.MergeSession(t.Context(), "s1", "other")
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Type != RefuseChangesetChanged {
		t.Fatalf("merge of another changeset: %v", err)
	}
	merged, err = f.m.MergeSession(t.Context(), "s1", l.ChangesetID)
	check(t, err)
	if merged.Merge.Numbers["a"] != 1 {
		t.Fatalf("merged %+v", merged)
	}
	l = f.waitFor("s1", "the rebase", func(l store.SessionLedger) bool { return l.Pending == "" })
	if body, ok := readFile(t, l.Path, "D1-Backups.md"); !ok || !strings.Contains(body, "number: D1") || !strings.Contains(body, "per D0") {
		t.Fatalf("renamed %q", body)
	}
	if _, ok := readFile(t, l.Path, "D?a-Backups.md"); ok {
		t.Fatal("temporary file kept")
	}
	if l.BaseCommit != l.ViewCommit || l.ViewCommit == "" || len(l.Known) != 0 || l.CollectionDue() {
		t.Fatalf("after the rebase %+v", l)
	}
	if notes := f.notes("s1", noteMerged); len(notes) != 1 || !strings.Contains(notes[0].Text, "D?a is now D1") ||
		!strings.Contains(notes[0].Text, "D?a-Backups.md -> D1-Backups.md") {
		t.Fatalf("merged notes %+v", notes)
	}
	// Nothing changed since: no changeset.
	l = f.due("s1")
	if l.ChangesetID != "" || l.ChangeCount != 0 {
		t.Fatalf("after the merge %+v", l)
	}
	_, err = f.m.MergeSession(t.Context(), "s1", "")
	if !errors.As(err, &refusal) || refusal.Type != RefuseNoChanges {
		t.Fatalf("merge without changes: %v", err)
	}
}

// TestPreparationWaitsForTheDevice: offline, a snapshot waits and holds
// the session's turns; it is prepared when the device connects, from the
// main line's head of then.
func TestPreparationWaitsForTheDevice(t *testing.T) {
	f := newFixture(t)
	f.dev.online.Store(false)
	ctx := t.Context()
	f.create("s1")
	f.m.enqueue("s1")
	f.settled("s1")
	if l := f.get("s1"); l.State != store.SessionLedgerPending || l.ErrorCode != 0 {
		t.Fatalf("offline %+v", l)
	}
	if !f.held("s1") {
		t.Fatal("waiting for an offline device does not hold the session")
	}
	// The main line moves while the device is away.
	head, err := f.ledger.CommitMain(ctx, "p1", []ledger.Decision{{Number: 0, Title: "Storage", Body: []byte("SQLite.\n")},
		{Number: 1, Title: "Later", Body: []byte("Later.\n")}},
		ledger.CommitOptions{Author: "seed", Message: "later"})
	check(t, err)
	f.online()
	l := f.waitFor("s1", "ready", func(l store.SessionLedger) bool { return l.State == store.SessionLedgerReady })
	if l.BaseCommit != head || l.ViewCommit != head || l.ErrorCode != 0 {
		t.Fatalf("ready %+v, want base %s", l, head)
	}
	if _, ok := readFile(t, l.Path, "D1-Later.md"); !ok {
		t.Fatal("prepared from the old head")
	}
	if f.held("s1") {
		t.Fatal("a ready snapshot holds the session")
	}
}

// TestPreparationWithoutLedgerCalls: a daemon that does not run ledger
// calls fails the preparation; the session runs without a ledger.
func TestPreparationWithoutLedgerCalls(t *testing.T) {
	f := newFixture(t)
	f.dev.unsupported.Store(true)
	f.create("s1")
	l := f.waitFor("s1", "failed", func(l store.SessionLedger) bool { return l.State == store.SessionLedgerFailed })
	if !strings.Contains(l.ErrorDetail, "does not keep ledger snapshots") || f.held("s1") {
		t.Fatalf("failed %+v, held %v", l, f.held("s1"))
	}
	wait.For(t, func() bool { return f.wake.count("s1") > 0 })
}

// TestPreparationOnARevokedDevice fails the snapshot: the session runs
// without a ledger instead of waiting for a device that never returns.
func TestPreparationOnARevokedDevice(t *testing.T) {
	f := newFixture(t)
	f.dev.online.Store(false)
	if _, err := f.st.DeleteIdentity(t.Context(), store.KindDevice, "d1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	f.create("s1")
	l := f.waitFor("s1", "failed", func(l store.SessionLedger) bool { return l.State == store.SessionLedgerFailed })
	if f.held("s1") || !strings.Contains(l.ErrorDetail, "revoked") {
		t.Fatalf("failed snapshot %+v holds the session: %v", l, f.held("s1"))
	}
}

// TestCollectionWaitsForAWriteSlot: a collection never fails for want of
// a slot; it waits, and runs once one is free, with its one call.
func TestCollectionWaitsForAWriteSlot(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	if !f.slots.TryTake("u1") {
		t.Fatal("no slot")
	}
	writeFile(t, l.Path, "D?a-New.md", decision("D?a", "New", "x\n"))
	f.markDue("s1")
	// The collection called the device and waits for the slot.
	wait.For(t, func() bool { return f.dev.calls("collect_ledger") == 1 && f.slots.Waiting() == 1 })
	f.slots.Release("u1")
	got := f.waitFor("s1", "collected", func(l store.SessionLedger) bool { return !l.CollectionDue() })
	if got.ChangesetID == "" || got.ProblemCount != 0 || got.Attempts != 0 || got.RetryAt != 0 {
		t.Fatalf("collected %+v", got)
	}
	if n := f.dev.calls("collect_ledger"); n != 1 {
		t.Fatalf("%d collections, want 1: the first failed", n)
	}
}

// TestCollectionsInFlight: at most maxInFlight snapshot operations run at
// once, at most maxPerDevice of them on one device.
func TestCollectionsInFlight(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5"} {
		f.session(id)
	}
	gate := make(chan struct{})
	f.dev.gate.Store(&gate)
	f.dev.peak.Store(0)
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5"} {
		f.markDue(id)
	}
	wait.For(t, func() bool { return f.dev.running.Load() == maxPerDevice })
	close(gate)
	for _, id := range []string{"s1", "s2", "s3", "s4", "s5"} {
		f.waitFor(id, "collected", func(l store.SessionLedger) bool { return !l.CollectionDue() })
	}
	if peak := f.dev.peak.Load(); peak > maxPerDevice {
		t.Fatalf("%d calls at once", peak)
	}
}

// TestCollectionKeepsATurnEndDuringIt: a turn that ends while a
// collection runs leaves the collection due.
func TestCollectionKeepsATurnEndDuringIt(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	gate := make(chan struct{})
	f.dev.gate.Store(&gate)
	f.markDue("s1")
	wait.For(t, func() bool { return f.dev.running.Load() == 1 })
	// The turn's end after the collection started.
	time.Sleep(5 * time.Millisecond)
	l = f.get("s1")
	prev := l.UpdatedAt
	l.DueAt, l.UpdatedAt = time.Now().UnixMilli(), max(time.Now().UnixMilli(), prev+1)
	ok, err := f.st.UpdateSessionLedger(t.Context(), l, prev)
	check(t, err)
	if !ok {
		t.Fatal("the row changed")
	}
	f.m.Kick("s1")
	f.dev.gate.Store(nil)
	close(gate)
	// The collection is recorded, and another follows for the later end.
	f.waitFor("s1", "collected twice", func(l store.SessionLedger) bool { return !l.CollectionDue() })
	if n := f.dev.calls("collect_ledger"); n != 2 {
		t.Fatalf("%d collections, want 2", n)
	}
}

// TestCollectionAfterKnownChangesDiffer: known changes that no longer
// match the changeset (a collection whose row update was lost) are
// dropped, and the snapshot collected again at once.
func TestCollectionAfterKnownChangesDiffer(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-New.md", decision("D?a", "New", "x\n"))
	l = f.due("s1")
	// The row knows the content the device holds next, which the
	// changeset does not.
	content := decision("D?a", "New", "y\n")
	sum := sha256.Sum256([]byte(content))
	prev := l.UpdatedAt
	l.Known = []store.LedgerKnownChange{{Path: "D?a-New.md", SHA256: hex.EncodeToString(sum[:])}}
	l.DueAt, l.UpdatedAt = time.Now().UnixMilli(), max(time.Now().UnixMilli(), prev+1)
	ok, err := f.st.UpdateSessionLedger(t.Context(), l, prev)
	check(t, err)
	if !ok {
		t.Fatal("the row changed")
	}
	writeFile(t, l.Path, "D?a-New.md", content)
	f.m.Kick("s1")
	got := f.waitFor("s1", "collected", func(l store.SessionLedger) bool { return !l.CollectionDue() })
	if got.Attempts != 0 || got.RetryAt != 0 || len(got.Known) != 1 || got.Known[0].SHA256 != l.Known[0].SHA256 || got.ChangeCount != 1 {
		t.Fatalf("collected %+v", got)
	}
	if n := f.dev.calls("collect_ledger"); n != 3 {
		t.Fatalf("%d collections, want 3", n)
	}
}

// TestCollectionNotRecordedOverAMerge: a collection whose changeset was
// merged while it ran records nothing; the rebase follows.
func TestCollectionNotRecordedOverAMerge(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-New.md", decision("D?a", "New", "x\n"))
	l = f.due("s1")
	gate := make(chan struct{})
	f.dev.gate.Store(&gate)
	f.markDue("s1")
	wait.For(t, func() bool { return f.dev.running.Load() == 1 })
	// An integrator merges the changeset directly, past the session's
	// lock the collection holds.
	if _, err := f.ledger.MergeChangeset(t.Context(), l.ChangesetID, ledger.MergeOptions{Author: "alice"}); err != nil {
		t.Fatal(err)
	}
	f.dev.gate.Store(nil)
	close(gate)
	f.m.Kick("s1")
	got := f.waitFor("s1", "rebased", func(l store.SessionLedger) bool { return l.Pending == "" })
	if got.CollectedAt != l.CollectedAt {
		t.Fatalf("the collection over the merge was recorded: %+v", got)
	}
	if _, ok := readFile(t, got.Path, "D1-New.md"); !ok {
		t.Fatal("not rebased")
	}
}

// TestNoWorkDuringATurn: nothing changes the snapshot while a turn runs;
// the turn's end collects.
func TestNoWorkDuringATurn(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	f.startTurn("s1", 1)
	writeFile(t, l.Path, "D?a-New.md", decision("D?a", "New", "x\n"))
	f.markDue("s1")
	f.settled("s1")
	if n := f.dev.calls("collect_ledger"); n != 0 {
		t.Fatalf("%d collections during the turn", n)
	}
	f.endTurn("s1", 1)
	got := f.waitFor("s1", "collected", func(l store.SessionLedger) bool { return !l.CollectionDue() })
	if got.ChangeCount != 1 {
		t.Fatalf("collected %+v", got)
	}
}

// TestTurnEndAnnounced: a turn's end that makes a collection due is
// announced to the session's watchers.
func TestTurnEndAnnounced(t *testing.T) {
	f := newFixture(t)
	f.worker()
	f.session("s1")
	f.startTurn("s1", 1)
	w, err := f.bus.Watch("s1", "u1")
	check(t, err)
	defer w.Close()
	f.dev.online.Store(false)
	f.endTurn("s1", 1)
	items, err := w.Take()
	check(t, err)
	if len(items) != 1 || items[0].Event.GetKind() != pb.SessionEvent_KIND_LEDGER || !strings.Contains(items[0].Event.GetPayloadJson(), `"collected"`) {
		t.Fatalf("events %+v", items)
	}
}

// TestRebaseRenumbersLaterChanges: the agent changes its new decision
// after the collection the merge used. The rebase gives the decision its
// number where the agent left it, and the next changeset holds the
// agent's change as a change of D1: never a second new decision, never
// a deletion of D1.
func TestRebaseRenumbersLaterChanges(t *testing.T) {
	cases := []struct {
		name string
		// change changes the snapshot at dir after the collection.
		change func(t *testing.T, dir string)
		// file is D1's file after the rebase, with a line of its body.
		file, body string
		// entries is the next changeset.
		entries []string
		// renamed is the merged note's rename.
		renamed string
	}{
		{name: "retitle", change: func(t *testing.T, dir string) {
			check(t, os.Remove(filepath.Join(dir, "D?a-Cache.md")))
			writeFile(t, dir, "D?a-Caching.md", decision("D?a", "Caching", "Cache reads.\n"))
		}, file: "D1-Caching.md", body: "Cache reads.", entries: []string{"D1-Caching.md<-D1-Cache.md"}, renamed: "D?a-Caching.md -> D1-Caching.md"},
		{name: "move", change: func(t *testing.T, dir string) {
			check(t, os.Remove(filepath.Join(dir, "D?a-Cache.md")))
			writeFile(t, dir, "infra/D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
		}, file: "infra/D1-Cache.md", body: "Cache reads.", entries: []string{"infra/D1-Cache.md<-D1-Cache.md"}, renamed: "infra/D?a-Cache.md -> infra/D1-Cache.md"},
		{name: "revise", change: func(t *testing.T, dir string) {
			writeFile(t, dir, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads and writes, per D?a.\n"))
		}, file: "D1-Cache.md", body: "Cache reads and writes, per D1.", entries: []string{"D1-Cache.md"}, renamed: "D?a-Cache.md -> D1-Cache.md"},
		{name: "rewrite", change: func(t *testing.T, dir string) {
			check(t, os.Remove(filepath.Join(dir, "D?a-Cache.md")))
			writeFile(t, dir, "ops/D?a-Eviction.md", decision("D?a", "Eviction", "Evict the oldest.\n"))
		}, file: "ops/D1-Eviction.md", body: "Evict the oldest.", entries: []string{"ops/D1-Eviction.md<-D1-Cache.md"}, renamed: "ops/D?a-Eviction.md -> ops/D1-Eviction.md"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.worker()
			l := f.session("s1")
			writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
			l, _ = f.offlineMerge("s1")
			c.change(t, l.Path)
			f.online()
			l = f.rebasedAndCollected("s1")
			body, ok := readFile(t, l.Path, c.file)
			if !ok || !strings.Contains(body, "number: D1\n") || !strings.Contains(body, c.body) {
				t.Fatalf("%s: %q", c.file, body)
			}
			for _, p := range []string{"D?a-Cache.md", "D?a-Caching.md", "infra/D?a-Cache.md", "ops/D?a-Eviction.md"} {
				if _, ok := readFile(t, l.Path, p); ok {
					t.Fatalf("%s kept", p)
				}
			}
			if c.file != "D1-Cache.md" {
				if _, ok := readFile(t, l.Path, "D1-Cache.md"); ok {
					t.Fatal("the merged file kept beside the agent's")
				}
			}
			if l.ProblemCount != 0 || l.BaseCommit == l.ViewCommit || l.ChangesetID == "" {
				t.Fatalf("after the rebase %+v", l)
			}
			if got := f.changes(l.ChangesetID); strings.Join(got, ",") != strings.Join(c.entries, ",") {
				t.Fatalf("next changeset %v, want %v", got, c.entries)
			}
			notes := f.notes("s1", noteMerged)
			if len(notes) != 1 || !strings.Contains(notes[0].Text, c.renamed) || strings.Contains(notes[0].Text, "D?a-Cache.md -> D1-Cache.md") && c.file != "D1-Cache.md" ||
				!strings.Contains(notes[0].Text, "are kept") {
				t.Fatalf("merged notes %+v", notes)
			}
			if f.dev.calls("rebase_ledger") != 1 {
				t.Fatalf("%d rebases", f.dev.calls("rebase_ledger"))
			}
		})
	}
}

// TestRebaseKeepsOtherLaterChanges: changes of other files after the
// collection the merge used are kept with their temporary references
// replaced, and go into the next changeset.
func TestRebaseKeepsOtherLaterChanges(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l, _ = f.offlineMerge("s1")
	body, _ := readFile(t, l.Path, "D0-Storage.md")
	writeFile(t, l.Path, "D0-Storage.md", strings.Replace(body, "SQLite.", "SQLite, cached per D?a.", 1))
	writeFile(t, l.Path, "D?b-More.md", decision("D?b", "More", "More.\n"))
	f.online()
	l = f.rebasedAndCollected("s1")
	if body, _ := readFile(t, l.Path, "D0-Storage.md"); !strings.Contains(body, "cached per D1.") {
		t.Fatalf("D0 %q", body)
	}
	if got := f.changes(l.ChangesetID); strings.Join(got, ",") != "D0-Storage.md,D?b-More.md added" {
		t.Fatalf("next changeset %v", got)
	}
}

// TestRebaseAnswerLost: a rebase that took effect but whose answer was
// lost is found by the next attempt's collection, which sends nothing
// again; the agent's change made in between goes into the next changeset.
func TestRebaseAnswerLost(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l = f.due("s1")
	f.dev.mu.Lock()
	f.dev.loseRebase = func(n int) bool { return n == 1 }
	f.dev.mu.Unlock()
	_, err := f.m.MergeSession(t.Context(), "s1", l.ChangesetID)
	check(t, err)
	// The first rebase took effect, its answer was lost: the target is
	// recorded, and the snapshot is renamed.
	lost := f.waitFor("s1", "the lost answer", func(l store.SessionLedger) bool { return l.RebaseTarget != "" && l.Attempts > 0 })
	if _, ok := readFile(t, l.Path, "D1-Cache.md"); !ok {
		t.Fatal("not renamed")
	}
	writeFile(t, l.Path, "D?b-Later.md", decision("D?b", "Later", "Later.\n"))
	// The retry, after the backoff, finds it done.
	l = f.rebasedAndCollected("s1")
	if l.BaseCommit != l.ViewCommit || l.BaseCommit != lost.RebaseTarget || l.RebaseTarget != "" {
		t.Fatalf("after the retry %+v (target %s)", l, lost.RebaseTarget)
	}
	if got := f.changes(l.ChangesetID); strings.Join(got, ",") != "D?b-Later.md added" {
		t.Fatalf("next changeset %v", got)
	}
	if n := f.dev.calls("rebase_ledger"); n != 1 {
		t.Fatalf("%d rebase calls, want 1", n)
	}
	if notes := f.notes("s1", noteMerged); len(notes) != 1 || !strings.Contains(notes[0].Text, "D?a is now D1") {
		t.Fatalf("merged notes %+v", notes)
	}
}

// TestRebaseAnswerLostWithLaterChanges: the lost rebase installed a
// target with the agent's later changes, not the view; found, it leaves
// the view as the next changeset's base and the changes in it.
func TestRebaseAnswerLostWithLaterChanges(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l, _ = f.offlineMerge("s1")
	body, _ := readFile(t, l.Path, "D0-Storage.md")
	writeFile(t, l.Path, "D0-Storage.md", strings.Replace(body, "SQLite.", "SQLite, later.", 1))
	f.dev.mu.Lock()
	f.dev.loseRebase = func(n int) bool { return n == 1 }
	f.dev.mu.Unlock()
	f.online()
	lost := f.waitFor("s1", "the lost answer", func(l store.SessionLedger) bool { return l.RebaseTarget != "" && l.Attempts > 0 })
	l = f.rebasedAndCollected("s1")
	if l.BaseCommit != lost.RebaseTarget || l.BaseCommit == l.ViewCommit {
		t.Fatalf("after the retry %+v (target %s)", l, lost.RebaseTarget)
	}
	if got := f.changes(l.ChangesetID); strings.Join(got, ",") != "D0-Storage.md" {
		t.Fatalf("next changeset %v", got)
	}
	if n := f.dev.calls("rebase_ledger"); n != 1 {
		t.Fatalf("%d rebase calls, want 1", n)
	}
}

// TestRebaseStale: the snapshot changes by hand while the rebase's edits
// apply; the rebase is planned again from what it holds then.
func TestRebaseStale(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l, _ = f.offlineMerge("s1")
	f.dev.mu.Lock()
	f.dev.beforeRebase = func(n int) {
		if n == 1 {
			writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Changed by hand.\n"))
		}
	}
	f.dev.mu.Unlock()
	f.online()
	l = f.rebasedAndCollected("s1")
	if body, _ := readFile(t, l.Path, "D1-Cache.md"); !strings.Contains(body, "Changed by hand.") {
		t.Fatalf("D1 %q", body)
	}
	if got := f.changes(l.ChangesetID); strings.Join(got, ",") != "D1-Cache.md" {
		t.Fatalf("next changeset %v", got)
	}
	if n := f.dev.calls("rebase_ledger"); n != 2 {
		t.Fatalf("%d rebase calls, want 2", n)
	}
}

// TestRebaseWithProblems: files no collection takes (a symbolic link)
// stop the rebase; the
// agent hears of them, its turns are not held meanwhile, and the rebase
// is tried again after its backoff.
func TestRebaseWithProblems(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l, _ = f.offlineMerge("s1")
	if !f.held("s1") {
		t.Fatal("a pending rebase does not hold the session")
	}
	check(t, os.Symlink("D0-Storage.md", filepath.Join(l.Path, "link.md")))
	f.online()
	got := f.waitFor("s1", "the problems", func(l store.SessionLedger) bool { return l.ProblemCount > 0 })
	if got.Pending != store.SessionLedgerRebase || f.held("s1") {
		t.Fatalf("problems %+v, held %v", got, f.held("s1"))
	}
	if notes := f.notes("s1", noteProblems); len(notes) != 1 || !strings.Contains(notes[0].Text, "link.md") {
		t.Fatalf("problem notes %+v", notes)
	}
	// Fixed; nothing hints the manager: the backoff retries.
	check(t, os.Remove(filepath.Join(l.Path, "link.md")))
	l = f.waitFor("s1", "rebased", func(l store.SessionLedger) bool { return l.Pending == "" })
	if _, ok := readFile(t, l.Path, "D1-Cache.md"); !ok || l.ProblemCount != 0 {
		t.Fatalf("after the fix %+v", l)
	}
}

// TestIntegratorWaitsForTheTurn: an integrator's merge or discard of a
// session's changeset is refused while a turn runs. After it, the merge's
// rebase holds the session's next turn, and the merged note comes before
// the user's message that arrived meanwhile.
func TestIntegratorWaitsForTheTurn(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l = f.due("s1")
	f.startTurn("s1", 1)
	c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID)
	check(t, err)
	running := func(err error) bool {
		var precondition *ledger.PreconditionError
		return errors.As(err, &precondition) && precondition.Violations[0].Type == RefuseTurnRunning
	}
	collections := f.dev.calls("collect_ledger")
	if _, err := f.m.MergeChangeset(t.Context(), "alice", c, ledger.MergeOptions{Author: "alice"}); !running(err) {
		t.Fatalf("merge during a turn: %v", err)
	}
	if n := f.dev.calls("collect_ledger"); n != collections {
		t.Fatalf("a refused merge collected the snapshot during the turn (%d collections)", n-collections)
	}
	if _, err := f.m.DiscardChangeset(t.Context(), c, "no", "alice"); !running(err) {
		t.Fatalf("discard during a turn: %v", err)
	}
	if got, err := f.st.GetLedgerChangeset(t.Context(), c.ID); err != nil || got.State != store.LedgerChangesetOpen {
		t.Fatalf("changeset %+v, %v", got, err)
	}
	// The agent retitles its decision during the turn; its end collects.
	check(t, os.Remove(filepath.Join(l.Path, "D?a-Cache.md")))
	writeFile(t, l.Path, "D?a-Caching.md", decision("D?a", "Caching", "Cache reads.\n"))
	f.endTurn("s1", 1)
	f.waitFor("s1", "collected", func(l store.SessionLedger) bool { return !l.CollectionDue() })
	f.dev.online.Store(false)
	if _, err := f.m.MergeChangeset(t.Context(), "alice", c, ledger.MergeOptions{Author: "alice"}); err != nil {
		t.Fatal(err)
	}
	f.message("s1", "next")
	if !f.held("s1") {
		t.Fatal("the next turn is not held by the rebase")
	}
	if got := Status(f.get("s1")); got.GetApplying() != pb.SessionLedgerStatus_APPLYING_MERGE {
		t.Fatalf("status %v", got)
	}
	f.online()
	l = f.waitFor("s1", "rebased", func(l store.SessionLedger) bool { return l.Pending == "" })
	if f.held("s1") || Status(l).GetApplying() != pb.SessionLedgerStatus_APPLYING_UNSPECIFIED {
		t.Fatalf("held after the rebase: %v", Status(l))
	}
	if _, ok := readFile(t, l.Path, "D1-Caching.md"); !ok {
		t.Fatal("not renamed")
	}
	inputs := f.pendingInputs("s1")
	if len(inputs) != 2 || inputs[0].Kind != store.InputNote || inputs[0].Note != noteMerged || inputs[1].Text != "next" {
		t.Fatalf("inputs %+v", inputs)
	}
}

// TestHoldStopsALiveLease: a worker that keeps its lease takes no inputs
// while a reset is owed; once the reset is done the session is woken and
// the turn runs on the reset snapshot, where what it writes stays.
func TestHoldStopsALiveLease(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	fence := f.fence("s1")
	f.startTurn("s1", 1)
	writeFile(t, l.Path, "D?a-Old.md", decision("D?a", "Old", "Old idea.\n"))
	f.endTurn("s1", 1)
	l = f.waitFor("s1", "collected", func(l store.SessionLedger) bool { return !l.CollectionDue() && l.ChangesetID != "" })
	f.dev.online.Store(false)
	c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID)
	check(t, err)
	before := f.wake.count("s1")
	_, err = f.m.DiscardChangeset(t.Context(), c, "not now", "integrator")
	check(t, err)
	// The session service hears that the hold started.
	if f.wake.count("s1") == before {
		t.Fatal("the session was not told of the hold")
	}
	f.message("s1", "go on")
	fence.Now = time.Now().UnixMilli()
	inputs, err := f.st.TakeInputs(t.Context(), fence)
	check(t, err)
	if len(inputs) != 0 || !f.held("s1") || Status(f.get("s1")).GetApplying() != pb.SessionLedgerStatus_APPLYING_DISCARD {
		t.Fatalf("inputs taken while the reset is owed: %d, held %v", len(inputs), f.held("s1"))
	}
	woken := f.wake.count("s1")
	f.online()
	f.waitFor("s1", "reset", func(l store.SessionLedger) bool { return l.Pending == "" })
	wait.For(t, func() bool { return f.wake.count("s1") > woken })
	fence.Now = time.Now().UnixMilli()
	inputs, err = f.st.TakeInputs(t.Context(), fence)
	check(t, err)
	if len(inputs) != 2 || inputs[0].Note != noteDiscarded || inputs[1].Text != "go on" {
		t.Fatalf("inputs after the reset %+v", inputs)
	}
	if _, ok := readFile(t, l.Path, "D?a-Old.md"); ok {
		t.Fatal("the discarded file is still there")
	}
	f.startTurn("s1", 2)
	writeFile(t, l.Path, "D?b-New.md", decision("D?b", "New", "Written after the discard.\n"))
	f.endTurn("s1", 2)
	l = f.waitFor("s1", "collected", func(l store.SessionLedger) bool { return !l.CollectionDue() && l.ChangesetID != "" })
	if _, ok := readFile(t, l.Path, "D?b-New.md"); !ok {
		t.Fatal("the turn's work is gone")
	}
	if got := f.changes(l.ChangesetID); strings.Join(got, ",") != "D?b-New.md added" {
		t.Fatalf("next changeset %v", got)
	}
}

// TestCheckAfterMergeRefused: a check of a merged changeset whose rebase
// is pending does not take the last collection for it.
func TestCheckAfterMergeRefused(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l, _ = f.offlineMerge("s1")
	c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID)
	check(t, err)
	_, err = f.m.CheckChangeset(t.Context(), c)
	var precondition *ledger.PreconditionError
	if !errors.As(err, &precondition) || precondition.Violations[0].Type != ledger.ViolationChangesetChanged {
		t.Fatalf("check of a merged changeset: %v", err)
	}
}

// TestDiscardResetsToTheView: a discard after a rebase that kept later
// changes writes the snapshot again from the view, its new base.
func TestDiscardResetsToTheView(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l, _ = f.offlineMerge("s1")
	writeFile(t, l.Path, "D?b-More.md", decision("D?b", "More", "More.\n"))
	f.online()
	l = f.rebasedAndCollected("s1")
	if l.BaseCommit == l.ViewCommit {
		t.Fatalf("nothing kept %+v", l)
	}
	c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID)
	check(t, err)
	if _, err := f.m.DiscardChangeset(t.Context(), c, "not now", "alice"); err != nil {
		t.Fatal(err)
	}
	got := f.waitFor("s1", "reset", func(x store.SessionLedger) bool { return x.Pending == "" && x.UpdatedAt > l.UpdatedAt })
	if got.BaseCommit != l.ViewCommit || got.ViewCommit != l.ViewCommit {
		t.Fatalf("after the reset %+v", got)
	}
	if _, ok := readFile(t, l.Path, "D?b-More.md"); ok {
		t.Fatal("discarded file kept")
	}
	if notes := f.notes("s1", noteDiscarded); len(notes) != 1 || !strings.Contains(notes[0].Text, "not now") {
		t.Fatalf("discarded notes %+v", notes)
	}
	// Nothing to collect relative to the view.
	got = f.due("s1")
	if got.ChangesetID != "" {
		t.Fatalf("after the reset %+v", got)
	}
}

// TestOfflineRebaseDoesNoWork: a pending rebase of an offline device does
// no ledger work (it would wait for the user's write slot) on any pass;
// one of a revoked device fails the snapshot.
func TestOfflineRebaseDoesNoWork(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	f.offlineMerge("s1")
	f.stop()
	if !f.slots.TryTake("u1") {
		t.Fatal("slot")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 5 {
			f.m.process(t.Context(), "s1")
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a pass waits for the write slot although the device is offline")
	}
	f.slots.Release("u1")
	if n := f.dev.calls("collect_ledger"); n != 1 {
		t.Fatalf("%d collections", n)
	}
	if _, err := f.st.DeleteIdentity(t.Context(), store.KindDevice, "d1", time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	f.m.process(t.Context(), "s1")
	if got := f.get("s1"); got.State != store.SessionLedgerFailed || got.Pending != "" || f.held("s1") {
		t.Fatalf("revoked %+v", got)
	}
}

// TestProblemNotesBounded: an agent that keeps leaving new problems is
// told of them in at most maxNotesWithoutUser notes in a row; after the
// user's next input it is told again.
func TestProblemNotesBounded(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	for i := range maxNotesWithoutUser + 2 {
		writeFile(t, l.Path, fmt.Sprintf("notes-%d.txt", i), "x\n")
		l = f.due("s1")
		// Three notes in a row at most.
		if want := min(i+1, 3); len(f.notes("s1", noteProblems)) != want {
			t.Fatalf("after %d collections: %d notes, want %d", i+1, len(f.notes("s1", noteProblems)), want)
		}
	}
	f.message("s1", "fix it")
	l = f.due("s1")
	if n := len(f.notes("s1", noteProblems)); n != maxNotesWithoutUser+1 {
		t.Fatalf("after the user's input: %d notes", n)
	}
	if !strings.Contains(f.notes("s1", noteProblems)[maxNotesWithoutUser].Text, fmt.Sprintf("notes-%d.txt", maxNotesWithoutUser+1)) {
		t.Fatal("the last note does not list the latest problems")
	}
}

// TestProblemNoteRefused: a stopped session takes no note; the problems
// are not recorded as told.
func TestProblemNoteRefused(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	_, _, _, err := f.st.StopSession(t.Context(), "s1", time.Now().UnixMilli(), "USER")
	check(t, err)
	writeFile(t, l.Path, "notes.txt", "x\n")
	l = f.due("s1")
	if l.ProblemCount != 1 || l.NotedHash != "" {
		t.Fatalf("collected %+v", l)
	}
}

// TestMergeSessionRefusesATurnStartedDuringItsCollection: a turn that
// starts while MergeSession collects refuses the merge.
func TestMergeSessionRefusesATurnStartedDuringItsCollection(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l = f.due("s1")
	gate := make(chan struct{})
	f.dev.gate.Store(&gate)
	errs := make(chan error, 1)
	go func() {
		_, err := f.m.MergeSession(t.Context(), "s1", l.ChangesetID)
		errs <- err
	}()
	wait.For(t, func() bool { return f.dev.running.Load() == 1 })
	f.startTurn("s1", 1)
	f.dev.gate.Store(nil)
	close(gate)
	var refusal *Refusal
	if err := <-errs; !errors.As(err, &refusal) || refusal.Type != RefuseTurnRunning {
		t.Fatalf("merge: %v", err)
	}
	if c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID); err != nil || c.State != store.LedgerChangesetOpen {
		t.Fatalf("changeset %+v, %v", c, err)
	}
}

// TestIntegratorMergeRefusesProblems: an integrator's merge of a session's
// changeset refuses problems of its collection, those of the device's
// included.
func TestIntegratorMergeRefusesProblems(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	// A problem of the device's collection, which the changeset does not
	// hold.
	check(t, os.Symlink("D0-Storage.md", filepath.Join(l.Path, "link.md")))
	l = f.due("s1")
	c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID)
	check(t, err)
	_, err = f.m.MergeChangeset(t.Context(), "alice", c, ledger.MergeOptions{Author: "alice"})
	var precondition *ledger.PreconditionError
	if !errors.As(err, &precondition) || precondition.Violations[0].Type != ledger.ViolationProblem || precondition.Violations[0].Subject != "link.md" {
		t.Fatalf("merge with problems: %v", err)
	}
}

// TestProcessHonorsTheSessionLock: background work on a session waits
// while an RPC holds its lock, and runs once the RPC ends.
func TestProcessHonorsTheSessionLock(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	unlock, err := f.m.lock(t.Context(), "s1")
	check(t, err)
	writeFile(t, l.Path, "D?a-New.md", decision("D?a", "New", "x\n"))
	f.markDue("s1")
	f.settled("s1")
	if n := f.dev.calls("collect_ledger"); n != 0 {
		t.Fatalf("%d collections under an RPC's lock", n)
	}
	unlock()
	// What an RPC's end does.
	f.m.Kick("s1")
	f.waitFor("s1", "collected", func(l store.SessionLedger) bool { return !l.CollectionDue() })
}

// TestRPCsWaitingForOneSessionDoNotStallOthers: requests about a session
// whose lock is held wait without in-flight tokens, so another session's
// snapshot is prepared meanwhile.
func TestRPCsWaitingForOneSessionDoNotStallOthers(t *testing.T) {
	f := newFixture(t)
	f.session("s1")
	unlock, err := f.m.lock(t.Context(), "s1")
	check(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	var wg sync.WaitGroup
	for range maxInFlight {
		wg.Go(func() { _, _ = f.m.MergeSession(ctx, "s1", "x") })
	}
	f.create("s2")
	f.waitFor("s2", "ready", func(l store.SessionLedger) bool { return l.State == store.SessionLedgerReady })
	cancel()
	wg.Wait()
	unlock()
}

// TestDeleteSession refuses a session with changes unless they are
// discarded, discards them only once the session is deleted, and removes
// the snapshot, later when the device is offline.
func TestDeleteSession(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-New.md", decision("D?a", "New", "x\n"))
	l = f.due("s1")
	ctx := t.Context()
	sess, err := f.st.GetSession(ctx, "s1", time.Now().UnixMilli())
	check(t, err)
	_, _, _, err = f.st.StopSession(ctx, "s1", time.Now().UnixMilli(), "USER")
	check(t, err)
	check := func(ctx context.Context) error { return f.st.CheckSessionDeletable(ctx, "s1", time.Now().UnixMilli()) }
	del := func(ctx context.Context) error { return f.st.DeleteSession(ctx, "s1", time.Now().UnixMilli()) }
	err = f.m.DeleteSession(ctx, sess, false, check, del)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Type != RefuseLedgerChanges {
		t.Fatalf("delete with changes: %v", err)
	}
	// A deletion that fails discards nothing.
	failed := errors.New("deletion failed")
	if err := f.m.DeleteSession(ctx, sess, true, check, func(context.Context) error { return failed }); !errors.Is(err, failed) {
		t.Fatalf("failed deletion: %v", err)
	}
	if c, err := f.st.GetLedgerChangeset(ctx, l.ChangesetID); err != nil || c.State != store.LedgerChangesetOpen {
		t.Fatalf("changeset after a failed deletion %+v, %v", c, err)
	}
	f.dev.online.Store(false)
	if err := f.m.DeleteSession(ctx, sess, true, check, del); err != nil {
		t.Fatal(err)
	}
	c, err := f.st.GetLedgerChangeset(ctx, l.ChangesetID)
	if err != nil || c.State != store.LedgerChangesetDiscarded || c.DiscardReason != SessionDeleted {
		t.Fatalf("changeset %+v, %v", c, err)
	}
	if _, err := os.Stat(l.Path); err != nil {
		t.Fatalf("removed while offline: %v", err)
	}
	f.online()
	wait.For(t, func() bool {
		_, statErr := os.Stat(l.Path)
		_, err := f.st.GetLedgerSnapshotRemoval(ctx, "s1")
		return errors.Is(statErr, os.ErrNotExist) && errors.Is(err, store.ErrNotFound)
	})
}

// TestDeleteSessionDueOffline: with the device offline, a turn's end the
// last collection did not see counts as changes.
func TestDeleteSessionDueOffline(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-New.md", decision("D?a", "New", "x\n"))
	ctx := t.Context()
	sess, err := f.st.GetSession(ctx, "s1", time.Now().UnixMilli())
	check(t, err)
	_, _, _, err = f.st.StopSession(ctx, "s1", time.Now().UnixMilli(), "USER")
	check(t, err)
	f.dev.online.Store(false)
	f.markDue("s1")
	chk := func(ctx context.Context) error { return f.st.CheckSessionDeletable(ctx, "s1", time.Now().UnixMilli()) }
	del := func(ctx context.Context) error { return f.st.DeleteSession(ctx, "s1", time.Now().UnixMilli()) }
	err = f.m.DeleteSession(ctx, sess, false, chk, del)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Type != RefuseLedgerChanges {
		t.Fatalf("delete with uncollected changes: %v", err)
	}
}

// TestSweepDiscardsOrphanedChangesets: an open changeset whose session is
// gone (its discard failed after the deletion) is discarded by the sweep.
func TestSweepDiscardsOrphanedChangesets(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-New.md", decision("D?a", "New", "x\n"))
	l = f.due("s1")
	ctx := t.Context()
	_, _, _, err := f.st.StopSession(ctx, "s1", time.Now().UnixMilli(), "USER")
	check(t, err)
	check(t, f.st.DeleteSession(ctx, "s1", time.Now().UnixMilli()))
	f.m.sweep(ctx)
	c, err := f.st.GetLedgerChangeset(ctx, l.ChangesetID)
	if err != nil || c.State != store.LedgerChangesetDiscarded || c.DiscardReason != SessionDeleted {
		t.Fatalf("changeset %+v, %v", c, err)
	}
}

// countingStore counts the store calls of the removals.
type countingStore struct {
	store.Store
	calls *atomic.Int64
}

func (c countingStore) ListLedgerSnapshotRemovals(ctx context.Context, deviceID, after string, limit int) ([]store.LedgerSnapshotRemoval, error) {
	c.calls.Add(1)
	return c.Store.ListLedgerSnapshotRemovals(ctx, deviceID, after, limit)
}

func (c countingStore) DeleteLedgerSnapshotRemoval(ctx context.Context, sessionID string) error {
	c.calls.Add(1)
	return c.Store.DeleteLedgerSnapshotRemoval(ctx, sessionID)
}

// TestRemovalOnABusyDevice: a removal whose device runs maxPerDevice
// operations waits for one of them to end, without spinning; it runs
// then.
func TestRemovalOnABusyDevice(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"s1", "s2", "s3"} {
		f.session(id)
	}
	f.stop()
	var n atomic.Int64
	f.start(countingStore{Store: f.st, calls: &n})
	gate := make(chan struct{})
	f.dev.gate.Store(&gate)
	f.markDue("s1")
	f.markDue("s2")
	wait.For(t, func() bool { return f.dev.running.Load() == maxPerDevice })
	ctx := t.Context()
	sess, err := f.st.GetSession(ctx, "s3", time.Now().UnixMilli())
	check(t, err)
	_, _, _, err = f.st.StopSession(ctx, "s3", time.Now().UnixMilli(), "USER")
	check(t, err)
	chk := func(ctx context.Context) error { return f.st.CheckSessionDeletable(ctx, "s3", time.Now().UnixMilli()) }
	del := func(ctx context.Context) error { return f.st.DeleteSession(ctx, "s3", time.Now().UnixMilli()) }
	check(t, f.m.DeleteSession(ctx, sess, true, chk, del))
	// The removal was listed and found the device busy.
	wait.For(t, func() bool {
		f.m.mu.Lock()
		defer f.m.mu.Unlock()
		return slices.Contains(f.m.blocked["d1"], removalKey+"s3")
	})
	before := n.Load()
	time.Sleep(500 * time.Millisecond)
	if got := n.Load() - before; got != 0 {
		t.Fatalf("%d store calls of the removals while the device is busy", got)
	}
	f.dev.gate.Store(nil)
	close(gate)
	wait.For(t, func() bool {
		_, statErr := os.Stat(filepath.Join(f.dev.root, "s3"))
		_, err := f.st.GetLedgerSnapshotRemoval(ctx, "s3")
		return errors.Is(statErr, os.ErrNotExist) && errors.Is(err, store.ErrNotFound)
	})
}

// setRow changes the session's snapshot row as fn says.
func (f *fixture) setRow(id string, fn func(l *store.SessionLedger)) {
	f.t.Helper()
	l := f.get(id)
	prev := l.UpdatedAt
	fn(&l)
	l.UpdatedAt = max(time.Now().UnixMilli(), prev+1)
	ok, err := f.st.UpdateSessionLedger(f.t.Context(), l, prev)
	check(f.t, err)
	if !ok {
		f.t.Fatal("the row changed")
	}
}

// TestRebaseRefusedByTheDevice: a rebase the device keeps refusing (a
// read-only snapshot directory) is retried after a failure, and after
// maxDeviceFailures in a row becomes a problem of the snapshot: the hold
// ends, the agent is told, and the status shows it. Fixed in the agent's
// next turn, the rebase runs at its end.
func TestRebaseRefusedByTheDevice(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	f.offlineMerge("s1")
	check(t, os.Chmod(l.Path, 0o555))
	defer os.Chmod(l.Path, 0o755)
	f.online()
	got := f.waitFor("s1", "a failed attempt", func(l store.SessionLedger) bool { return l.Attempts > 0 })
	if got.ProblemCount != 0 || got.Pending != store.SessionLedgerRebase || !f.held("s1") {
		t.Fatalf("after one failure %+v", got)
	}
	// The attempts before the last failed too.
	woken := f.wake.count("s1")
	f.setRow("s1", func(l *store.SessionLedger) { l.Attempts, l.RetryAt = maxDeviceFailures-1, 0 })
	f.m.enqueue("s1")
	got = f.waitFor("s1", "the problem", func(l store.SessionLedger) bool { return l.ProblemCount > 0 })
	if f.held("s1") || got.Pending != store.SessionLedgerRebase || !strings.Contains(got.Problems[0].Message, "PERMISSION_DENIED") {
		t.Fatalf("problem %+v, held %v", got, f.held("s1"))
	}
	if st := Status(got); st.GetProblemCount() != 1 || st.GetApplying() != pb.SessionLedgerStatus_APPLYING_MERGE {
		t.Fatalf("status %v", st)
	}
	if notes := f.notes("s1", noteProblems); len(notes) != 1 || !strings.Contains(notes[0].Text, "could not be updated with the merge") {
		t.Fatalf("problem notes %+v", notes)
	}
	wait.For(t, func() bool { return f.wake.count("s1") > woken })
	// The agent fixes it in a turn; the turn's end runs the rebase.
	f.startTurn("s1", 1)
	check(t, os.Chmod(l.Path, 0o755))
	f.endTurn("s1", 1)
	got = f.waitFor("s1", "rebased", func(l store.SessionLedger) bool { return l.Pending == "" })
	if _, ok := readFile(t, l.Path, "D1-Cache.md"); !ok || got.ProblemCount != 0 || got.Attempts != 0 {
		t.Fatalf("after the fix %+v", got)
	}
}

// TestRebaseKeepsChanging: a snapshot that keeps changing during its
// rebase exhausts the rebase's attempts as a device failure does.
func TestRebaseKeepsChanging(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	f.offlineMerge("s1")
	f.setRow("s1", func(l *store.SessionLedger) { l.Attempts = maxDeviceFailures - 1 })
	f.dev.mu.Lock()
	f.dev.beforeRebase = func(n int) {
		writeFile(t, l.Path, "notes.md", fmt.Sprintf("change %d\n", n))
	}
	f.dev.mu.Unlock()
	f.online()
	got := f.waitFor("s1", "the problem", func(l store.SessionLedger) bool { return l.ProblemCount > 0 })
	if !strings.Contains(got.Problems[0].Message, "kept changing") || f.held("s1") || f.dev.calls("rebase_ledger") != maxRebaseAttempts {
		t.Fatalf("problem %+v after %d rebases", got, f.dev.calls("rebase_ledger"))
	}
}

// TestResetRefusedByTheDevice: a reset the device keeps refusing becomes a
// problem of the snapshot the same way.
func TestResetRefusedByTheDevice(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l = f.due("s1")
	f.dev.online.Store(false)
	c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID)
	check(t, err)
	_, err = f.m.DiscardChangeset(t.Context(), c, "no", "alice")
	check(t, err)
	f.m.enqueue("s1")
	f.settled("s1")
	f.setRow("s1", func(l *store.SessionLedger) { l.Attempts = maxDeviceFailures - 1 })
	// A file in the way of the staging directory.
	staging := filepath.Join(f.dev.root, ".staging")
	check(t, os.RemoveAll(staging))
	check(t, os.WriteFile(staging, []byte("x"), 0o644))
	f.online()
	got := f.waitFor("s1", "the problem", func(l store.SessionLedger) bool { return l.ProblemCount > 0 })
	if got.Pending != store.SessionLedgerReset || f.held("s1") || !strings.Contains(got.Problems[0].Message, "could not be written again after the discard") {
		t.Fatalf("problem %+v", got)
	}
	if Status(got).GetApplying() != pb.SessionLedgerStatus_APPLYING_DISCARD {
		t.Fatalf("status %v", Status(got))
	}
}

// TestRebaseResultNamesItsBase: a REBASED answer that does not name the
// request's base commit is not taken for a rebase.
func TestRebaseResultNamesItsBase(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	f.offlineMerge("s1")
	f.dev.mu.Lock()
	f.dev.rebaseResult = func(r *pb.RebaseLedgerResult) { r.CurrentBaseCommit = "" }
	f.dev.mu.Unlock()
	f.online()
	got := f.waitFor("s1", "a failed attempt", func(l store.SessionLedger) bool { return l.Attempts > 0 })
	if got.Pending != store.SessionLedgerRebase || got.RebaseTarget == "" {
		t.Fatalf("after an answer without its base %+v", got)
	}
}

// TestRebaseStaleOnItsTarget: a STALE answer that finds the snapshot on
// the request's base already (a lost answer, then a change) finishes the
// rebase with a collection due.
func TestRebaseStaleOnItsTarget(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	f.offlineMerge("s1")
	f.dev.mu.Lock()
	f.dev.rebaseResult = func(r *pb.RebaseLedgerResult) {
		r.Outcome, r.Error = pb.RebaseLedgerResult_OUTCOME_FAILED, pb.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_STALE
	}
	f.dev.mu.Unlock()
	collections := f.dev.calls("collect_ledger")
	f.online()
	got := f.rebasedAndCollected("s1")
	if got.BaseCommit != got.ViewCommit || f.dev.calls("collect_ledger") != collections+2 {
		t.Fatalf("after the rebase %+v, %d collections", got, f.dev.calls("collect_ledger")-collections)
	}
}

// TestRebaseKeepsATurnEndDuringIt: a turn's end the rebase's collection
// did not see leaves the collection due after the rebase.
func TestRebaseKeepsATurnEndDuringIt(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	f.offlineMerge("s1")
	f.dev.mu.Lock()
	f.dev.beforeRebase = func(int) {
		time.Sleep(2 * time.Millisecond)
		f.setRow("s1", func(l *store.SessionLedger) { l.DueAt = time.Now().UnixMilli() })
	}
	f.dev.mu.Unlock()
	collections := f.dev.calls("collect_ledger")
	f.online()
	got := f.rebasedAndCollected("s1")
	if got.BaseCommit != got.ViewCommit || f.dev.calls("collect_ledger") != collections+2 {
		t.Fatalf("after the rebase %+v, %d collections", got, f.dev.calls("collect_ledger")-collections)
	}
}

// TestCollectionOnAnotherBase: a collection whose manifest records another
// base than the row is collected again relative to it; a base the ledger
// does not hold fails the snapshot.
func TestCollectionOnAnotherBase(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	prepared := l.BaseCommit
	later, err := f.ledger.CommitMain(t.Context(), "p1", []ledger.Decision{{Number: 0, Title: "Storage", Body: []byte("SQLite.\n")},
		{Number: 1, Title: "Later", Body: []byte("Later.\n")}}, ledger.CommitOptions{Author: "seed", Message: "later"})
	check(t, err)
	f.setRow("s1", func(l *store.SessionLedger) { l.BaseCommit = later })
	got := f.due("s1")
	if got.BaseCommit != prepared || got.ChangesetID != "" || got.ProblemCount != 0 {
		t.Fatalf("collected against the wrong base: %+v", got)
	}
	manifest := filepath.Join(f.dev.root, ".manifests", "s1.json")
	data, err := os.ReadFile(manifest)
	check(t, err)
	check(t, os.WriteFile(manifest, []byte(strings.Replace(string(data), prepared, strings.Repeat("e", 40), 1)), 0o644))
	f.markDue("s1")
	got = f.waitFor("s1", "failed", func(l store.SessionLedger) bool { return l.State == store.SessionLedgerFailed })
	if !strings.Contains(got.ErrorDetail, "does not hold") {
		t.Fatalf("failed %+v", got)
	}
}

// TestUnknownDeviceFails: a call the router rejects for an unknown device
// fails the snapshot.
func TestUnknownDeviceFails(t *testing.T) {
	f := newFixture(t)
	f.session("s1")
	f.dev.reject.Store(true)
	f.markDue("s1")
	got := f.waitFor("s1", "failed", func(l store.SessionLedger) bool { return l.State == store.SessionLedgerFailed })
	if !strings.Contains(got.ErrorDetail, "revoked") {
		t.Fatalf("failed %+v", got)
	}
}

// TestRebaseProblemsWake: problems that end the hold wake the session even
// when the agent gets no note for them (three notes in a row already).
func TestRebaseProblemsWake(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	f.offlineMerge("s1")
	for i := range 3 {
		_, err := f.st.SubmitInput(t.Context(), store.Input{InputID: fmt.Sprintf("n%d", i), SessionID: "s1", ClientMsgID: fmt.Sprintf("n%d", i),
			Kind: store.InputNote, Note: noteMerged, Text: "x", Priority: store.InputNormal, CreatedAt: time.Now().UnixMilli()})
		check(t, err)
	}
	check(t, os.Symlink("D0-Storage.md", filepath.Join(l.Path, "link.md")))
	woken := f.wake.count("s1")
	f.online()
	f.waitFor("s1", "the problems", func(l store.SessionLedger) bool { return l.ProblemCount > 0 })
	wait.For(t, func() bool { return f.wake.count("s1") > woken })
	if n := len(f.notes("s1", noteProblems)); n != 0 {
		t.Fatalf("%d problem notes past the bound", n)
	}
}

// TestCheckRefusedAfterTheRowMoved: a check that falls back on the last
// collection reads the row again: a changeset merged meanwhile is
// refused.
func TestCheckRefusedAfterTheRowMoved(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l = f.due("s1")
	c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID)
	check(t, err)
	if _, err := f.ledger.MergeChangeset(t.Context(), c.ID, ledger.MergeOptions{Author: "alice"}); err != nil {
		t.Fatal(err)
	}
	_, err = f.m.recheck(t.Context(), "s1", c)
	var precondition *ledger.PreconditionError
	if !errors.As(err, &precondition) || precondition.Violations[0].Type != ledger.ViolationChangesetChanged {
		t.Fatalf("recheck of a merged changeset: %v", err)
	}
}

// TestSweepRunsPeriodically: the manager's periodic pass discards orphaned
// changesets, from where the last pass stopped.
func TestSweepRunsPeriodically(t *testing.T) {
	f := newFixture(t)
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-New.md", decision("D?a", "New", "x\n"))
	l = f.due("s1")
	f.stop()
	ctx := t.Context()
	_, _, _, err := f.st.StopSession(ctx, "s1", time.Now().UnixMilli(), "USER")
	check(t, err)
	check(t, f.st.DeleteSession(ctx, "s1", time.Now().UnixMilli()))
	clk := clock.New()
	m := New(Config{Store: f.st, Ledger: f.ledger, Clock: clk, Submit: f.dev.submit, Online: f.dev.isOnline, Slots: f.slots})
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(runCtx) }()
	defer func() { cancel(); <-done }()
	wait.For(t, func() bool { return clk.Waiters() >= 2 })
	clk.Advance(scanEvery)
	wait.For(t, func() bool {
		c, err := f.st.GetLedgerChangeset(ctx, l.ChangesetID)
		return err == nil && c.State == store.LedgerChangesetDiscarded
	})
}

// TestDeletedNewDecisionRestored: the agent deletes its new decision after
// the collection the merge used; the ledger keeps every decision, so the
// rebase puts the merged one back and the note says so.
func TestDeletedNewDecisionRestored(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	f.offlineMerge("s1")
	check(t, os.Remove(filepath.Join(l.Path, "D?a-Cache.md")))
	f.online()
	got := f.rebasedAndCollected("s1")
	if body, ok := readFile(t, l.Path, "D1-Cache.md"); !ok || !strings.Contains(body, "Cache reads.") || got.ChangesetID != "" {
		t.Fatalf("after the rebase %+v: D1 %q", got, body)
	}
	if notes := f.notes("s1", noteMerged); len(notes) != 1 || !strings.Contains(notes[0].Text, "put back: D1-Cache.md") {
		t.Fatalf("merged notes %+v", notes)
	}
}

// TestReusedLetters: after the collection the merge used, a file reuses
// the letters of the merged new decision. With the merged path gone and
// that file the only one with them, it stands for the decision (as a
// retitle and rewrite would); beside the merged file, it is another new
// decision, given fresh letters, and the agent is asked to check it.
func TestReusedLetters(t *testing.T) {
	t.Run("merged path gone", func(t *testing.T) {
		f := newFixture(t)
		f.worker()
		l := f.session("s1")
		writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
		f.offlineMerge("s1")
		check(t, os.Remove(filepath.Join(l.Path, "D?a-Cache.md")))
		writeFile(t, l.Path, "D?a-Queue.md", decision("D?a", "Queue", "Use a queue.\n"))
		f.online()
		got := f.rebasedAndCollected("s1")
		if entries := f.changes(got.ChangesetID); strings.Join(entries, ",") != "D1-Queue.md<-D1-Cache.md" || got.ProblemCount != 0 {
			t.Fatalf("next changeset %v, problems %+v", entries, got.Problems)
		}
	})
	t.Run("merged path kept", func(t *testing.T) {
		f := newFixture(t)
		f.worker()
		l := f.session("s1")
		writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
		f.offlineMerge("s1")
		writeFile(t, l.Path, "D?a-Queue.md", decision("D?a", "Queue", "Use a queue.\n"))
		f.online()
		got := f.rebasedAndCollected("s1")
		if entries := f.changes(got.ChangesetID); strings.Join(entries, ",") != "D?b-Queue.md added" {
			t.Fatalf("next changeset %v", entries)
		}
		if body, ok := readFile(t, l.Path, "D1-Cache.md"); !ok || !strings.Contains(body, "Cache reads.") {
			t.Fatalf("D1 %q", body)
		}
		notes := f.notes("s1", noteMerged)
		if len(notes) != 1 || !strings.Contains(notes[0].Text, "Check these files:\n- D?b-Queue.md: this new decision reused the temporary number D?a") {
			t.Fatalf("merged notes %+v", notes)
		}
	})
}

// TestIntegratorMergeRefusedForATurnStartedDuringItsCollection: a turn
// that starts while an integrator's merge collects refuses the merge,
// checked again right before it.
func TestIntegratorMergeRefusedForATurnStartedDuringItsCollection(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l = f.due("s1")
	c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID)
	check(t, err)
	gate := make(chan struct{})
	f.dev.gate.Store(&gate)
	errs := make(chan error, 1)
	go func() {
		_, err := f.m.MergeChangeset(t.Context(), "alice", c, ledger.MergeOptions{Author: "alice"})
		errs <- err
	}()
	wait.For(t, func() bool { return f.dev.running.Load() == 1 })
	f.startTurn("s1", 1)
	f.dev.gate.Store(nil)
	close(gate)
	var precondition *ledger.PreconditionError
	if err := <-errs; !errors.As(err, &precondition) || precondition.Violations[0].Type != RefuseTurnRunning {
		t.Fatalf("merge: %v", err)
	}
	if got, err := f.st.GetLedgerChangeset(t.Context(), c.ID); err != nil || got.State != store.LedgerChangesetOpen {
		t.Fatalf("changeset %+v, %v", got, err)
	}
}

// TestResetWakesWithoutANote: the end of a reset wakes the session even
// when its note is refused (the session's inputs are at their bound).
func TestResetWakesWithoutANote(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l = f.due("s1")
	f.dev.online.Store(false)
	c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID)
	check(t, err)
	_, err = f.m.DiscardChangeset(t.Context(), c, "no", "alice")
	check(t, err)
	for i := range store.MaxOpenInputs {
		f.message("s1", fmt.Sprintf("m%d", i))
	}
	for i := range store.MaxNoteOverflow {
		_, err := f.st.SubmitInput(t.Context(), store.Input{InputID: fmt.Sprintf("n%d", i), SessionID: "s1", ClientMsgID: fmt.Sprintf("n%d", i),
			Kind: store.InputNote, Note: noteMerged, Text: "x", Priority: store.InputNormal, CreatedAt: time.Now().UnixMilli()})
		check(t, err)
	}
	woken := f.wake.count("s1")
	f.online()
	f.waitFor("s1", "reset", func(l store.SessionLedger) bool { return l.Pending == "" })
	wait.For(t, func() bool { return f.wake.count("s1") > woken })
	if n := len(f.notes("s1", noteDiscarded)); n != 0 {
		t.Fatalf("%d discarded notes past the bound", n)
	}
}

// cursorStore records where each listing of orphaned changesets starts.
type cursorStore struct {
	store.Store
	mu     sync.Mutex
	starts []string
}

func (c *cursorStore) ListOrphanSessionChangesets(ctx context.Context, after string, limit int) ([]store.LedgerChangeset, error) {
	c.mu.Lock()
	c.starts = append(c.starts, after)
	c.mu.Unlock()
	return c.Store.ListOrphanSessionChangesets(ctx, after, limit)
}

// TestSweepMovesOn: each pass of the sweep goes on from where the last
// stopped, so orphans whose discard keeps failing (and stay first) do not
// keep it from the others; after the last page it starts over.
func TestSweepMovesOn(t *testing.T) {
	f := newFixture(t)
	f.stop()
	ctx := t.Context()
	now := time.Now().UnixMilli()
	var ids []string
	for i := range sweepPage + 1 {
		id := fmt.Sprintf("s%02d", i)
		_, err := f.st.CreateSession(ctx, store.Session{ID: id, ClientRequestID: id, UserID: "u1", ProjectID: "p1", ClaudeSessionID: "c-" + id,
			DefaultDeviceID: "d1", CreatedAt: now}, nil, 100)
		check(t, err)
		cs := fmt.Sprintf("01a20000-0000-7000-8000-0000000000%02d", i)
		check(t, f.st.CreateLedgerChangeset(ctx, store.LedgerChangeset{ID: cs, ProjectID: "p1", SessionID: id, State: store.LedgerChangesetOpen,
			CreatedAt: now, UpdatedAt: now}))
		_, _, _, err = f.st.StopSession(ctx, id, now, "USER")
		check(t, err)
		check(t, f.st.DeleteSession(ctx, id, now))
		ids = append(ids, cs)
	}
	cs := &cursorStore{Store: f.st}
	m := New(Config{Store: cs, Ledger: f.ledger, Submit: f.dev.submit, Online: f.dev.isOnline, Slots: f.slots})
	m.sweep(ctx)
	m.sweep(ctx)
	m.sweep(ctx)
	if want := []string{"", ids[sweepPage-1], ""}; !slices.Equal(cs.starts, want) {
		t.Fatalf("sweeps started at %v, want %v", cs.starts, want)
	}

}

// TestDiscardRefusedForTakenInputs: once the worker has taken the user's
// input, before it reports the turn started, a discard or a merge of the
// session's changeset is refused: the turn for that input runs on the
// snapshot as it is.
func TestDiscardRefusedForTakenInputs(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	f.startTurn("s1", 1)
	writeFile(t, l.Path, "D?a-Old.md", decision("D?a", "Old", "Old idea.\n"))
	f.endTurn("s1", 1)
	l = f.waitFor("s1", "collected", func(l store.SessionLedger) bool { return !l.CollectionDue() && l.ChangesetID != "" })
	f.message("s1", "go on")
	inputs, err := f.st.TakeInputs(t.Context(), f.fence("s1"))
	check(t, err)
	if len(inputs) != 1 {
		t.Fatalf("inputs %+v", inputs)
	}
	c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID)
	check(t, err)
	var precondition *ledger.PreconditionError
	if _, err := f.m.DiscardChangeset(t.Context(), c, "not now", "integrator"); !errors.As(err, &precondition) ||
		precondition.Violations[0].Type != RefuseTurnRunning {
		t.Fatalf("discard with an input taken: %v", err)
	}
	if _, err := f.m.MergeChangeset(t.Context(), "alice", c, ledger.MergeOptions{Author: "alice"}); !errors.As(err, &precondition) ||
		precondition.Violations[0].Type != RefuseTurnRunning {
		t.Fatalf("merge with an input taken: %v", err)
	}
	var refusal *Refusal
	if _, err := f.m.MergeSession(t.Context(), "s1", l.ChangesetID); !errors.As(err, &refusal) || refusal.Type != RefuseTurnRunning {
		t.Fatalf("session merge with an input taken: %v", err)
	}
	f.startTurn("s1", 2)
	writeFile(t, l.Path, "D?b-New.md", decision("D?b", "New", "Written for the user's input.\n"))
	f.endTurn("s1", 2)
	l = f.waitFor("s1", "collected", func(l store.SessionLedger) bool { return !l.CollectionDue() })
	if got, err := f.st.GetLedgerChangeset(t.Context(), c.ID); err != nil || got.State != store.LedgerChangesetOpen || l.Pending != "" {
		t.Fatalf("changeset %+v, %v; snapshot %+v", got, err, l)
	}
	if _, ok := readFile(t, l.Path, "D?b-New.md"); !ok {
		t.Fatal("the turn's work is gone")
	}
}

// TestMergeOfAFailedSnapshot: a merge of the changeset of a snapshot that
// failed owes it nothing, and its status says nothing is being applied.
func TestMergeOfAFailedSnapshot(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	l = f.due("s1")
	// The snapshot disappears from the device: the next collection fails it.
	check(t, os.RemoveAll(l.Path))
	check(t, os.RemoveAll(filepath.Join(f.dev.root, ".manifests")))
	f.markDue("s1")
	f.waitFor("s1", "failed", func(l store.SessionLedger) bool { return l.State == store.SessionLedgerFailed })
	c, err := f.st.GetLedgerChangeset(t.Context(), l.ChangesetID)
	check(t, err)
	if _, err := f.m.MergeChangeset(t.Context(), "u1", c, ledger.MergeOptions{Author: "alice"}); err != nil {
		t.Fatal(err)
	}
	got := f.get("s1")
	if st := Status(got); got.Pending != "" || st.GetApplying() != pb.SessionLedgerStatus_APPLYING_UNSPECIFIED || st.GetState() != pb.SessionLedgerStatus_STATE_FAILED {
		t.Fatalf("status %v, pending %q", st, got.Pending)
	}
	// Status alone leaves applying empty for a row that is not ready.
	got.Pending = store.SessionLedgerRebase
	if st := Status(got); st.GetApplying() != pb.SessionLedgerStatus_APPLYING_UNSPECIFIED {
		t.Fatalf("status of a failed row owing a rebase %v", st)
	}
}

// TestFreshLettersStaleReference: a file given fresh letters keeps its
// reference to the reused ones, which is a problem of the next changeset
// until the agent resolves it: the changeset cannot be merged meanwhile.
func TestFreshLettersStaleReference(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	f.offlineMerge("s1")
	writeFile(t, l.Path, "D?a-Queue.md", decision("D?a", "Queue", "Use a queue in front of D?a.\n"))
	f.online()
	got := f.rebasedAndCollected("s1")
	if got.ProblemCount != 1 || got.Problems[0].Path != "D?b-Queue.md" ||
		got.Problems[0].Message != "refers to D?a, which no decision of this changeset has; check it" {
		t.Fatalf("after the rebase %+v", got)
	}
	var refusal *Refusal
	if _, err := f.m.MergeSession(t.Context(), "s1", got.ChangesetID); !errors.As(err, &refusal) || refusal.Type != RefuseProblems {
		t.Fatalf("merge with a stale reference: %v", err)
	}
	// Resolved: it refers to the merged decision by its number.
	writeFile(t, l.Path, "D?b-Queue.md", decision("D?b", "Queue", "Use a queue in front of D1.\n"))
	got = f.due("s1")
	if got.ProblemCount != 0 {
		t.Fatalf("after the fix %+v", got)
	}
}

// TestRebaseWakesWithoutANote: the end of a rebase wakes the session even
// when its note is refused (the session's inputs are at their bound).
func TestRebaseWakesWithoutANote(t *testing.T) {
	f := newFixture(t)
	f.worker()
	l := f.session("s1")
	writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
	f.offlineMerge("s1")
	for i := range store.MaxOpenInputs {
		f.message("s1", fmt.Sprintf("m%d", i))
	}
	for i := range store.MaxNoteOverflow {
		_, err := f.st.SubmitInput(t.Context(), store.Input{InputID: fmt.Sprintf("n%d", i), SessionID: "s1", ClientMsgID: fmt.Sprintf("n%d", i),
			Kind: store.InputNote, Note: noteDiscarded, Text: "x", Priority: store.InputNormal, CreatedAt: time.Now().UnixMilli()})
		check(t, err)
	}
	woken := f.wake.count("s1")
	f.online()
	f.waitFor("s1", "rebased", func(l store.SessionLedger) bool { return l.Pending == "" })
	wait.For(t, func() bool { return f.wake.count("s1") > woken })
	if n := len(f.notes("s1", noteMerged)); n != 0 {
		t.Fatalf("%d merged notes past the bound", n)
	}
}

// failedAttempts runs the session's pending step until it has failed n
// times, each after its backoff is cleared (as time passing would); the
// manager's own loop is stopped.
func (f *fixture) failedAttempts(id string, n int64) store.SessionLedger {
	f.t.Helper()
	var l store.SessionLedger
	for range n {
		before := f.get(id).Attempts
		f.setRow(id, func(l *store.SessionLedger) { l.RetryAt = 0 })
		f.m.process(f.t.Context(), id)
		if l = f.get(id); l.Attempts != before+1 {
			f.t.Fatalf("attempt not counted: %+v", l)
		}
	}
	return l
}

// TestDeviceFailureThreshold: a rebase the device refuses becomes a
// problem on the third failed attempt in a row, not before; so does one
// whose own collection the device refuses.
func TestDeviceFailureThreshold(t *testing.T) {
	for _, c := range []struct {
		name, mode, message string
	}{
		{name: "rebase", mode: "dir", message: "the device answered PERMISSION_DENIED"},
		{name: "collection", mode: "manifests", message: "the device could not collect it"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			f.worker()
			l := f.session("s1")
			writeFile(t, l.Path, "D?a-Cache.md", decision("D?a", "Cache", "Cache reads.\n"))
			f.offlineMerge("s1")
			f.stop()
			f.dev.online.Store(true)
			locked := l.Path
			if c.mode == "manifests" {
				locked = filepath.Join(f.dev.root, ".manifests")
			}
			check(t, os.Chmod(locked, 0o500))
			defer os.Chmod(locked, 0o755)
			if c.mode == "manifests" {
				check(t, os.Chmod(locked, 0o000))
			}
			got := f.failedAttempts("s1", 2)
			if got.ProblemCount != 0 || !f.held("s1") {
				t.Fatalf("after two failures %+v", got)
			}
			got = f.failedAttempts("s1", 1)
			if got.ProblemCount != 1 || f.held("s1") || !strings.Contains(got.Problems[0].Message, c.message) {
				t.Fatalf("after three failures %+v", got)
			}
		})
	}
}
