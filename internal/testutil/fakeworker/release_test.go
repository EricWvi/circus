package fakeworker

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

func releaseIdleEvent(epoch uint64) *pb.WorkerEvent {
	return &pb.WorkerEvent{Event: &pb.WorkerEvent_ReleaseIdle_{ReleaseIdle: &pb.WorkerEvent_ReleaseIdle{SessionId: linkSessionID, Epoch: epoch}}}
}

func inputNudge() *pb.WorkerEvent {
	return &pb.WorkerEvent{Event: &pb.WorkerEvent_Nudge_{Nudge: &pb.WorkerEvent_Nudge{
		SessionId: linkSessionID, Reasons: []pb.WorkerEvent_Nudge_Reason{pb.WorkerEvent_Nudge_REASON_INPUT},
	}}}
}

// TestReleaseIdleRequests: the fake worker, like the real one, releases on
// ReleaseIdle only the lease it holds at that epoch, only while idle: a
// request for another epoch, one that arrives during a turn, and one that
// arrives with an input to run are no-ops; one while idle releases with
// REASON_IDLE long before the idle timer.
func TestReleaseIdleRequests(t *testing.T) {
	f := &fakeLink{inputs: []*input{{InputId: "in-1", Kind: pb.SessionReply_Inputs_Input_KIND_USER, Text: "hello"}},
		hints: make(chan *pb.WorkerEvent, 8)}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{
			Credential:  credfile.File{ControlPlaneURL: server.URL, Role: credfile.RoleWorker, PrincipalID: linkWorkerID, Token: "token"},
			StreamDelay: time.Millisecond, Logger: slog.New(slog.DiscardHandler),
		})
	}()
	defer func() {
		cancel()
		if err := wait.Recv(t, done); err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	reports := func() (started, ended int) {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, r := range f.reports {
			if r.GetPhase() == pb.ReportTurn_PHASE_STARTED {
				started++
			} else {
				ended++
			}
		}
		return started, ended
	}
	releases := func() int {
		f.mu.Lock()
		defer f.mu.Unlock()
		return len(f.releases)
	}
	ended := func(n int) {
		t.Helper()
		wait.For(t, func() bool { _, e := reports(); return e == n })
	}
	quiet := func(what string) {
		t.Helper()
		time.Sleep(100 * time.Millisecond)
		if n := releases(); n != 0 {
			t.Fatalf("%s: released", what)
		}
	}
	ended(1)

	// Another epoch.
	f.hints <- releaseIdleEvent(2)
	quiet("another epoch")

	// During a turn: a slow one, asked once it has started.
	f.mu.Lock()
	f.inputs = []*input{{InputId: "in-2", Kind: pb.SessionReply_Inputs_Input_KIND_USER, Text: "/fake slow 1"}}
	f.mu.Unlock()
	f.hints <- inputNudge()
	wait.For(t, func() bool { s, _ := reports(); return s == 2 })
	f.hints <- releaseIdleEvent(1)
	ended(2)
	quiet("during a turn")

	// With an input to run: the request and the input's Nudge together.
	f.mu.Lock()
	f.inputs = []*input{{InputId: "in-3", Kind: pb.SessionReply_Inputs_Input_KIND_USER, Text: "again"}}
	f.mu.Unlock()
	f.hints <- inputNudge()
	f.hints <- releaseIdleEvent(1)
	ended(3)
	quiet("with an input")

	// Idle: released at once, as by the idle timer.
	f.hints <- releaseIdleEvent(1)
	wait.For(t, func() bool { return releases() == 1 })
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.releases[0] != pb.ReleaseLease_REASON_IDLE {
		t.Fatalf("released with %v", f.releases[0])
	}
}
