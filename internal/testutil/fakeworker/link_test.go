package fakeworker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
)

const (
	linkWorkerID  = "worker-1"
	linkSessionID = "session-1"
	linkDevice    = "box"
	linkCwd       = "/work"
)

// fakeLink is a scripted control plane for one session on the worker link:
// it offers the session, leases it with ledgerPath, hands out inputs on the
// first TakeInputs, answers every device call with result (a successful
// text output by default) and records what the worker sends.
type fakeLink struct {
	ledgerPath string
	inputs     []*input
	result     func(call *pb.DeviceCall) *pb.DeviceResult

	mu               sync.Mutex
	turns, published uint64
	appended         map[string]uint64 // by subpath
	entries          map[string][]*pb.TranscriptEntry
	events           []*pb.PublishEvents_Event
	reports          []*pb.ReportTurn
	calls            []*pb.SubmitCall
	releases         []pb.ReleaseLease_Reason

	// hints, when set, are sent on the Attach stream as they come.
	hints chan *pb.WorkerEvent
	// ended is how many ENDED reports run waits for (default 1).
	ended int
}

// run serves the fake worker until it has reported a turn ENDED, then
// stops the worker.
func (f *fakeLink) run(t *testing.T) {
	t.Helper()
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
	wait.For(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		n := 0
		for _, r := range f.reports {
			if r.GetPhase() == pb.ReportTurn_PHASE_ENDED {
				n++
			}
		}
		return n >= max(f.ended, 1) && f.reports[len(f.reports)-1].GetPhase() == pb.ReportTurn_PHASE_ENDED
	})
	cancel()
	if err := wait.Recv(t, done); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func (f *fakeLink) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := wire.AcceptWebSocket(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
	defer cancel()
	_, opts, err := wire.AcceptHello(ctx, conn, wire.WorkerUplink, wire.Options{}, func(*pb.Hello) *pb.Welcome {
		return &pb.Welcome{PrincipalId: linkWorkerID, InstanceId: "instance", ServerTimeUnixMs: 1}
	})
	if err != nil {
		return
	}
	link, err := wire.New(conn, wire.WorkerControlSide, opts)
	if err != nil {
		return
	}
	defer link.Close()
	for {
		m, err := link.Receive(ctx)
		if err != nil {
			return
		}
		up := m.(*pb.WorkerUplink)
		down := &pb.WorkerDownlink{RequestId: up.GetRequestId()}
		switch {
		case up.GetAttach() != nil:
			down.Payload = &pb.WorkerDownlink_WorkerEvent{WorkerEvent: &pb.WorkerEvent{
				Event: &pb.WorkerEvent_Offer_{Offer: &pb.WorkerEvent_Offer{SessionId: linkSessionID}},
			}}
			if f.hints != nil {
				rid := up.GetRequestId()
				go func() {
					for {
						select {
						case ev := <-f.hints:
							if link.Send(ctx, &pb.WorkerDownlink{RequestId: rid, Payload: &pb.WorkerDownlink_WorkerEvent{WorkerEvent: ev}}) != nil {
								return
							}
						case <-ctx.Done():
							return
						}
					}
				}()
			}
		case up.GetResumeLeases() != nil:
			down.Payload = sessionReply(&pb.SessionReply{Result: &pb.SessionReply_Resumed_{Resumed: &pb.SessionReply_Resumed{}}})
		case up.GetSession() != nil:
			down.Payload = sessionReply(f.session(up.GetSession()))
		case up.GetSubmitCall() != nil:
			down.Payload = &pb.WorkerDownlink_CallUpdate{CallUpdate: f.submit(up.GetSubmitCall())}
		default:
			continue
		}
		if err := link.Send(ctx, down); err != nil {
			return
		}
	}
}

func sessionReply(r *pb.SessionReply) *pb.WorkerDownlink_SessionReply {
	return &pb.WorkerDownlink_SessionReply{SessionReply: r}
}

