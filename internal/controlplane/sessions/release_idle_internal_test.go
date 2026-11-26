package sessions

import (
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// releaseCase builds a service with worker w1's current boot b1, an
// attached connection c1 of it and session s's entry holding the lease at
// epoch 3 under provider p, bound to c1; mutate changes it before a check.
func releaseCase(mutate func(s *Service, e *entry, c *Conn)) (*Service, *entry, *Conn, store.IdleLease) {
	c := &Conn{id: "c1", workerID: "w1", bootID: "b1", attached: true}
	e := &entry{user: "u1", holder: holder{conn: "c1", worker: "w1", boot: "b1", epoch: 3, provider: "p"}, bound: c}
	s := &Service{entries: map[string]*entry{"s": e}, conns: map[string]*Conn{"c1": c}, boots: map[string]bootRec{"w1": {boot: "b1"}}}
	l := store.IdleLease{Lease: store.Lease{SessionID: "s", Epoch: 3, WorkerID: "w1", BootID: "b1", ConnID: "c1", ProviderID: "p"}}
	if mutate != nil {
		mutate(s, e, c)
	}
	return s, e, c, l
}

// TestAskable: a lease the store found idle gets a ReleaseIdle only while
// the entry still names it and it is bound to a live, attached connection
// of the current boot, delivered, unreserved, without pending reasons and
// without a request outstanding; the same checks, where they apply, hold
// when the request is about to be sent.
func TestAskable(t *testing.T) {
	const now = 1_000_000
	cases := []struct {
		name   string
		mutate func(s *Service, e *entry, c *Conn)
		ok     bool
	}{
		{"eligible", nil, true},
		{"another epoch", func(_ *Service, e *entry, _ *Conn) { e.holder.epoch = 4 }, false},
		{"another worker", func(_ *Service, e *entry, _ *Conn) { e.holder.worker = "w2" }, false},
		{"another boot", func(_ *Service, e *entry, _ *Conn) { e.holder.boot = "b0" }, false},
		{"another provider", func(_ *Service, e *entry, _ *Conn) { e.holder.provider = "q" }, false},
		{"unbound", func(_ *Service, e *entry, _ *Conn) { e.bound = nil }, false},
		{"bound elsewhere", func(_ *Service, _ *entry, c *Conn) { c.id = "c2" }, false},
		{"closed", func(_ *Service, _ *entry, c *Conn) { c.closed = true }, false},
		{"not attached", func(_ *Service, _ *entry, c *Conn) { c.attached = false }, false},
		{"stale boot", func(s *Service, _ *entry, _ *Conn) { s.boots["w1"] = bootRec{boot: "b2"} }, false},
		{"reason pending", func(_ *Service, e *entry, _ *Conn) { e.reasons = reasonJob }, false},
		{"reply not delivered", func(_ *Service, e *entry, _ *Conn) { e.pending = true }, false},
		{"reply abandoned", func(_ *Service, e *entry, _ *Conn) { e.abandoned = true }, false},
		{"reserved", func(_ *Service, e *entry, _ *Conn) { e.reserved = 1 }, false},
		{"asked", func(_ *Service, e *entry, _ *Conn) { e.askedAt = now - 1 }, false},
		{"asked long ago", func(_ *Service, e *entry, _ *Conn) { e.askedAt = now - ReleaseIdleTimeout.Milliseconds() }, true},
	}
	for _, tc := range cases {
		s, e, _, l := releaseCase(tc.mutate)
		if got := s.askable(e, l, "p", now); got != tc.ok {
			t.Errorf("%s: askable = %v, want %v", tc.name, got, tc.ok)
		}
	}

	// At send time: the request goes out only for the bound connection,
	// the same epoch, a delivered reply and no pending reason.
	send := []struct {
		name   string
		mutate func(s *Service, e *entry, c *Conn)
		epoch  int64
		ok     bool
	}{
		{"eligible", nil, 3, true},
		{"older epoch", nil, 2, false},
		{"reason raised meanwhile", func(_ *Service, e *entry, _ *Conn) { e.reasons = reasonInput }, 3, false},
		{"reply not delivered", func(_ *Service, e *entry, _ *Conn) { e.pending = true }, 3, false},
		{"reply abandoned", func(_ *Service, e *entry, _ *Conn) { e.abandoned = true }, 3, false},
		{"rebound", func(_ *Service, e *entry, _ *Conn) { e.bound = &Conn{id: "c2"} }, 3, false},
	}
	for _, tc := range send {
		s, _, c, _ := releaseCase(tc.mutate)
		events := s.hintEvents(c, "s", &hint{release: tc.epoch})
		got := len(events) == 1 && events[0].GetReleaseIdle().GetEpoch() == uint64(tc.epoch) && events[0].GetReleaseIdle().GetSessionId() == "s"
		if got != tc.ok || (!tc.ok && len(events) != 0) {
			t.Errorf("%s: events %v, want a request: %v", tc.name, events, tc.ok)
		}
	}
}

// TestRunnable: a waiting session counts as demand only while a live,
// attached connection of its user and of its worker's current boot may run
// it (pin and local mode honoured, the session's agent installed and
// wanted there) and the provider is assigned to that worker.
func TestRunnable(t *testing.T) {
	allowed := gate{provider: "p", workers: map[string]bool{"w1": true}}
	cases := []struct {
		name   string
		mutate func(s *Service, e *entry, c *Conn)
		g      gate
		ok     bool
	}{
		{"runnable", nil, allowed, true},
		{"worker not assigned", nil, gate{provider: "p", workers: map[string]bool{"w2": true}}, false},
		{"not attached", func(_ *Service, _ *entry, c *Conn) { c.attached = false }, allowed, false},
		{"closed", func(_ *Service, _ *entry, c *Conn) { c.closed = true }, allowed, false},
		{"stale boot", func(s *Service, _ *entry, _ *Conn) { s.boots["w1"] = bootRec{boot: "b2"} }, allowed, false},
		{"another user", func(_ *Service, e *entry, _ *Conn) { e.user = "u2" }, allowed, false},
		{"pinned elsewhere", func(_ *Service, e *entry, _ *Conn) { e.pin = "w2" }, allowed, false},
		{"pinned here", func(_ *Service, e *entry, _ *Conn) { e.pin = "w1" }, allowed, true},
		{"local-only connection, session not local", func(_ *Service, _ *entry, c *Conn) { c.localOnly = true }, allowed, false},
		{"agent not installed", func(s *Service, _ *entry, _ *Conn) { s.agents["w1"] = agentView{} }, allowed, false},
		{"agent not wanted", func(s *Service, _ *entry, _ *Conn) {
			v := s.agents["w1"]
			v.agents.Agents[0].Wanted = false
			s.agents["w1"] = v
		}, allowed, false},
	}
	for _, tc := range cases {
		c := &Conn{id: "c1", workerID: "w1", bootID: "b1", userID: "u1", attached: true}
		installed := store.AgentStatus{Agent: store.AgentClaudeCode, State: store.AgentInstalled, InstalledVersion: "1", TargetVersion: "1"}
		s := &Service{conns: map[string]*Conn{"c1": c}, boots: map[string]bootRec{"w1": {boot: "b1"}},
			agents: map[string]agentView{"w1": {agents: store.WorkerAgents{WorkerID: "w1", Agents: []store.WorkerAgent{{
				WorkerID: "w1", Agent: store.AgentClaudeCode, Status: installed, Wished: true, Wanted: true, WantedVersion: "1"}}}}}}
		e := &entry{user: "u1", agent: store.AgentClaudeCode}
		if tc.mutate != nil {
			tc.mutate(s, e, c)
		}
		if got := s.runnable(e, tc.g); got != tc.ok {
			t.Errorf("%s: runnable = %v, want %v", tc.name, got, tc.ok)
		}
	}
}

// TestDetachVoidsRequests: when a connection's hint stream ends, the
// ReleaseIdle requests queued for its leases no longer count.
func TestDetachVoidsRequests(t *testing.T) {
	s, e, c, _ := releaseCase(func(_ *Service, e *entry, _ *Conn) { e.askedAt = 1 })
	other := &entry{holder: holder{epoch: 1}, bound: &Conn{id: "c2"}, askedAt: 1}
	s.entries["o"] = other
	c.s = s
	c.detach()
	if e.askedAt != 0 || other.askedAt != 1 {
		t.Fatalf("after detach: asked %d, other connection's %d", e.askedAt, other.askedAt)
	}
}
