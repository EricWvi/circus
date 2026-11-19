package webapi_test

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

// withBus gives the harness a bus with other bounds.
func withBus(opts sessions.BusOptions, out **sessions.Bus) func(*webapi.Config) {
	return func(c *webapi.Config) {
		*out = sessions.NewBus(opts)
		c.Bus = *out
	}
}

// watch opens a WatchSession stream that ends with the test.
func (c *client) watch(t *testing.T, sessionID string, afterSeq uint64) (*connect.ServerStreamForClient[circusv1.SessionEvent], context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := c.sessions.WatchSession(ctx, connect.NewRequest(&circusv1.WatchSessionRequest{SessionId: sessionID, AfterSeq: afterSeq}))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); stream.Close() })
	return stream, cancel
}

// watchError opens a stream and returns how it ends.
func (c *client) watchError(t *testing.T, sessionID string, afterSeq uint64) error {
	t.Helper()
	stream, _ := c.watch(t, sessionID, afterSeq)
	for stream.Receive() {
		if stream.Msg().GetKind() != circusv1.SessionEvent_KIND_UNSPECIFIED {
			t.Fatalf("unexpected event %v", stream.Msg())
		}
	}
	return stream.Err()
}

// publisher appends durable events through the store and announces them on
// the bus after each commit, as the worker link does.
type publisher struct {
	h  *harness
	id string
}

func (p publisher) durable(t *testing.T, n int) int64 {
	t.Helper()
	res, err := p.h.st.AppendEvents(context.Background(), p.id, nil, []store.EventInput{{Kind: "ASSISTANT", Payload: fmt.Appendf(nil, `{"n":%d}`, n), At: p.h.clock.Load()}}, store.DefaultMaxViewBytes)
	if err != nil {
		t.Error(err)
		return 0
	}
	p.h.bus.Durable(p.id, uint64(res.Seqs[0]))
	return res.Seqs[0]
}

func (p publisher) ephemeral(text string) {
	p.h.bus.Ephemeral(p.id, &circusv1.SessionEvent{Kind: circusv1.SessionEvent_KIND_ASSISTANT, PayloadJson: text, Ephemeral: true, TsUnixMs: p.h.clock.Load()})
}

// collect reads a stream until it has seen durable seq last, and returns
// the durable seqs in order and the number of ephemeral events other than
// keepalives.
func collect(t *testing.T, stream *connect.ServerStreamForClient[circusv1.SessionEvent], last int64) ([]int64, int) {
	t.Helper()
	seqs, ephemeral, err := receiveUntil(stream, last)
	if err != nil {
		t.Fatal(err)
	}
	return seqs, ephemeral
}

func receiveUntil(stream *connect.ServerStreamForClient[circusv1.SessionEvent], last int64) ([]int64, int, error) {
	var seqs []int64
	ephemeral := 0
	for len(seqs) == 0 || seqs[len(seqs)-1] < last {
		if !stream.Receive() {
			return nil, 0, fmt.Errorf("stream ended after %d events: %w", len(seqs), stream.Err())
		}
		ev := stream.Msg()
		switch {
		case ev.GetEphemeral():
			if ev.GetSeq() != 0 {
				return nil, 0, fmt.Errorf("ephemeral with seq: %v", ev)
			}
			if ev.GetKind() != circusv1.SessionEvent_KIND_UNSPECIFIED {
				ephemeral++
			}
		default:
			seqs = append(seqs, int64(ev.GetSeq()))
		}
	}
	return seqs, ephemeral, nil
}

func wantContiguous(t *testing.T, seqs []int64, from, to int64) {
	t.Helper()
	if int64(len(seqs)) != to-from+1 {
		t.Fatalf("%d events, want %d..%d", len(seqs), from, to)
	}
	for i, s := range seqs {
		if s != from+int64(i) {
			t.Fatalf("event %d has seq %d, want %d (gap or duplicate)", i, s, from+int64(i))
		}
	}
}

