//go:build e2e && linux

package e2e

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/fakeworker"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/google/uuid"
)

// TestLocalMode: the worker `circus join --role both` enrolled shares the
// daemon's machine, so a session created without a worker is local and
// pinned to it; a session pinned to a worker on another machine is not
// local. Each runs only on its own worker: the local one waits while its
// worker is offline although another worker is attached.
func TestLocalMode(t *testing.T) {
	h := newHarness(t)
	api, project := browserSessionAPI(t, h)
	ctx := testwait.Ctx(t)

	token, _, err := auth.CreateEnrollToken(ctx, h.st, h.user(), store.KindWorker, time.Hour, time.Now())
	must(t, err)
	e, err := auth.Enroll(ctx, h.st, auth.EnrollRequest{Token: token, Kind: store.KindWorker, Name: "far", MachineID: "another-machine", OS: "linux", Arch: "amd64"}, time.Now())
	must(t, err)
	far := credfile.File{ControlPlaneURL: h.base, Role: credfile.RoleWorker, PrincipalID: e.Identity.ID, Token: e.Token}

	create := func(pin string) *sessionBrowser {
		t.Helper()
		req := &pb.CreateSessionRequest{ClientRequestId: uuid.NewString(), ProjectId: project}
		if pin != "" {
			req.PinnedWorkerId = &pin
		}
		res, err := api.CreateSession(ctx, connect.NewRequest(req))
		must(t, err)
		s := &sessionBrowser{t: t, ctx: ctx, api: api, id: res.Msg.GetSession().GetSessionId()}
		got, err := api.GetSession(ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: s.id}))
		must(t, err)
		if got.Msg.GetPinnedWorkerId() != res.Msg.GetSession().GetPinnedWorkerId() || got.Msg.GetConfig().GetLocal() != res.Msg.GetSession().GetConfig().GetLocal() {
			t.Fatalf("GetSession %v, created %v", got.Msg, res.Msg.GetSession())
		}
		return s
	}
	local := create("")
	if s := local.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_UNSPECIFIED); !s.GetConfig().GetLocal() || s.GetPinnedWorkerId() != h.worker.PrincipalID {
		t.Fatalf("session without a worker = %v, want local on %s", s, h.worker.PrincipalID)
	}
	remote := create(far.PrincipalID)
	if s := remote.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_UNSPECIFIED); s.GetConfig().GetLocal() || s.GetPinnedWorkerId() != far.PrincipalID {
		t.Fatalf("session on another machine = %v", s)
	}

	run := func(cred credfile.File) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() {
			done <- fakeworker.Run(ctx, fakeworker.Options{Credential: cred, MaxSessions: 4, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
		}()
		t.Cleanup(func() {
			cancel()
			if err := testwait.Recv(t, done); !errors.Is(err, context.Canceled) {
				t.Errorf("fake worker %s exited: %v", cred.PrincipalID, err)
			}
		})
	}

	// Only the far worker runs: the remote session completes, the local
	// one waits for its own worker.
	run(far)
	local.send("hello local", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	remote.send("hello remote", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	remote.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
	if s := local.state(pb.Session_STATE_QUEUED, pb.TurnOutcome_TURN_OUTCOME_UNSPECIFIED); s.GetTurnCount() != 0 {
		t.Fatalf("local session ran elsewhere: %v", s)
	}

	run(h.worker)
	local.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED)
}
