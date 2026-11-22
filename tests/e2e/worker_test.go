//go:build e2e && linux

package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

func TestTypeScriptWorkerLink(t *testing.T) {
	buildWorker(t)
	h := newHarness(t)

	api, project := browserSessionAPI(t, h)
	ctx, cancel := context.WithTimeout(t.Context(), 35*time.Second)
	defer cancel()
	reply, err := api.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{
		ClientRequestId: uuid.NewString(), ProjectId: project,
	}))
	must(t, err)
	session := reply.Msg.GetSession()
	// The harness enrolls daemon and worker together on the same machine.
	if !session.GetConfig().GetLocal() || session.GetPinnedWorkerId() != h.worker.PrincipalID {
		t.Fatalf("session did not select its local worker: %v", session)
	}
	s := &sessionBrowser{t: t, ctx: ctx, api: api, id: session.GetSessionId()}
	input := s.send("run a local turn", pb.InputPriority_INPUT_PRIORITY_NORMAL)
	bootID := uuid.Must(uuid.NewV7()).String()
	proxy, blocked, drop := relayWorkerReply(t, h.base)
	w := h.startCommand(exec.Command("node", filepath.Join(root, "worker/test/e2e/session.ts"),
		s.id, input.GetInputId(), bootID, proxy), nil)
	select {
	case <-blocked:
	case <-w.done:
		t.Fatalf("TypeScript worker exited before the state reply: %v", w.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	lease, err := h.st.GetLease(ctx, s.id)
	must(t, err)
	if lease.Epoch == 0 || lease.ConnID == "" || lease.BootID != bootID {
		t.Fatalf("worker has no bound lease: %+v", lease)
	}
	// Control-plane shutdown aborts worker links. Reuse the same address and
	// store so the existing lease can be rebound during restart grace.
	h.server.signal(t, syscall.SIGTERM)
	must(t, h.server.wait(t, 5*time.Second))
	h.startServer(false)
	close(drop)
	must(t, w.wait(t, 12*time.Second))
	if !strings.Contains(w.stdout.String(), "lease resumed and released\n") {
		t.Fatalf("worker did not resume and release the lease: %s", w.stdout.String())
	}
	if state := s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetTurnCount() != 1 {
		t.Fatalf("worker started more than one turn: %v", state)
	}
	// The browser client validates the committed event independently of the
	// worker's PublishEvents reply.
	watch := s.watch(input.GetEventSeq())
	event := watch.until(contains(pb.SessionEvent_KIND_RESULT, "typescript worker"))
	if event.GetSeq() != input.GetEventSeq()+1 {
		t.Fatalf("unexpected event sequence: %v", event)
	}
	after, err := h.st.GetLease(ctx, s.id)
	must(t, err)
	if after.Epoch != lease.Epoch || after.ConnID != "" {
		t.Fatalf("lease after release: before %+v, after %+v", lease, after)
	}
}

// Hold the first state reply after forwarding the request to the control
// plane. The test restarts the server and then drops the connection while
// the worker is still waiting. Later connections go straight through.
func relayWorkerReply(t *testing.T, base string) (string, <-chan struct{}, chan<- struct{}) {
	t.Helper()
	target, err := url.Parse(base)
	must(t, err)
	pass := httputil.NewSingleHostReverseProxy(target)
	blocked, drop := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !first.CompareAndSwap(false, true) {
			pass.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
		defer cancel()
		up, _, err := wire.DialWebSocket(ctx, "ws"+strings.TrimPrefix(base, "http")+r.URL.Path,
			&websocket.DialOptions{HTTPHeader: http.Header{"Authorization": r.Header.Values("Authorization")}})
		if err != nil {
			http.Error(w, "upstream worker dial failed", http.StatusBadGateway)
			return
		}
		defer up.Close()
		down, err := wire.AcceptWebSocket(w, r, nil)
		if err != nil {
			return
		}
		defer down.Close()
		up.SetReadLimit(wire.DefaultMaxFragmentBytes + wire.FrameOverhead)
		down.SetReadLimit(wire.DefaultMaxFragmentBytes + wire.FrameOverhead)
		sent := make(chan struct{})
		go func() {
			defer close(sent)
			defer cancel()
			for {
				frame, err := down.ReadMessage(ctx)
				if err != nil || up.WriteMessage(ctx, frame) != nil {
					return
				}
			}
		}()
		defer func() { cancel(); <-sent }()
		for {
			frame, err := up.ReadMessage(ctx)
			if err != nil {
				return
			}
			msg, err := wire.Decode(wire.WorkerDownlink, frame, wire.DefaultMaxFragmentBytes+wire.FrameOverhead)
			if err != nil {
				return
			}
			if msg.(*pb.WorkerDownlink).GetSessionReply().GetState() != nil {
				close(blocked)
				select {
				case <-drop:
				case <-ctx.Done():
				}
				return
			}
			if down.WriteMessage(ctx, frame) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return server.URL, blocked, drop
}