// TestWatchReplayThenLive starts streams while events are being committed:
// every stream sees each durable event exactly once, in order, across the
// replay pages and the switch to live delivery.
func TestWatchReplayThenLive(t *testing.T) {
	h, super := newHarness(t, func(c *webapi.Config) { c.WatchKeepalive = 50 * time.Millisecond })
	_, sess := sessionSetup(t, h, super)
	p := publisher{h, sess.SessionId}
	const before, total = store.MaxPageSize + 50, 900
	for i := range before {
		p.durable(t, i)
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := before; i < total; i++ {
			p.durable(t, i)
			if i%3 == 0 {
				p.ephemeral(fmt.Sprint("partial ", i))
			}
		}
	})
	results := make(chan string, 4)
	for _, after := range []uint64{0, 0, 150, before + 100} {
		wg.Go(func() {
			stream, _ := super.watch(t, sess.SessionId, after)
			seqs, _, err := receiveUntil(stream, total)
			if err != nil {
				results <- err.Error()
				return
			}
			for i, s := range seqs {
				if s != int64(after)+1+int64(i) {
					results <- fmt.Sprintf("after %d: event %d has seq %d", after, i, s)
					return
				}
			}
			results <- ""
		})
	}
	wg.Wait()
	close(results)
	for r := range results {
		if r != "" {
			t.Fatal(r)
		}
	}
	// Events the bus announces again or late are not repeated.
	stream, _ := super.watch(t, sess.SessionId, total-1)
	seqs, _ := collect(t, stream, total)
	// A keepalive comes only from the live loop: the replay has finished,
	// so it cannot read the events below and make "live" stale.
	for stream.Receive() && stream.Msg().Kind != circusv1.SessionEvent_KIND_UNSPECIFIED {
	}
	h.bus.Durable(sess.SessionId, total)
	p.ephemeral("live")
	last := p.durable(t, total)
	more, ephemeral := collect(t, stream, last)
	wantContiguous(t, append(seqs, more...), total, total+1)
	if ephemeral != 1 {
		t.Fatalf("ephemeral events = %d", ephemeral)
	}
}

func TestWatchEventFields(t *testing.T) {
	h, super := newHarness(t)
	_, sess := sessionSetup(t, h, super)
	if _, err := super.send(t, sess.SessionId, "m1", "hello"); err != nil {
		t.Fatal(err)
	}
	big := `"` + strings.Repeat("x", store.MaxEventPayloadBytes) + `"`
	if _, err := h.st.AppendEvents(t.Context(), sess.SessionId, nil, []store.EventInput{{Kind: "TURN_STARTED", Payload: []byte(big), At: 42, Boundary: true}}, store.DefaultMaxViewBytes); err != nil {
		t.Fatal(err)
	}
	stream, _ := super.watch(t, sess.SessionId, 0)
	var got []*circusv1.SessionEvent
	for len(got) < 2 && stream.Receive() {
		got = append(got, stream.Msg())
	}
	if len(got) != 2 {
		t.Fatalf("events = %v, %v", got, stream.Err())
	}
	if got[0].Kind != circusv1.SessionEvent_KIND_INPUT_ACCEPTED || got[0].Seq != 1 || !strings.Contains(got[0].PayloadJson, `"text":"hello"`) || got[0].TsUnixMs != h.clock.Load() || got[0].Ephemeral {
		t.Fatalf("user event = %v", got[0])
	}
	if got[1].Kind != circusv1.SessionEvent_KIND_TURN_STARTED || got[1].Seq != 2 || !got[1].Truncated || got[1].PayloadJson != "" || got[1].TsUnixMs != 42 {
		t.Fatalf("truncated event = %v", got[1])
	}
	err := super.watchError(t, sess.SessionId, math.MaxInt64+1)
	wantField(t, err, connect.CodeInvalidArgument, "after_seq")
}

// TestWatchSlowWatcherLosesEphemeral leaves a stream unread while large
// ephemeral events pile up: once the connection's buffers are full the
// watcher's queue fills and drops ephemeral events, and every durable event
// still arrives.
func TestWatchSlowWatcherLosesEphemeral(t *testing.T) {
	var bus *sessions.Bus
	h, super := newHarness(t, withBus(sessions.BusOptions{MaxQueuedEvents: 4, MaxQueuedBytes: 256 << 10}, &bus))
	h.bus = bus
	t.Cleanup(bus.Close)
	_, sess := sessionSetup(t, h, super)
	p := publisher{h, sess.SessionId}
	stream, _ := super.watch(t, sess.SessionId, 0)
	wait.For(t, func() bool { return bus.Watchers() == 1 })
	const sent = 400
	chunk := strings.Repeat("e", 100<<10)
	for i := range sent {
		p.ephemeral(chunk)
		if i%100 == 0 {
			p.durable(t, i)
		}
	}
	last := p.durable(t, sent)
	seqs, ephemeral := collect(t, stream, last)
	wantContiguous(t, seqs, 1, last)
	if ephemeral == 0 || ephemeral >= sent {
		t.Fatalf("%d of %d ephemeral events arrived", ephemeral, sent)
	}
}

