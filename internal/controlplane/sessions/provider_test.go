package sessions_test

import (
	"bytes"
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/providers"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
)

var testKey = bytes.Repeat([]byte{3}, providers.KeyBytes)

// withSecrets gives the session service a provider key.
func withSecrets(box *providers.Box) option {
	return func(c *sessions.Config) { c.Secrets = box }
}

func newBox(t *testing.T, key []byte) *providers.Box {
	t.Helper()
	b, err := providers.NewBox(key)
	must(t, err)
	return b
}

// provider stores a provider of the fixture's user assigned to the given
// worker indexes, sealed with box.
func (f *fixture) provider(box *providers.Box, id string, maxSessions int, workers ...int) store.Provider {
	f.t.Helper()
	sealed, err := box.Seal(id, providers.Secrets{Key: "sk-fake-" + id, Env: []providers.EnvValue{{Name: "FAKE_VAR", Value: "fake-value-" + id}}})
	must(f.t, err)
	p := store.Provider{ID: id, UserID: f.user.ID, Name: id, Kind: store.ProviderKindClaudeCode, Scheme: store.ProviderSchemeBearer,
		BaseURL: "https://gw.example", SonnetModel: "sonnet-fake", Secrets: sealed, KeyLast4: "last", EnvNames: []string{"FAKE_VAR"},
		EnvLast4: []string{"last"}, MaxSessions: maxSessions, CreatedAt: ms(f), UpdatedAt: ms(f)}
	for _, w := range workers {
		p.WorkerIDs = append(p.WorkerIDs, f.workers[w].Identity.ID)
	}
	got, err := f.st.CreateProvider(f.t.Context(), p)
	must(f.t, err)
	return got
}

// assign replaces the provider's workers and cap, then tells the
// scheduler, as UpdateProvider does.
func (f *fixture) assign(p store.Provider, maxSessions int, workers ...int) store.Provider {
	f.t.Helper()
	p.WorkerIDs, p.MaxSessions = nil, maxSessions
	for _, w := range workers {
		p.WorkerIDs = append(p.WorkerIDs, f.workers[w].Identity.ID)
	}
	p.UpdatedAt = ms(f)
	got, err := f.st.UpdateProvider(f.t.Context(), f.user.ID, p, nil)
	must(f.t, err)
	f.svc.ProviderChanged(f.user.ID, p.ID)
	return got
}

// providerSession creates a session naming provider; pinned names a
// worker index or -1.
func (f *fixture) providerSession(id, provider string, pinned int) {
	f.t.Helper()
	s := store.Session{ID: id, ClientRequestID: id, UserID: f.user.ID, ProjectID: "p1", ClaudeSessionID: "claude-" + id, DefaultDeviceID: f.device.ID,
		Model: "m", PermissionMode: "auto", CreatedAt: ms(f), ProviderID: provider}
	if pinned >= 0 {
		s.PinnedWorkerID, s.Local = f.workers[pinned].Identity.ID, true
	}
	_, err := f.st.CreateSession(f.t.Context(), s, []store.SessionCwd{{DeviceID: f.device.ID, Cwd: "/work"}}, store.DefaultMaxSessionsPerUser)
	must(f.t, err)
}

// queueEvents returns the queue reasons the bus announced for session id,
// in order.
func (f *fixture) queueEvents(id string) []string {
	f.t.Helper()
	var out []string
	f.bus.mu.Lock()
	defer f.bus.mu.Unlock()
	for i, ev := range f.bus.ephemeral {
		if ev.GetKind() != circusv1.SessionEvent_KIND_QUEUE || f.bus.ephemeralIDs[i] != id {
			continue
		}
		var p struct {
			QueueReason string `json:"queue_reason"`
		}
		must(f.t, json.Unmarshal([]byte(ev.GetPayloadJson()), &p))
		if !ev.GetEphemeral() || ev.GetSeq() != 0 {
			f.t.Fatalf("queue event not ephemeral: %v", ev)
		}
		out = append(out, p.QueueReason)
	}
	return out
}

func (f *fixture) reason(id string) circusv1.QueueReason { return f.svc.QueueReason(id) }

func (f *fixture) waitReason(id string, want circusv1.QueueReason) {
	f.t.Helper()
	wait.For(f.t, func() bool { return f.reason(id) == want })
}

func wantEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("queue events %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("queue events %q, want %q", got, want)
		}
	}
}

// TestProviderOffersOnlyToAssignedWorkers: a session with a provider goes
// only to the provider's workers; a local session pinned to a worker the
// provider is not assigned to waits with NO_ASSIGNED_WORKER.
func TestProviderOffersOnlyToAssignedWorkers(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 0, 1)
	c0, c1 := f.dial(0, "b0"), f.dial(1, "b1")
	ev0, ev1 := f.attach(c0, 8), f.attach(c1, 8)
	f.providerSession("s", p.ID, -1)
	f.input("s", "1", store.InputNormal)
	offerTo(t, ev1, "s")
	noOffer(t, ev0)
	if f.reason("s") != 0 {
		t.Fatalf("offered session has reason %v", f.reason("s"))
	}
	// Worker 0 is refused even when it asks without an Offer.
	wantRejected(t, call(t, c0, wc.Acquire("s", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	acquire(t, c1, "s")

	f.providerSession("pinned", p.ID, 0)
	f.input("pinned", "1", store.InputNormal)
	f.waitReason("pinned", circusv1.QueueReason_QUEUE_REASON_NO_ASSIGNED_WORKER)
	noOffer(t, ev0)
	wantEvents(t, f.queueEvents("pinned"), "no_assigned_worker")
	// Assigning worker 0 lets it run.
	f.assign(p, 0, 0, 1)
	offerTo(t, ev0, "pinned")
	f.waitReason("pinned", 0)
	wantEvents(t, f.queueEvents("pinned"), "no_assigned_worker", "unspecified")
}

// TestProviderFullQueuesAndWakes: with max_sessions 1 the second session
// waits with PROVIDER_FULL (announced by a KIND_QUEUE event) and runs once
// the first releases its lease.
func TestProviderFullQueuesAndWakes(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 1, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.providerSession("a", p.ID, -1)
	f.input("a", "1", store.InputNormal)
	offerTo(t, ev0, "a")
	epoch := acquire(t, c0, "a")
	f.providerSession("b", p.ID, -1)
	f.input("b", "1", store.InputNormal)
	f.waitReason("b", circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL)
	noOffer(t, ev0)
	wantEvents(t, f.queueEvents("b"), "provider_full")
	// Without an Offer it cannot be taken either.
	wantRejected(t, call(t, c0, wc.Acquire("b", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	if l := f.lease("b"); l.Epoch != 0 {
		t.Fatalf("refused acquisition wrote %+v", l)
	}
	// The release wakes b.
	turn(t, c0, "a", epoch, 1)
	wantOk(t, call(t, c0, wc.Release("a", epoch, circusv1.ReleaseLease_REASON_IDLE)), false)
	offerTo(t, ev0, "b")
	acquire(t, c0, "b")
	f.waitReason("b", 0)
	wantEvents(t, f.queueEvents("b"), "provider_full", "unspecified")
}

// TestProviderFullRaceAtAcquire: two sessions offered while the provider
// had room; the second AcquireLease is refused PROVIDER_FULL, writes
// nothing, and the session waits with PROVIDER_FULL until a release.
func TestProviderFullRaceAtAcquire(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 0, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.providerSession("a", p.ID, -1)
	f.providerSession("b", p.ID, -1)
	f.input("a", "1", store.InputNormal)
	f.input("b", "1", store.InputNormal)
	offers(t, ev0, "a", "b")
	// Lower the cap behind the offers' back (no ProviderChanged).
	p.MaxSessions, p.UpdatedAt = 1, ms(f)
	_, err := f.st.UpdateProvider(t.Context(), f.user.ID, p, nil)
	must(t, err)
	epoch := acquire(t, c0, "a")
	wantRejected(t, call(t, c0, wc.Acquire("b", true)), circusv1.SessionReply_Rejected_CODE_PROVIDER_FULL)
	f.waitReason("b", circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL)
	if l := f.lease("b"); l.Epoch != 0 || f.get("b").State != store.SessionQueued {
		t.Fatalf("refused acquisition wrote %+v", l)
	}
	turn(t, c0, "a", epoch, 1)
	wantOk(t, call(t, c0, wc.Release("a", epoch, circusv1.ReleaseLease_REASON_IDLE)), false)
	offerTo(t, ev0, "b")
	acquire(t, c0, "b")
}

// TestGetLaunchEnv: fenced like every session op; local login without a
// provider; the decrypted variables with one; PROVIDER_UNAVAILABLE when
// unassigned; STOPPED after a stop.
func TestGetLaunchEnv(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 0, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.session("local", -1)
	f.input("local", "1", store.InputNormal)
	offerTo(t, ev0, "local")
	le := acquire(t, c0, "local")
	if r := call(t, c0, wc.LaunchEnv("local", le)); !r.GetLaunchEnv().GetLocalLogin() || len(r.GetLaunchEnv().GetEnv()) != 0 || r.GetLaunchEnv().GetProviderId() != "" {
		t.Fatalf("local login = %v", r)
	}
	wantFenced(t, call(t, c0, wc.LaunchEnv("local", le+1)), le)

	f.providerSession("s", p.ID, -1)
	f.input("s", "1", store.InputNormal)
	offerTo(t, ev0, "s")
	epoch := acquire(t, c0, "s")
	r := call(t, c0, wc.LaunchEnv("s", epoch))
	var got []string
	for _, v := range r.GetLaunchEnv().GetEnv() {
		got = append(got, v.GetName()+"="+v.GetValue())
	}
	wantEvents(t, got, "ANTHROPIC_BASE_URL=https://gw.example", "ANTHROPIC_AUTH_TOKEN=sk-fake-prov",
		"ANTHROPIC_DEFAULT_SONNET_MODEL=sonnet-fake", "FAKE_VAR=fake-value-prov")
	if r.GetLaunchEnv().GetLocalLogin() || r.GetLaunchEnv().GetProviderId() != p.ID {
		t.Fatalf("launch env %v", r)
	}
	// Unassigning revokes nothing, but the next start is refused.
	f.assign(p, 0)
	f.settle()
	noEvent(t, ev0)
	if l := f.lease("s"); uint64(l.Epoch) != epoch || l.WorkerID == "" {
		t.Fatalf("unassigning touched the lease: %+v", l)
	}
	wantRejected(t, call(t, c0, wc.LaunchEnv("s", epoch)), circusv1.SessionReply_Rejected_CODE_PROVIDER_UNAVAILABLE)
	// The worker releases for its provider: the session queues again.
	wantOk(t, call(t, c0, wc.Release("s", epoch, circusv1.ReleaseLease_REASON_PROVIDER)), false)
	if st := f.get("s").State; st != store.SessionQueued {
		t.Fatalf("after REASON_PROVIDER the session is %s", st)
	}
	f.waitReason("s", circusv1.QueueReason_QUEUE_REASON_NO_ASSIGNED_WORKER)

	// STOPPED after a stop.
	_, _, _, err := f.st.StopSession(t.Context(), "local", ms(f), "user")
	must(t, err)
	wantRejected(t, call(t, c0, wc.LaunchEnv("local", le)), circusv1.SessionReply_Rejected_CODE_STOPPED)
}

// TestProviderUnreadableAndDeleted: unreadable secrets refuse the
// acquisition and GetLaunchEnv and queue the session with
// SECRETS_UNREADABLE; a deleted provider queues it with PROVIDER_DELETED.
func TestProviderUnreadableAndDeleted(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	other := newBox(t, bytes.Repeat([]byte{4}, providers.KeyBytes))
	p := f.provider(other, "unreadable", 0, 0) // sealed with another key
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.providerSession("s", p.ID, -1)
	f.input("s", "1", store.InputNormal)
	f.waitReason("s", circusv1.QueueReason_QUEUE_REASON_PROVIDER_SECRETS_UNREADABLE)
	noOffer(t, ev0)
	wantRejected(t, call(t, c0, wc.Acquire("s", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)

	// Re-entering the secrets lets it run.
	sealed, err := box.Seal(p.ID, providers.Secrets{Key: "k", Env: []providers.EnvValue{{Name: "FAKE_VAR", Value: "v"}}})
	must(t, err)
	p.Secrets, p.UpdatedAt = sealed, ms(f)
	_, err = f.st.UpdateProvider(t.Context(), f.user.ID, p, nil)
	must(t, err)
	f.svc.ProviderChanged(f.user.ID, p.ID)
	offerTo(t, ev0, "s")

	// Deleted: PROVIDER_DELETED, also for sessions the scheduler never saw.
	d := f.provider(box, "doomed", 0, 0)
	f.providerSession("d", d.ID, -1)
	_, err = f.st.SubmitInput(t.Context(), store.Input{InputID: "d-1", SessionID: "d", ClientMsgID: "1", Kind: store.InputUser, Text: "x", Priority: store.InputNormal, CreatedAt: ms(f)})
	must(t, err) // queued, no Wake
	must(t, f.st.DeleteProvider(t.Context(), f.user.ID, d.ID, ms(f)))
	f.svc.ProviderChanged(f.user.ID, d.ID)
	f.waitReason("d", circusv1.QueueReason_QUEUE_REASON_PROVIDER_DELETED)
	wantRejected(t, call(t, c0, wc.Acquire("d", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
}

// TestProviderChangeRevokesIdleLease: changing a session's provider while
// it holds an idle lease releases the lease in the store; the scheduler
// revokes it on the holder's connection and offers the session under the
// new provider.
func TestProviderChangeRevokesIdleLease(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	a := f.provider(box, "a", 0, 0)
	b := f.provider(box, "b", 0, 1)
	c0, c1 := f.dial(0, "b0"), f.dial(1, "b1")
	ev0, ev1 := f.attach(c0, 8), f.attach(c1, 8)
	f.providerSession("s", a.ID, -1)
	f.input("s", "1", store.InputNormal)
	offerTo(t, ev0, "s")
	epoch := acquire(t, c0, "s")
	turn(t, c0, "s", epoch, 1)
	_, err := f.st.SubmitInput(t.Context(), store.Input{InputID: "s-2", SessionID: "s", ClientMsgID: "2", Kind: store.InputUser, Text: "x", Priority: store.InputNormal, CreatedAt: ms(f)})
	must(t, err)
	set, err := f.st.SetSessionProvider(t.Context(), "s", b.ID, ms(f))
	must(t, err)
	if set.Loss == nil {
		t.Fatal("no lease released")
	}
	f.svc.Wake("s")
	for {
		e := next(t, ev0)
		if r := e.GetLeaseRevoked(); r != nil && r.GetSessionId() == "s" && r.GetEpoch() == epoch {
			break
		}
	}
	offerTo(t, ev1, "s")
	acquire(t, c1, "s")
}

// TestWorkerOfflineReason: a session whose only possible worker is
// offline waits with WORKER_OFFLINE; one waiting for capacity has no
// reason.
func TestWorkerOfflineReason(t *testing.T) {
	f := newFixture(t)
	f.localSession("s", 0)
	f.input("s", "1", store.InputNormal)
	f.waitReason("s", circusv1.QueueReason_QUEUE_REASON_WORKER_OFFLINE)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 1)
	offerTo(t, ev0, "s")
	f.waitReason("s", 0)
	acquire(t, c0, "s")
	f.localSession("t", 0)
	f.input("t", "1", store.InputNormal)
	f.settle()
	if r := f.reason("t"); r != 0 {
		t.Fatalf("session waiting for capacity has reason %v", r)
	}
}

// TestQueueReasonsGiveWayToLeases: entries that hold only a queue reason
// never keep a lease out of the full table.
func TestQueueReasonsGiveWayToLeases(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	sessions.DisableFallback(f.svc)
	p := f.provider(box, "prov", 0) // no worker
	for _, id := range []string{"q1", "q2"} {
		f.providerSession(id, p.ID, -1)
		f.input(id, "1", store.InputNormal)
		f.waitReason(id, circusv1.QueueReason_QUEUE_REASON_NO_ASSIGNED_WORKER)
	}
	sessions.SetMaxTracked(f.svc, sessions.Tracked(f.svc))
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.session("run", -1)
	f.input("run", "1", store.InputNormal)
	offerTo(t, ev0, "run")
	acquire(t, c0, "run")
	if n := sessions.Tracked(f.svc); n != 2 {
		t.Fatalf("tracked %d, want 2", n)
	}
	// The evicted reason did not clear, so no event says it did.
	for _, id := range []string{"q1", "q2"} {
		wantEvents(t, f.queueEvents(id), "no_assigned_worker")
	}
}

// TestAcquireRefusesUnreadableSecrets: secrets that stopped opening after
// the Offer refuse the acquisition with PROVIDER_UNAVAILABLE, write
// nothing, and the session waits with SECRETS_UNREADABLE.
func TestAcquireRefusesUnreadableSecrets(t *testing.T) {
	for _, fresh := range []bool{true, false} {
		t.Run(map[bool]string{true: "fresh", false: "not fresh"}[fresh], func(t *testing.T) {
			box := newBox(t, testKey)
			f := newFixture(t, withSecrets(box))
			p := f.provider(box, "prov", 0, 0)
			c0 := f.dial(0, "b0")
			ev0 := f.attach(c0, 8)
			f.providerSession("s", p.ID, -1)
			f.input("s", "1", store.InputNormal)
			offerTo(t, ev0, "s")
			// Resealed with another key behind the Offer's back.
			sealed, err := newBox(t, bytes.Repeat([]byte{5}, providers.KeyBytes)).Seal(p.ID, providers.Secrets{Key: "k", Env: []providers.EnvValue{{Name: "FAKE_VAR"}}})
			must(t, err)
			p.Secrets, p.UpdatedAt = sealed, ms(f)
			_, err = f.st.UpdateProvider(t.Context(), f.user.ID, p, nil)
			must(t, err)
			wantRejected(t, call(t, c0, wc.Acquire("s", fresh)), circusv1.SessionReply_Rejected_CODE_PROVIDER_UNAVAILABLE)
			if l := f.lease("s"); l.Epoch != 0 || f.get("s").State != store.SessionQueued {
				t.Fatalf("refused acquisition wrote %+v", l)
			}
			f.waitReason("s", circusv1.QueueReason_QUEUE_REASON_PROVIDER_SECRETS_UNREADABLE)
		})
	}
}

// hideProvider is a store whose transactions no longer find one provider,
// as if it had been deleted under a live lease.
type hideProvider struct {
	store.Store
	id atomic.Value // string
}

func (h *hideProvider) Tx(ctx context.Context, fn func(ctx context.Context, tx store.Ops) error) error {
	return h.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error { return fn(ctx, hiddenOps{tx, h}) })
}

type hiddenOps struct {
	store.Ops
	h *hideProvider
}

func (o hiddenOps) GetProvider(ctx context.Context, userID, id string, now int64) (store.Provider, error) {
	if hidden, _ := o.h.id.Load().(string); hidden == id {
		return store.Provider{}, store.ErrNotFound
	}
	return o.Ops.GetProvider(ctx, userID, id, now)
}

// TestLaunchEnvDeletedProvider: a provider gone under a live lease is
// PROVIDER_UNAVAILABLE, never local login.
func TestLaunchEnvDeletedProvider(t *testing.T) {
	box := newBox(t, testKey)
	hide := &hideProvider{}
	f := newFixture(t, withSecrets(box), func(c *sessions.Config) {
		hide.Store = c.Store
		c.Store = hide
	})
	p := f.provider(box, "prov", 0, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.providerSession("s", p.ID, -1)
	f.input("s", "1", store.InputNormal)
	offerTo(t, ev0, "s")
	epoch := acquire(t, c0, "s")
	hide.id.Store(p.ID)
	wantRejected(t, call(t, c0, wc.LaunchEnv("s", epoch)), circusv1.SessionReply_Rejected_CODE_PROVIDER_UNAVAILABLE)
}

// TestProviderReadRacingRelease: an evaluation reads the provider full,
// and the lease that filled it ends before the evaluation records
// PROVIDER_FULL (so the release woke nobody); the provider counter makes
// the evaluation run again, and the session is offered.
func TestProviderReadRacingRelease(t *testing.T) {
	box := newBox(t, testKey)
	ps := newPauseStore()
	f := newFixture(t, withSecrets(box), withPause(ps))
	sessions.DisableFallback(f.svc)
	p := f.provider(box, "prov", 1, 0)
	c0 := f.dial(0, "b0")
	ev0 := f.attach(c0, 8)
	f.providerSession("a", p.ID, -1)
	f.input("a", "1", store.InputNormal)
	offerTo(t, ev0, "a")
	epoch := acquire(t, c0, "a")
	f.providerSession("b", p.ID, -1)
	f.settle()
	ps.pauseProvider()
	f.input("b", "1", store.InputNormal)
	wait.Recv(t, ps.entered) // b's evaluation read the provider full
	turn(t, c0, "a", epoch, 1)
	wantOk(t, call(t, c0, wc.Release("a", epoch, circusv1.ReleaseLease_REASON_IDLE)), false)
	ps.resume <- struct{}{}
	offerTo(t, ev0, "b")
}

// TestProviderScanFindsUntracked: ProviderChanged evaluates queued
// sessions naming the provider that the scheduler never saw, with no
// worker attached (so no rescan reaches them): after a deletion they wait
// with PROVIDER_DELETED, after their secrets became unreadable with
// SECRETS_UNREADABLE.
func TestProviderScanFindsUntracked(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	sessions.DisableFallback(f.svc)
	queueSilently := func(id, provider string) {
		f.providerSession(id, provider, -1)
		_, err := f.st.SubmitInput(t.Context(), store.Input{InputID: id + "-1", SessionID: id, ClientMsgID: "1", Kind: store.InputUser, Text: "x", Priority: store.InputNormal, CreatedAt: ms(f)})
		must(t, err)
	}
	doomed := f.provider(box, "doomed", 0, 0)
	queueSilently("d", doomed.ID)
	must(t, f.st.DeleteProvider(t.Context(), f.user.ID, doomed.ID, ms(f)))
	f.svc.ProviderChanged(f.user.ID, doomed.ID)
	f.waitReason("d", circusv1.QueueReason_QUEUE_REASON_PROVIDER_DELETED)

	p := f.provider(box, "resealed", 0, 0)
	queueSilently("u", p.ID)
	sealed, err := newBox(t, bytes.Repeat([]byte{6}, providers.KeyBytes)).Seal(p.ID, providers.Secrets{Key: "k", Env: []providers.EnvValue{{Name: "FAKE_VAR"}}})
	must(t, err)
	p.Secrets, p.UpdatedAt = sealed, ms(f)
	_, err = f.st.UpdateProvider(t.Context(), f.user.ID, p, nil)
	must(t, err)
	f.svc.ProviderChanged(f.user.ID, p.ID)
	f.waitReason("u", circusv1.QueueReason_QUEUE_REASON_PROVIDER_SECRETS_UNREADABLE)
}

// TestFallbackRederivesStaleReasons: a reason no event re-derived for
// ReasonRecheck is evaluated again by the fallback, so one whose session
// left the queue unseen clears; a fresher one is left alone.
func TestFallbackRederivesStaleReasons(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	p := f.provider(box, "prov", 0) // no worker
	f.providerSession("s", p.ID, -1)
	f.input("s", "1", store.InputNormal)
	f.waitReason("s", circusv1.QueueReason_QUEUE_REASON_NO_ASSIGNED_WORKER)
	// The session leaves the queue without a Wake.
	_, _, _, err := f.st.StopSession(t.Context(), "s", ms(f), "user")
	must(t, err)
	f.clk.Advance(sessions.ReasonRecheck - time.Second)
	sessions.Fallback(f.svc, t.Context())
	f.settle()
	if r := f.reason("s"); r != circusv1.QueueReason_QUEUE_REASON_NO_ASSIGNED_WORKER {
		t.Fatalf("a fresh reason was evaluated again: %v", r)
	}
	f.clk.Advance(time.Second)
	sessions.Fallback(f.svc, t.Context())
	f.waitReason("s", 0)
}

// TestProviderChangeMovesOffer: an Offer to a worker the session's new
// provider is not assigned to is withdrawn and the session offered to the
// new provider's worker.
func TestProviderChangeMovesOffer(t *testing.T) {
	box := newBox(t, testKey)
	f := newFixture(t, withSecrets(box))
	sessions.DisableFallback(f.svc)
	a := f.provider(box, "a", 0, 0)
	b := f.provider(box, "b", 0, 1)
	c0, c1 := f.dial(0, "b0"), f.dial(1, "b1")
	ev0, ev1 := f.attach(c0, 8), f.attach(c1, 8)
	f.providerSession("s", a.ID, -1)
	f.input("s", "1", store.InputNormal)
	offerTo(t, ev0, "s")
	set, err := f.st.SetSessionProvider(t.Context(), "s", b.ID, ms(f))
	must(t, err)
	if !set.Changed || set.Loss != nil {
		t.Fatalf("change %+v", set)
	}
	f.svc.Wake("s")
	offerTo(t, ev1, "s")
	wantRejected(t, call(t, c0, wc.Acquire("s", true)), circusv1.SessionReply_Rejected_CODE_NOT_OFFERED)
	acquire(t, c1, "s")
}
