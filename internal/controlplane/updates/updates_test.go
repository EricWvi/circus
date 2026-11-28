package updates_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/controlplane/updates"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
	"github.com/EricWvi/circus/internal/store/storetest"
	"github.com/EricWvi/circus/internal/wire"
)

type daemons struct {
	mu      sync.Mutex
	online  map[string]bool // device id → online with the feature
	failure error
	sent    []string // "device:update id"
}

func (d *daemons) Supports(id, feature string) (bool, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.online[id], d.online[id] && feature == wire.FeatureMachineUpdate
}

func (d *daemons) UpdateMachine(_ context.Context, id string, u *circusv1.UpdateMachine) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failure != nil {
		return d.failure
	}
	d.sent = append(d.sent, id+":"+u.GetUpdateId())
	return nil
}

type workers struct {
	mu        sync.Mutex
	updatable map[string]bool
	sent      []string
}

func (w *workers) Updatable(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.updatable[id]
}

func (w *workers) UpdateMachine(id string, u *circusv1.UpdateMachine) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.updatable[id] {
		return false
	}
	w.sent = append(w.sent, id+":"+u.GetUpdateId())
	return true
}

// hookStore runs beforeTx (once) as the next transaction starts: a commit
// of another request between Start's reads and its write.
type hookStore struct {
	store.Store
	mu       sync.Mutex
	beforeTx func()
}

func (h *hookStore) Tx(ctx context.Context, fn func(context.Context, store.Ops) error) error {
	h.mu.Lock()
	hook := h.beforeTx
	h.beforeTx = nil
	h.mu.Unlock()
	if hook != nil {
		hook()
	}
	return h.Store.Tx(ctx, fn)
}

type fixture struct {
	st      *hookStore
	faulty  *storetest.Faulty
	m       *updates.Manager
	daemons *daemons
	workers *workers
	now     time.Time
	logs    *bytes.Buffer
}

const latest = "v1.5.0"