func TestWatchCaps(t *testing.T) {
	var bus *sessions.Bus
	h, super := newHarness(t, withBus(sessions.BusOptions{MaxWatchers: 3, MaxWatchersPerUser: 2}, &bus))
	h.bus = bus
	t.Cleanup(bus.Close)
	_, sess := sessionSetup(t, h, super)
	_, cancelFirst := super.watch(t, sess.SessionId, 0)
	super.watch(t, sess.SessionId, 0)
	wait.For(t, func() bool { return bus.Watchers() == 2 })
	wantCode(t, super.watchError(t, sess.SessionId, 0), connect.CodeResourceExhausted)

	bob := h.signIn(t, "bob", "bob@example.com")
	h.foreignSession(t, "bob", "bob-project", "bob-session")
	bob.watch(t, "bob-session", 0)
	wait.For(t, func() bool { return bus.Watchers() == 3 })
	carol := h.signIn(t, "carol", "carol@example.com")
	h.foreignSession(t, "carol", "carol-project", "carol-session")
	wantCode(t, carol.watchError(t, "carol-session", 0), connect.CodeResourceExhausted)

	// A closed stream frees its slot.
	cancelFirst()
	wait.For(t, func() bool { return bus.Watchers() == 2 })
	carol.watch(t, "carol-session", 0)
	wait.For(t, func() bool { return bus.Watchers() == 3 })
}

func TestWatchEnds(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	p, sess := sessionSetup(t, h, super)
	stop := func(id string) {
		t.Helper()
		if _, err := super.sessions.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: id})); err != nil {
			t.Fatal(err)
		}
	}
	ended := func(stream *connect.ServerStreamForClient[circusv1.SessionEvent]) error {
		t.Helper()
		done := make(chan error, 1)
		go func() {
			for stream.Receive() {
			}
			done <- stream.Err()
		}()
		return wait.Recv(t, done)
	}

	// Deleting the session.
	stop(sess.SessionId)
	stream, _ := super.watch(t, sess.SessionId, 0)
	wait.For(t, func() bool { return h.bus.Watchers() == 1 })
	if _, err := super.sessions.DeleteSession(ctx, connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: sess.SessionId})); err != nil {
		t.Fatal(err)
	}
	wantCode(t, ended(stream), connect.CodeNotFound)
	wait.For(t, func() bool { return h.bus.Watchers() == 0 })

	// Deleting the project deletes its sessions.
	second := super.mustSession(t, p.ProjectId)
	stop(second.SessionId)
	stream, _ = super.watch(t, second.SessionId, 0)
	wait.For(t, func() bool { return h.bus.Watchers() == 1 })
	if _, err := super.projects.DeleteProject(ctx, connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId})); err != nil {
		t.Fatal(err)
	}
	wantCode(t, ended(stream), connect.CodeNotFound)

	// The client going away.
	p = super.mustCreate(t, "Again", loc("box", "/work"))
	third := super.mustSession(t, p.ProjectId)
	_, cancel := super.watch(t, third.SessionId, 0)
	wait.For(t, func() bool { return h.bus.Watchers() == 1 })
	cancel()
	wait.For(t, func() bool { return h.bus.Watchers() == 0 })

	// The server shutting down.
	stream, _ = super.watch(t, third.SessionId, 0)
	wait.For(t, func() bool { return h.bus.Watchers() == 1 })
	h.bus.Close()
	wantCode(t, ended(stream), connect.CodeUnavailable)
	wantCode(t, super.watchError(t, third.SessionId, 0), connect.CodeUnavailable)
}

func TestWatchKeepalive(t *testing.T) {
	h, super := newHarness(t, func(c *webapi.Config) { c.WatchKeepalive = 20 * time.Millisecond })
	_, sess := sessionSetup(t, h, super)
	stream, _ := super.watch(t, sess.SessionId, 0)
	for range 2 {
		if !stream.Receive() {
			t.Fatal(stream.Err())
		}
		ev := stream.Msg()
		if ev.Kind != circusv1.SessionEvent_KIND_UNSPECIFIED || !ev.Ephemeral || ev.Seq != 0 || ev.PayloadJson != "" {
			t.Fatalf("keepalive = %v", ev)
		}
	}
	publisher{h, sess.SessionId}.durable(t, 1)
	for stream.Receive() && stream.Msg().Kind == circusv1.SessionEvent_KIND_UNSPECIFIED {
	}
	if stream.Msg().Seq != 1 {
		t.Fatalf("event = %v, %v", stream.Msg(), stream.Err())
	}
	if !stream.Receive() || stream.Msg().Kind != circusv1.SessionEvent_KIND_UNSPECIFIED {
		t.Fatalf("no keepalive after an event: %v", stream.Err())
	}
}
