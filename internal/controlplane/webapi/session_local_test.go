package webapi_test

import (
	"testing"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
	"google.golang.org/protobuf/proto"
)

// localWorker is a worker identity a local-mode test starts with.
type localWorker struct {
	id, machine string
	// connectedAt is the last connection (0: never connected).
	connectedAt int64
	deleted     bool
	revoked     bool
	other       bool // a worker of another user
}

// addWorker creates w, with a credential, as a worker of the super user
// (or of user "other").
func (h *harness) addWorker(t *testing.T, w localWorker) {
	t.Helper()
	ctx := t.Context()
	user := h.owner.ID
	if w.other {
		user = "other"
		if _, err := h.st.GetUser(ctx, user); err != nil {
			if err := h.st.CreateUser(ctx, store.User{ID: user, Email: "other@example.com", Role: store.RoleUser, CreatedAt: h.clock.Load()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	now := h.clock.Load()
	if err := h.st.CreateIdentity(ctx, store.Identity{Kind: store.KindWorker, ID: w.id, UserID: user, Name: w.id, MachineID: w.machine, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateCredential(ctx, store.Credential{ID: "cred-" + w.id, Kind: store.KindWorker, PrincipalID: w.id, TokenHash: []byte("hash-" + w.id), CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if w.connectedAt != 0 {
		if _, err := h.st.SetConnected(ctx, store.KindWorker, w.id, "boot-"+w.id, w.connectedAt); err != nil {
			t.Fatal(err)
		}
	}
	if w.revoked {
		if _, err := h.st.RevokeCredentials(ctx, store.KindWorker, w.id, now); err != nil {
			t.Fatal(err)
		}
	}
	if w.deleted {
		if _, err := h.st.DeleteIdentity(ctx, store.KindWorker, w.id, now); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCreateSessionLocalMode: CreateSession makes a session local exactly
// when its worker shares the device's machine; without a requested worker
// it picks the user's live worker on that machine.
func TestCreateSessionLocalMode(t *testing.T) {
	// The device d1 is on machine m-d1.
	const here, elsewhere = "m-d1", "m-far"
	for _, tc := range []struct {
		name    string
		workers []localWorker
		pin     string
		local   bool // the client sets config.local
		field   string
		code    connect.Code
		wantPin string
		want    bool
	}{
		{name: "no worker"},
		{name: "worker on the same machine", workers: []localWorker{{id: "w1", machine: here}}, wantPin: "w1", want: true},
		{name: "worker on another machine", workers: []localWorker{{id: "w1", machine: elsewhere}}},
		{name: "deleted worker", workers: []localWorker{{id: "w1", machine: here, deleted: true}}},
		{name: "revoked worker", workers: []localWorker{{id: "w1", machine: here, revoked: true}}},
		{name: "worker of another user", workers: []localWorker{{id: "w1", machine: here, other: true}}},
		{name: "several candidates", workers: []localWorker{
			{id: "w-never", machine: here},
			{id: "w-old", machine: here, connectedAt: 100},
			{id: "w-b", machine: here, connectedAt: 200},
			{id: "w-a", machine: here, connectedAt: 200},
			{id: "w-far", machine: elsewhere, connectedAt: 300},
			{id: "w-gone", machine: here, connectedAt: 300, deleted: true},
		}, wantPin: "w-a", want: true},
		{name: "explicit pin on the machine", workers: []localWorker{{id: "w1", machine: here}, {id: "w2", machine: here, connectedAt: 500}},
			pin: "w1", wantPin: "w1", want: true},
		{name: "explicit pin off the machine", workers: []localWorker{{id: "w1", machine: here}, {id: "w2", machine: elsewhere}},
			pin: "w2", wantPin: "w2"},
		{name: "explicit pin to a deleted worker", workers: []localWorker{{id: "w1", machine: here, deleted: true}},
			pin: "w1", field: "pinned_worker_id", code: connect.CodeInvalidArgument},
		{name: "client sets local", workers: []localWorker{{id: "w1", machine: here}},
			local: true, field: "config.local", code: connect.CodeInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, super := newHarness(t)
			h.device(t, "d1", "box", "linux")
			for _, w := range tc.workers {
				h.addWorker(t, w)
			}
			p := super.mustCreate(t, "Project", loc("box", "/work"))
			req := &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "key", Config: &circusv1.SessionConfig{Local: tc.local}}
			if tc.pin != "" {
				req.PinnedWorkerId = new(tc.pin)
			}
			got, err := super.createSession(t, req)
			if tc.field != "" {
				wantField(t, err, tc.code, tc.field)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.GetPinnedWorkerId() != tc.wantPin || got.GetConfig().GetLocal() != tc.want {
				t.Fatalf("created pin %q local %v, want %q %v", got.GetPinnedWorkerId(), got.GetConfig().GetLocal(), tc.wantPin, tc.want)
			}
			read := super.getSession(t, got.SessionId)
			if !proto.Equal(read, got) {
				t.Fatalf("GetSession = %v, want %v", read, got)
			}
		})
	}
}

// TestCreateSessionLocalRepeat: a repeat compares what the client sent, not
// the derived pin, and returns the first session whatever the workers
// became since.
func TestCreateSessionLocalRepeat(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "Project", loc("box", "/work"))
	remote := &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "remote"}
	first, err := super.createSession(t, remote)
	if err != nil || first.GetConfig().GetLocal() || first.GetPinnedWorkerId() != "" {
		t.Fatalf("first = %v, %v", first, err)
	}

	// A worker appears on the machine: the repeat still returns the
	// remote session; a new request gets a local one.
	h.addWorker(t, localWorker{id: "w1", machine: "m-d1"})
	again, err := super.createSession(t, remote)
	if err != nil || !proto.Equal(again, first) {
		t.Fatalf("repeat after a worker appeared = %v, %v, want %v", again, err, first)
	}
	derived := &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "derived"}
	local, err := super.createSession(t, derived)
	if err != nil || !local.GetConfig().GetLocal() || local.GetPinnedWorkerId() != "w1" {
		t.Fatalf("local = %v, %v", local, err)
	}

	// The client asked for no worker: naming the derived one is another
	// request; repeating without one returns the local session even after
	// its worker is gone and another took its place.
	named := proto.CloneOf(derived)
	named.PinnedWorkerId = new("w1")
	_, err = super.createSession(t, named)
	wantField(t, err, connect.CodeAlreadyExists, "client_request_id")
	if _, err := h.st.DeleteIdentity(t.Context(), store.KindWorker, "w1", h.clock.Load()); err != nil {
		t.Fatal(err)
	}
	h.addWorker(t, localWorker{id: "w2", machine: "m-d1"})
	again, err = super.createSession(t, derived)
	if err != nil || !proto.Equal(again, local) {
		t.Fatalf("repeat after the worker changed = %v, %v, want %v", again, err, local)
	}

	// An explicit pin is compared as sent.
	explicit := &circusv1.CreateSessionRequest{ProjectId: p.ProjectId, ClientRequestId: "explicit", PinnedWorkerId: new("w2")}
	pinned, err := super.createSession(t, explicit)
	if err != nil || !pinned.GetConfig().GetLocal() || pinned.GetPinnedWorkerId() != "w2" {
		t.Fatalf("pinned = %v, %v", pinned, err)
	}
	again, err = super.createSession(t, explicit)
	if err != nil || !proto.Equal(again, pinned) {
		t.Fatalf("explicit repeat = %v, %v, want %v", again, err, pinned)
	}
	unnamed := proto.CloneOf(explicit)
	unnamed.PinnedWorkerId = nil
	_, err = super.createSession(t, unnamed)
	wantField(t, err, connect.CodeAlreadyExists, "client_request_id")
}