// newFixture: user u1 has machine m1 with device d1 and worker w1 at
// v1.4.0, machine m2 with device d2 (a development build) and machine m3
// with device d3 at v1.4.0; user u2 has device x1 on its own machine m1.
// Nothing is online.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cp.db")
	sqlitetest.Seed(t, path)
	db, err := sqlite.Open(t.Context(), path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	faulty := storetest.NewFaulty(db)
	f := &fixture{st: &hookStore{Store: faulty}, faulty: faulty, daemons: &daemons{online: map[string]bool{}}, workers: &workers{updatable: map[string]bool{}},
		now: time.UnixMilli(1_800_000_000_000), logs: &bytes.Buffer{}}
	ctx := t.Context()
	for _, u := range []string{"u1", "u2"} {
		must(t, db.CreateUser(ctx, store.User{ID: u, Name: u, CreatedAt: 1}))
	}
	for _, id := range []store.Identity{
		{Kind: store.KindDevice, ID: "d1", UserID: "u1", Name: "laptop", MachineID: "m1", ClientVersion: "v1.4.0", CreatedAt: 1},
		{Kind: store.KindWorker, ID: "w1", UserID: "u1", Name: "laptop", MachineID: "m1", ClientVersion: "v1.4.0", CreatedAt: 1},
		{Kind: store.KindDevice, ID: "d2", UserID: "u1", Name: "dev", MachineID: "m2", ClientVersion: "dev", CreatedAt: 1},
		{Kind: store.KindDevice, ID: "d3", UserID: "u1", Name: "server", MachineID: "m3", ClientVersion: "v1.4.0", CreatedAt: 1},
		{Kind: store.KindDevice, ID: "x1", UserID: "u2", Name: "other", MachineID: "m1", ClientVersion: "v1.4.0", CreatedAt: 1},
	} {
		must(t, db.CreateIdentity(ctx, id))
	}
	f.m, err = updates.New(updates.Config{Store: f.st, Daemons: f.daemons, Workers: f.workers, Now: func() time.Time { return f.now },
		Latest: func(context.Context) (string, error) { return latest, nil },
		Logger: slog.New(slog.NewTextHandler(f.logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	f.m.Check(ctx)
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) record(t *testing.T, user, machine string) (store.MachineUpdate, bool) {
	t.Helper()
	u, err := f.st.GetMachineUpdate(t.Context(), user, machine)
	if errors.Is(err, store.ErrNotFound) {
		return u, false
	}
	must(t, err)
	return u, true
}

func (f *fixture) timers(t *testing.T) []store.Timer {
	t.Helper()
	ts, err := f.st.ListTimers(t.Context(), store.TimerFilter{Kind: timers.MachineUpdate, Limit: 10})
	must(t, err)
	return ts
}

func (f *fixture) identity(t *testing.T, kind store.PrincipalKind, id string) store.Identity {
	t.Helper()
	ident, err := f.st.GetIdentity(t.Context(), kind, id)
	must(t, err)
	return ident
}

func (f *fixture) setVersion(t *testing.T, kind store.PrincipalKind, id, version string) {
	t.Helper()
	_, err := f.st.UpdateHelloMetadata(t.Context(), kind, id, store.HelloMetadata{ClientVersion: version})
	must(t, err)
}

func (f *fixture) sent() string {
	return strings.Join(append(append([]string{}, f.daemons.sent...), f.workers.sent...), ",")
}

func TestCheckKeepsLastGoodRelease(t *testing.T) {
	var answer string
	var fail error
	m, err := updates.New(updates.Config{Store: nopStore{}, Daemons: &daemons{}, Workers: &workers{},
		Latest: func(context.Context) (string, error) { return answer, fail }})
	must(t, err)
	if m.Latest() != "" {
		t.Fatal("a release is known before any check")
	}
	answer = "v1.0.0"
	m.Check(t.Context())
	if m.Latest() != "v1.0.0" {
		t.Fatalf("latest = %q", m.Latest())
	}
	for _, bad := range []struct {
		answer string
		err    error
	}{
		{"", errors.New("network down")},
		{"dev", nil},
		// A valid tag longer than a stored target version.
		{"v2.0.0-" + strings.Repeat("a", 60), nil},
	} {
		answer, fail = bad.answer, bad.err
		m.Check(t.Context())
		if m.Latest() != "v1.0.0" {
			t.Fatalf("after reading %q, %v latest = %q, want the last good one", bad.answer, bad.err, m.Latest())
		}
	}
	answer, fail = "v1.1.0", nil
	m.Check(t.Context())
	if m.Latest() != "v1.1.0" {
		t.Fatalf("latest = %q", m.Latest())
	}
}

// TestRunChecksPeriodically: Run reads at once and then every interval.
func TestRunChecksPeriodically(t *testing.T) {
	calls := make(chan struct{}, 8)
	m, err := updates.New(updates.Config{Store: nopStore{}, Daemons: &daemons{}, Workers: &workers{}, CheckInterval: 10 * time.Millisecond,
		Latest: func(context.Context) (string, error) { calls <- struct{}{}; return "v1.0.0", nil }})
	must(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); m.Run(ctx) }()
	for range 2 {
		select {
		case <-calls:
		case <-time.After(10 * time.Second):
			t.Fatal("no periodic check")
		}
	}
	cancel()
	<-done
	// Without a reader, Run returns at once and nothing is offered.
	d := &daemons{online: map[string]bool{"d": true}}
	m, err = updates.New(updates.Config{Store: nopStore{}, Daemons: d, Workers: &workers{}})
	must(t, err)
	m.Run(t.Context())
	id := store.Identity{Kind: store.KindDevice, ID: "d", MachineID: "m", ClientVersion: "v0.1.0"}
	if got := m.Offer(id, updates.Machine{}); got != "" {
		t.Errorf("offer without a known release = %q", got)
	}
}

// TestOffer: only the card of a role that is itself online as the
// installed service offers an update, for an older release, while no
// update of its machine runs.
func TestOffer(t *testing.T) {
	f := newFixture(t)
	f.daemons.online["d1"] = true
	running := &store.MachineUpdate{State: store.MachineUpdateRunning}
	failed := &store.MachineUpdate{State: store.MachineUpdateFailed, Failure: store.UpdateTimedOut}
	d1 := f.identity(t, store.KindDevice, "d1")
	w1 := f.identity(t, store.KindWorker, "w1")
	at := func(id store.Identity, v string) store.Identity { id.ClientVersion = v; return id }
	for _, tc := range []struct {
		name string
		id   store.Identity
		mach updates.Machine
		want string
	}{
		{"older release", d1, updates.Machine{}, latest},
		{"older pre-release", at(d1, "v1.5.0-rc.1"), updates.Machine{}, latest},
		{"latest", at(d1, latest), updates.Machine{}, ""},
		{"newer", at(d1, "v1.6.0"), updates.Machine{}, ""},
		{"dev build", at(d1, "dev"), updates.Machine{}, ""},
		{"dev worker", at(d1, "0.0.0"), updates.Machine{}, ""},
		// The worker is not online as the service, although its daemon is.
		{"role not capable", w1, updates.Machine{}, ""},
		{"running", d1, updates.Machine{Update: running}, ""},
		{"failed: retry offered", d1, updates.Machine{Update: failed}, latest},
	} {
		if got := f.m.Offer(tc.id, tc.mach); got != tc.want {
			t.Errorf("%s: offer = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestExecutorChoice: among the machine's capable roles, one below the
// target, the daemon first.
func TestExecutorChoice(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		daemon, worker       bool   // capable
		daemonV, workerV     string // versions
		pressKind            store.PrincipalKind
		pressID, wantSentPfx string
	}{
		{"both below: the daemon", true, true, "v1.4.0", "v1.4.0", store.KindWorker, "w1", "d1:"},
		{"daemon at target: the worker below", true, true, latest, "v1.4.0", store.KindWorker, "w1", "w1:"},
		{"worker at target: the daemon below", true, true, "v1.4.0", latest, store.KindDevice, "d1", "d1:"},
		{"only the pressed worker", false, true, "v1.4.0", "v1.4.0", store.KindWorker, "w1", "w1:"},
		{"the daemon for a pressed worker", true, true, "v1.4.0", "v1.3.0", store.KindWorker, "w1", "d1:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.daemons.online["d1"], f.workers.updatable["w1"] = tc.daemon, tc.worker
			f.setVersion(t, store.KindDevice, "d1", tc.daemonV)
			f.setVersion(t, store.KindWorker, "w1", tc.workerV)
			u, err := f.m.Start(t.Context(), "u1", tc.pressKind, tc.pressID)
			must(t, err)
			if f.sent() != tc.wantSentPfx+u.UpdateID {
				t.Fatalf("sent %q, want %q", f.sent(), tc.wantSentPfx+u.UpdateID)
			}
		})
	}
}

// TestStartOneUpdate: either card starts the machine's one update, which
// both cards then show; pressing again returns it unchanged.
func TestStartOneUpdate(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.daemons.online["d1"], f.workers.updatable["w1"] = true, true
	u, err := f.m.Start(ctx, "u1", store.KindWorker, "w1")
	must(t, err)
	want := store.MachineUpdate{UserID: "u1", MachineID: "m1", UpdateID: u.UpdateID, TargetVersion: latest, State: store.MachineUpdateRunning,
		StartedAt: f.now.UnixMilli(), DeadlineAt: f.now.Add(5 * time.Minute).UnixMilli()}
	if u != want {
		t.Fatalf("update = %+v\nwant %+v", u, want)
	}
	if ts := f.timers(t); len(ts) != 1 || ts[0].SubjectID != u.UpdateID || ts[0].DueAt != want.DeadlineAt {
		t.Fatalf("timers = %+v", ts)
	}
	machines, err := f.m.Machines(ctx, "u1")
	must(t, err)
	if got := machines["m1"].Update; got == nil || *got != want ||
		f.m.Offer(f.identity(t, store.KindDevice, "d1"), machines["m1"]) != "" || f.m.Offer(f.identity(t, store.KindWorker, "w1"), machines["m1"]) != "" {
		t.Fatalf("machine m1 = %+v", machines["m1"])
	}
	again, err := f.m.Start(ctx, "u1", store.KindDevice, "d1")
	must(t, err)
	if again != want || len(f.daemons.sent)+len(f.workers.sent) != 1 {
		t.Fatalf("second start = %+v, sent %q", again, f.sent())
	}
	if _, ok := f.record(t, "u2", "m1"); ok {
		t.Fatal("another user's machine got the update")
	}
}

func TestStartRefusals(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.daemons.online["d1"], f.daemons.online["d2"], f.daemons.online["x1"] = true, true, true
	for _, tc := range []struct {
		name, user string
		kind       store.PrincipalKind
		id         string
		want       error
		reason     string
	}{
		{"unknown", "u1", store.KindDevice, "nope", updates.ErrNotFound, ""},
		{"another user's", "u2", store.KindDevice, "d1", updates.ErrNotFound, ""},
		{"wrong kind", "u1", store.KindWorker, "d1", updates.ErrNotFound, ""},
		{"dev build", "u1", store.KindDevice, "d2", updates.ErrUnavailable, `"dev" is not a release`},
		// The device could execute, but the pressed worker is not online as the service.
		{"pressed role not capable", "u1", store.KindWorker, "w1", updates.ErrUnavailable, "the worker is not online as the installed service"},
		{"offline device", "u1", store.KindDevice, "d3", updates.ErrUnavailable, "the device is not online as the installed service"},
	} {
		_, err := f.m.Start(ctx, tc.user, tc.kind, tc.id)
		if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.reason) {
			t.Errorf("%s: %v, want %v with %q", tc.name, err, tc.want, tc.reason)
		}
	}
	f.setVersion(t, store.KindDevice, "d1", latest)
	if _, err := f.m.Start(ctx, "u1", store.KindDevice, "d1"); !errors.Is(err, updates.ErrUnavailable) || err.Error() != "already at the latest release "+latest {
		t.Errorf("at the latest release: %v", err)
	}
	_, err := f.st.DeleteIdentity(ctx, store.KindWorker, "w1", 2)
	must(t, err)
	if _, err := f.m.Start(ctx, "u1", store.KindWorker, "w1"); !errors.Is(err, updates.ErrNotFound) {
		t.Errorf("deleted worker: %v", err)
	}
	if f.sent() != "" {
		t.Errorf("refusals sent %q", f.sent())
	}
	if _, ok := f.record(t, "u1", "m1"); ok {
		t.Error("a refusal recorded an update")
	}
}

// TestStartRacesReconnect: the pressed role reconnects at the target
// between the press's reads and its write: nothing is recorded or sent.
func TestStartRacesReconnect(t *testing.T) {
	f := newFixture(t)
	f.daemons.online["d1"] = true
	f.st.beforeTx = func() { f.setVersion(t, store.KindDevice, "d1", latest) }
	_, err := f.m.Start(t.Context(), "u1", store.KindDevice, "d1")
	if !errors.Is(err, updates.ErrUnavailable) {
		t.Fatalf("start = %v, want ErrUnavailable", err)
	}
	if _, ok := f.record(t, "u1", "m1"); ok || f.sent() != "" || len(f.timers(t)) != 0 {
		t.Fatalf("recorded or sent: sent %q, timers %v", f.sent(), f.timers(t))
	}
}

// TestStartRacesDeletion: a role deleted between the press's reads and
// its write is never chosen: the daemon chosen at first is deleted, so the
// pressed worker runs it; the pressed worker deleted refuses the press.
func TestStartRacesDeletion(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     store.PrincipalKind
		id       string
		want     error
		wantSent string
	}{
		{"the executor", store.KindDevice, "d1", nil, "w1:"},
		{"the pressed worker", store.KindWorker, "w1", updates.ErrNotFound, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.daemons.online["d1"], f.workers.updatable["w1"] = true, true
			f.st.beforeTx = func() {
				_, _, err := auth.RevokeByID(context.Background(), f.faulty, tc.kind, "u1", tc.id, f.now)
				must(t, err)
			}
			u, err := f.m.Start(t.Context(), "u1", store.KindWorker, "w1")
			if !errors.Is(err, tc.want) {
				t.Fatalf("start = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				if _, ok := f.record(t, "u1", "m1"); ok || f.sent() != "" || len(f.timers(t)) != 0 {
					t.Fatalf("recorded or sent: sent %q, timers %v", f.sent(), f.timers(t))
				}
				return
			}
			if f.sent() != tc.wantSent+u.UpdateID {
				t.Fatalf("sent %q, want %q", f.sent(), tc.wantSent+u.UpdateID)
			}
		})
	}
}

// TestStartRechoosesExecutor: the daemon, chosen first as below the
// target, reconnects at the target before the press commits: the worker,
// still below, runs the update instead.
func TestStartRechoosesExecutor(t *testing.T) {
	f := newFixture(t)
	f.daemons.online["d1"], f.workers.updatable["w1"] = true, true
	f.st.beforeTx = func() { f.setVersion(t, store.KindDevice, "d1", latest) }
	u, err := f.m.Start(t.Context(), "u1", store.KindWorker, "w1")
	must(t, err)
	if f.sent() != "w1:"+u.UpdateID {
		t.Fatalf("sent %q, want the worker", f.sent())
	}
}

// TestStartConcurrent: simultaneous presses start one update.
func TestStartConcurrent(t *testing.T) {
	f := newFixture(t)
	f.daemons.online["d1"], f.workers.updatable["w1"] = true, true
	var wg sync.WaitGroup
	ids := make([]string, 4)
	for i := range ids {
		wg.Go(func() {
			kind, id := store.KindDevice, "d1"
			if i%2 == 1 {
				kind, id = store.KindWorker, "w1"
			}
			u, err := f.m.Start(context.Background(), "u1", kind, id)
			if err != nil {
				t.Error(err)
			}
			ids[i] = u.UpdateID
		})
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("attempts %v, want one", ids)
		}
	}
	if len(f.daemons.sent)+len(f.workers.sent) != 1 {
		t.Fatalf("sent %q, want once", f.sent())
	}
}

// TestCompletesOnReconnect: a role of the machine connecting with a
// version at least the target removes the update and its timer.
func TestCompletesOnReconnect(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.daemons.online["d1"] = true
	_, err := f.m.Start(ctx, "u1", store.KindDevice, "d1")
	must(t, err)
	must(t, f.m.Connected(ctx, store.KindDevice, "d1", "v1.4.0"))
	must(t, f.m.Connected(ctx, store.KindWorker, "w1", "0.0.0"))
	must(t, f.m.Connected(ctx, store.KindDevice, "x1", "v2.0.0"))
	must(t, f.m.Connected(ctx, store.KindDevice, "d2", "v2.0.0"))
	if _, ok := f.record(t, "u1", "m1"); !ok {
		t.Fatal("update removed early")
	}
	must(t, f.m.Connected(ctx, store.KindWorker, "w1", latest))
	if _, ok := f.record(t, "u1", "m1"); ok {
		t.Fatal("update kept after the machine connected with the target")
	}
	if ts := f.timers(t); len(ts) != 0 {
		t.Fatalf("timers = %+v", ts)
	}
}

// TestFailureReports: the executor's report fails the running attempt,
// cleaned and bounded; a report naming another machine's running attempt
// or an older attempt changes nothing; a store failure is returned.
func TestFailureReports(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.daemons.online["d1"], f.daemons.online["d3"] = true, true
	m1, err := f.m.Start(ctx, "u1", store.KindDevice, "d1")
	must(t, err)
	m3, err := f.m.Start(ctx, "u1", store.KindDevice, "d3")
	must(t, err)
	// d1 reports m3's real, running attempt: not its machine's.
	must(t, f.m.Failed(ctx, store.KindDevice, "d1", m3.UpdateID, "nope"))
	must(t, f.m.Failed(ctx, store.KindDevice, "d1", "unknown", "nope"))
	must(t, f.m.Failed(ctx, store.KindDevice, "missing", m1.UpdateID, "nope"))
	for machine, want := range map[string]store.MachineUpdate{"m1": m1, "m3": m3} {
		if u, _ := f.record(t, "u1", machine); u != want {
			t.Fatalf("%s = %+v, want it unchanged", machine, u)
		}
	}

	// A store failure is not swallowed.
	f.faulty.FailAllWrites(storetest.ErrInjected)
	if err := f.m.Failed(ctx, store.KindDevice, "d1", m1.UpdateID, "boom"); !errors.Is(err, storetest.ErrInjected) {
		t.Fatalf("Failed with the store failing = %v", err)
	}
	f.faulty.FailAllWrites(nil)

	long := strings.Repeat("noise\n", 1000) + "\x1b[31merror:\x1b[0m disk full\x00"
	must(t, f.m.Failed(ctx, store.KindDevice, "d1", m1.UpdateID, long))
	u, _ := f.record(t, "u1", "m1")
	if u.State != store.MachineUpdateFailed || u.Failure != store.UpdateFailedCommand || len(u.Error) > updates.MaxErrorBytes ||
		!strings.HasSuffix(u.Error, "error: disk full") || strings.ContainsAny(u.Error, "\x1b\x00") {
		t.Fatalf("failed update = %+v", u)
	}
	if ts := f.timers(t); len(ts) != 1 || ts[0].SubjectID != m3.UpdateID {
		t.Fatalf("timers = %+v, want only m3's", ts)
	}

	// Retry: a new attempt; the first one's late report changes nothing.
	f.now = f.now.Add(time.Minute)
	retry, err := f.m.Start(ctx, "u1", store.KindDevice, "d1")
	must(t, err)
	if retry.UpdateID == m1.UpdateID || retry.State != store.MachineUpdateRunning || retry.Error != "" || retry.Failure != "" {
		t.Fatalf("retry = %+v", retry)
	}
	must(t, f.m.Failed(ctx, store.KindDevice, "d1", m1.UpdateID, "late"))
	if u, _ := f.record(t, "u1", "m1"); u != retry {
		t.Fatalf("update = %+v, want %+v", u, retry)
	}
}

// claim claims the machine update timers due at at.
func (f *fixture) claim(t *testing.T, at int64) []store.Timer {
	t.Helper()
	claims, err := f.st.ClaimDueTimers(t.Context(), at, "cp", 5000, 10)
	must(t, err)
	return claims
}

// TestDeadline: the deadline fails a running update as timed out; a claim
// whose lease another claim has taken over does nothing.
func TestDeadline(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.workers.updatable["w1"] = true
	u, err := f.m.Start(ctx, "u1", store.KindWorker, "w1")
	must(t, err)
	if claims := f.claim(t, u.DeadlineAt-1); len(claims) != 0 {
		t.Fatalf("claimed before the deadline: %+v", claims)
	}
	stale := f.claim(t, u.DeadlineAt)
	// Its lease (5 s) ran out before it was handled: claimed again.
	current := f.claim(t, u.DeadlineAt+6000)
	if len(stale) != 1 || len(current) != 1 || stale[0].Token == current[0].Token {
		t.Fatalf("claims %+v, %+v", stale, current)
	}
	must(t, f.m.HandleTimer(ctx, stale[0]))
	if got, _ := f.record(t, "u1", "m1"); got != u {
		t.Fatalf("a stale claim changed the update: %+v", got)
	}
	must(t, f.m.HandleTimer(ctx, current[0]))
	got, _ := f.record(t, "u1", "m1")
	if got.State != store.MachineUpdateFailed || got.Failure != store.UpdateTimedOut || got.Error != "" {
		t.Fatalf("update = %+v", got)
	}
	// A completion after the timeout still clears it.
	must(t, f.m.Connected(ctx, store.KindDevice, "d1", "v1.6.0"))
	if _, ok := f.record(t, "u1", "m1"); ok {
		t.Fatal("failed update kept after the machine connected with a newer version")
	}
}

// TestDeadlineFindsTarget: every role already at the target when the
// deadline passes (their completions were lost) completes the update.
func TestDeadlineFindsTarget(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.daemons.online["d1"] = true
	u, err := f.m.Start(ctx, "u1", store.KindDevice, "d1")
	must(t, err)
	// The Hellos' metadata committed; their completions did not.
	f.setVersion(t, store.KindDevice, "d1", latest)
	f.setVersion(t, store.KindWorker, "w1", latest)
	claims := f.claim(t, u.DeadlineAt)
	if len(claims) != 1 {
		t.Fatalf("claims = %+v", claims)
	}
	must(t, f.m.HandleTimer(ctx, claims[0]))
	if got, ok := f.record(t, "u1", "m1"); ok {
		t.Fatalf("update = %+v, want it completed", got)
	}
}

// TestDeadlineRoleStillBehind: the daemon was at the target before the
// attempt; the worker's command was never delivered, so the worker is
// still behind at the deadline: the update timed out.
func TestDeadlineRoleStillBehind(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.daemons.online["d1"], f.workers.updatable["w1"] = true, true
	f.setVersion(t, store.KindDevice, "d1", latest)
	u, err := f.m.Start(ctx, "u1", store.KindWorker, "w1")
	must(t, err)
	claims := f.claim(t, u.DeadlineAt)
	if len(claims) != 1 {
		t.Fatalf("claims = %+v", claims)
	}
	must(t, f.m.HandleTimer(ctx, claims[0]))
	got, _ := f.record(t, "u1", "m1")
	if got.State != store.MachineUpdateFailed || got.Failure != store.UpdateTimedOut {
		t.Fatalf("update = %+v, want timed out", got)
	}
}

// TestFailureAfterLostCompletion: every role already reports the target
// (their completions were lost) when a failure report arrives: the update
// is done, not failed.
func TestFailureAfterLostCompletion(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.daemons.online["d1"] = true
	u, err := f.m.Start(ctx, "u1", store.KindDevice, "d1")
	must(t, err)
	f.setVersion(t, store.KindDevice, "d1", latest)
	f.setVersion(t, store.KindWorker, "w1", latest)
	must(t, f.m.Failed(ctx, store.KindDevice, "d1", u.UpdateID, "restart: exit status 1"))
	if got, ok := f.record(t, "u1", "m1"); ok {
		t.Fatalf("update = %+v, want it done", got)
	}
	if ts := f.timers(t); len(ts) != 0 {
		t.Fatalf("timers = %+v", ts)
	}
}

// TestLongMachineID: a machine id as long as enrollment allows (255
// bytes) enrolls, is updated, and its update goes with its revocation; an
// identity with a longer machine id (not enrollable, but storable) is
// revoked without trouble.
func TestLongMachineID(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	now := time.Now()
	machine := strings.Repeat("m", 255)
	tok, _, err := auth.CreateEnrollToken(ctx, f.st, "u1", store.KindDevice, time.Hour, now)
	must(t, err)
	d, err := auth.Enroll(ctx, f.st, auth.EnrollRequest{Token: tok, Kind: store.KindDevice, Name: "long", MachineID: machine, ClientVersion: "v1.4.0"}, now)
	must(t, err)
	f.daemons.online[d.Identity.ID] = true
	if _, err := f.m.Start(ctx, "u1", store.KindDevice, d.Identity.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.record(t, "u1", machine); !ok {
		t.Fatal("no update recorded")
	}
	_, _, err = auth.RevokeByID(ctx, f.st, store.KindDevice, "u1", d.Identity.ID, now)
	must(t, err)
	if _, ok := f.record(t, "u1", machine); ok {
		t.Fatal("update kept after revoking the machine's last identity")
	}
	longer := store.Identity{Kind: store.KindDevice, ID: "longer", UserID: "u1", Name: "longer", MachineID: machine + "m", CreatedAt: 1}
	must(t, f.st.CreateIdentity(ctx, longer))
	if _, _, err := auth.RevokeByID(ctx, f.st, store.KindDevice, "u1", "longer", now); err != nil {
		t.Fatalf("revoking an identity with a %d-byte machine id: %v", len(longer.MachineID), err)
	}
}

// TestSendFailure: a command that cannot be sent fails the attempt.
func TestSendFailure(t *testing.T) {
	f := newFixture(t)
	f.daemons.online["d1"] = true
	f.daemons.failure = errors.New("link closed")
	u, err := f.m.Start(t.Context(), "u1", store.KindDevice, "d1")
	must(t, err)
	if u.State != store.MachineUpdateFailed || u.Failure != store.UpdateUnreachable || u.Error != "link closed" {
		t.Fatalf("update = %+v", u)
	}
	if ts := f.timers(t); len(ts) != 0 {
		t.Fatalf("timers = %+v", ts)
	}
}

// TestRemovedWithLastIdentity: the update goes in the transaction that
// revokes the machine's last identity (the Web delete and the CLI's
// revoke); a rejoin on the same machine keeps it, one from another
// machine leaves it.
func TestRemovedWithLastIdentity(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	f.daemons.online["d1"], f.daemons.online["d3"] = true, true
	_, err := f.m.Start(ctx, "u1", store.KindDevice, "d1")
	must(t, err)
	_, err = f.m.Start(ctx, "u1", store.KindDevice, "d3")
	must(t, err)
	now := time.Now()
	_, _, err = auth.RevokeByID(ctx, f.st, store.KindDevice, "u1", "d1", now)
	must(t, err)
	if _, ok := f.record(t, "u1", "m1"); !ok {
		t.Fatal("update removed while the worker remains")
	}
	_, _, err = auth.RevokeByName(ctx, f.st, store.KindWorker, "u1", "laptop", now)
	must(t, err)
	if _, ok := f.record(t, "u1", "m1"); ok {
		t.Fatal("update kept after the last identity was revoked")
	}

	rejoin := func(machine string) {
		t.Helper()
		tok, _, err := auth.CreateRejoinToken(ctx, f.st, "u1", store.KindDevice, "d3", time.Hour, now)
		must(t, err)
		res, err := auth.Enroll(ctx, f.st, auth.EnrollRequest{Token: tok, Kind: store.KindDevice, MachineID: machine, ClientVersion: "v1.4.0",
			Online: offline}, now)
		must(t, err)
		f.daemons.online[res.Identity.ID] = true
	}
	rejoin("m3")
	if _, ok := f.record(t, "u1", "m3"); !ok {
		t.Fatal("a rejoin on the same machine removed its update")
	}
	// Rejoin the replacement, now from another machine.
	ids, err := f.st.ListIdentities(ctx, store.KindDevice, "u1")
	must(t, err)
	for _, id := range ids {
		if id.MachineID == "m3" {
			tok, _, err := auth.CreateRejoinToken(ctx, f.st, "u1", store.KindDevice, id.ID, time.Hour, now)
			must(t, err)
			_, err = auth.Enroll(ctx, f.st, auth.EnrollRequest{Token: tok, Kind: store.KindDevice, MachineID: "m9", ClientVersion: "v1.4.0",
				Online: offline}, now)
			must(t, err)
		}
	}
	if _, ok := f.record(t, "u1", "m3"); ok {
		t.Fatal("a rejoin from another machine left the old machine's update")
	}
}

// nopStore satisfies the Manager's required store for tests that only
// check releases.
type nopStore struct{ store.Store }

func offline(context.Context, store.PrincipalKind, string, string) (bool, error) { return false, nil }
