//go:build e2e && linux

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/fakeworker"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/google/uuid"
)

// outageClock holds the fake worker's reconnect after a scripted outage
// (its two-second pause) until the test releases it, so the control plane
// always sees the link closed before the worker returns. Other waits run
// in real time.
type outageClock struct {
	clock.Real
	release chan struct{}
}

func (c outageClock) After(d time.Duration) <-chan time.Time {
	if d != 2*time.Second {
		return c.Real.After(d)
	}
	ch := make(chan time.Time, 1)
	go func() {
		<-c.release
		ch <- time.Now()
	}()
	return ch
}

// workerGone returns the disconnect time of a KIND_WORKER event.
func workerGone(t *testing.T, e *pb.SessionEvent) int64 {
	t.Helper()
	var p struct {
		At string `json:"disconnected_at_unix_ms"`
	}
	must(t, json.Unmarshal([]byte(e.GetPayloadJson()), &p))
	at, err := strconv.ParseInt(p.At, 10, 64)
	must(t, err)
	if !e.GetEphemeral() || e.GetSeq() != 0 {
		t.Fatalf("worker event not ephemeral: %v", e)
	}
	return at
}

// expireLease moves lease l's expiry into the past, written directly into
// the control plane's database, if l is still the session's lease.
func expireLease(t *testing.T, h *harness, l store.Lease) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+h.db+"?_pragma=busy_timeout(5000)")
	must(t, err)
	defer db.Close()
	res, err := db.ExecContext(t.Context(), `UPDATE session_leases SET expires_at = ? WHERE session_id = ? AND epoch = ? AND holder_conn_id = ?`,
		time.Now().UnixMilli()-1, l.SessionID, l.Epoch, l.ConnID)
	must(t, err)
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("lease %+v no longer held: %d rows, %v", l, n, err)
	}
}

// TestWorkerDisconnected: when the fake worker's link drops during a
// running turn, watchers learn at once that the lease holder is
// disconnected (KIND_WORKER and Session.worker_disconnected_at_unix_ms).
// A worker that comes back resumes its lease and the mark clears; one
// that never does loses the lease at expiry, which clears it too.
func TestWorkerDisconnected(t *testing.T) {
	h := newHarness(t)
	api, project := browserSessionAPI(t, h)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	outage := outageClock{release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- fakeworker.Run(ctx, fakeworker.Options{
			Credential: h.worker, MaxSessions: 4, StreamDelay: 40 * time.Millisecond, Clock: outage,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := testwait.Recv(t, done); !errors.Is(err, context.Canceled) {
			t.Errorf("fake worker exited: %v", err)
		}
	})
	open := func(t *testing.T) *sessionBrowser {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
		t.Cleanup(cancel)
		res, err := api.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{ClientRequestId: uuid.NewString(), ProjectId: project}))
		must(t, err)
		return &sessionBrowser{t: t, ctx: ctx, api: api, id: res.Msg.GetSession().GetSessionId()}
	}
	gone := func(s *sessionBrowser) int64 {
		s.t.Helper()
		resp, err := s.api.GetSession(s.ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: s.id}))
		must(s.t, err)
		return resp.Msg.GetWorkerDisconnectedAtUnixMs()
	}
	isWorker := func(e *pb.SessionEvent) bool { return e.GetKind() == pb.SessionEvent_KIND_WORKER }
	// disconnect starts a turn that drops the link and waits until the
	// watch announces the disconnect; the turn is still running.
	disconnect := func(s *sessionBrowser, watch *sessionWatch) int64 {
		s.t.Helper()
		if at := gone(s); at != 0 {
			s.t.Fatalf("connected holder marked disconnected at %d", at)
		}
		before := time.Now().UnixMilli()
		s.send("/fake blip", pb.InputPriority_INPUT_PRIORITY_NORMAL)
		at := workerGone(s.t, watch.until(isWorker))
		if at < before || at > time.Now().UnixMilli() {
			s.t.Fatalf("disconnected at %d, outside %d..now", at, before)
		}
		state := s.state(pb.Session_STATE_RUNNING, pb.TurnOutcome_TURN_OUTCOME_UNSPECIFIED)
		if state.GetWorkerDisconnectedAtUnixMs() != at {
			s.t.Fatalf("session %v, want disconnected at %d", state, at)
		}
		return at
	}

	t.Run("reconnect clears", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		disconnect(s, watch)
		outage.release <- struct{}{}
		if at := workerGone(t, watch.until(isWorker)); at != 0 {
			t.Fatalf("after the reconnect: disconnected at %d, want 0", at)
		}
		watch.until(contains(pb.SessionEvent_KIND_RESULT, "/fake blip"))
		if state := s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_COMPLETED); state.GetWorkerDisconnectedAtUnixMs() != 0 || state.GetTurnCount() != 1 {
			t.Fatalf("after the reconnect: %v", state)
		}
	})

	t.Run("expiry clears", func(t *testing.T) {
		s := open(t)
		watch := s.watch(0)
		watch.expect(pb.SessionEvent_KIND_UNSPECIFIED)
		disconnect(s, watch)
		// The worker never comes back. Rather than wait out the lease's
		// remaining 40 to 60 seconds, end its lifetime now in the database
		// (the store never moves an expiry back): the control plane's
		// expiry sweep (every few seconds) releases it as it would later.
		l, err := h.st.GetLease(s.ctx, s.id)
		must(t, err)
		expireLease(t, h, l)
		// The loss announces its boundary and the cleared mark; the stream
		// may deliver them in either order.
		var boundary *pb.SessionEvent
		cleared := false
		for boundary == nil || !cleared {
			e := watch.until(func(e *pb.SessionEvent) bool {
				return isWorker(e) || e.GetKind() == pb.SessionEvent_KIND_TURN_INTERRUPTED
			})
			if !isWorker(e) {
				boundary = e
				continue
			}
			if at := workerGone(t, e); at != 0 || cleared {
				t.Fatalf("after the expiry: disconnected at %d (cleared before: %v), want one 0", at, cleared)
			}
			cleared = true
		}
		if eventString(t, boundary, "turn_no") != "1" {
			t.Fatalf("expiry boundary %v", boundary)
		}
		state := s.state(pb.Session_STATE_IDLE, pb.TurnOutcome_TURN_OUTCOME_INTERRUPTED)
		if state.GetWorkerDisconnectedAtUnixMs() != 0 {
			t.Fatalf("after the expiry: %v", state)
		}
		if l, err := h.st.GetLease(s.ctx, s.id); err != nil || l.WorkerID != "" {
			t.Fatalf("lease after the expiry: %+v, %v", l, err)
		}
	})
}