func (f *fakeLink) session(req *pb.SessionRequest) *pb.SessionReply {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch op := req.GetOp().(type) {
	case *pb.SessionRequest_AcquireLease:
		return &pb.SessionReply{Result: &pb.SessionReply_Lease_{Lease: &pb.SessionReply_Lease{
			Epoch: 1, ClaudeSessionId: "claude-1", LedgerPath: f.ledgerPath,
			Config: &pb.SessionConfig{DefaultDevice: linkDevice, Cwds: []*pb.SessionConfig_Cwd{{Device: linkDevice, Cwd: linkCwd}}},
		}}}
	case *pb.SessionRequest_GetLaunchEnv:
		return &pb.SessionReply{Result: &pb.SessionReply_LaunchEnv_{LaunchEnv: &pb.SessionReply_LaunchEnv{LocalLogin: true}}}
	case *pb.SessionRequest_TakeInputs:
		inputs := f.inputs
		f.inputs = nil
		return &pb.SessionReply{Result: &pb.SessionReply_Inputs_{Inputs: &pb.SessionReply_Inputs{Inputs: inputs}}}
	case *pb.SessionRequest_ReportTurn:
		f.reports = append(f.reports, op.ReportTurn)
		if op.ReportTurn.GetPhase() == pb.ReportTurn_PHASE_STARTED {
			f.turns++
		}
		return &pb.SessionReply{Result: &pb.SessionReply_Turn_{Turn: &pb.SessionReply_Turn{TurnNo: f.turns}}}
	// A batch is resent after a lost reply; like the control plane, the
	// fake keeps it once.
	case *pb.SessionRequest_AppendTranscript:
		a := op.AppendTranscript
		if f.appended == nil {
			f.appended, f.entries = make(map[string]uint64), make(map[string][]*pb.TranscriptEntry)
		}
		if a.GetBatchSeq() > f.appended[a.GetSubpath()] {
			f.appended[a.GetSubpath()] = a.GetBatchSeq()
			f.entries[a.GetSubpath()] = append(f.entries[a.GetSubpath()], a.GetEntries()...)
		}
	case *pb.SessionRequest_PublishEvents:
		if seq := op.PublishEvents.GetBatchSeq(); seq > f.published {
			f.published = seq
			f.events = append(f.events, op.PublishEvents.GetEvents()...)
		}
	case *pb.SessionRequest_ReleaseLease:
		f.releases = append(f.releases, op.ReleaseLease.GetReason())
	case *pb.SessionRequest_GetState:
		return &pb.SessionReply{Result: &pb.SessionReply_State{State: &pb.SessionReply_SessionStateReply{State: &pb.SessionState{}}}}
	}
	return &pb.SessionReply{Result: &pb.SessionReply_Ok_{Ok: &pb.SessionReply_Ok{}}}
}

func (f *fakeLink) submit(req *pb.SubmitCall) *pb.CallUpdate {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	result := &pb.DeviceResult{Output: textOutput("ok", false)}
	if f.result != nil {
		result = f.result(req.GetCall())
	}
	result.CallId = req.GetCall().GetCallId()
	return &pb.CallUpdate{CallId: result.CallId, Update: &pb.CallUpdate_Result{Result: result}}
}

func textOutput(text string, isError bool) *pb.ToolOutput {
	return &pb.ToolOutput{Content: []*pb.ContentBlock{{Block: &pb.ContentBlock_Text{Text: text}}}, IsError: isError}
}

// kinds returns the decoded payloads of the published durable events of
// kind.
func (f *fakeLink) kinds(kind pb.SessionEvent_Kind) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, e := range f.events {
		if e.GetKind() != kind || e.GetEphemeral() {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(e.GetJson()), &payload); err != nil {
			panic(err)
		}
		out = append(out, payload)
	}
	return out
}

// resultText is the text of the turn's RESULT.
func (f *fakeLink) resultText(t *testing.T) string {
	t.Helper()
	results := f.kinds(pb.SessionEvent_KIND_RESULT)
	if len(results) != 1 {
		t.Fatalf("%d results, want 1", len(results))
	}
	text, _ := results[0]["result"].(string)
	return text
}

func (f *fakeLink) submitted() []*pb.SubmitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*pb.SubmitCall(nil), f.calls...)
}
