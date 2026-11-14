package workerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
)

func TestSubmitValidatesIDsBeforeOpeningStream(t *testing.T) {
	c := &Client{}
	valid := func() *circusv1.DeviceCall {
		return &circusv1.DeviceCall{CallId: "valid", SessionId: "session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "true"}}}
	}
	for _, call := range []*circusv1.DeviceCall{nil, {CallId: "bad id", SessionId: "session"}, {CallId: "valid", SessionId: "bad session"}, {CallId: "valid", SessionId: ""}} {
		if _, err := c.Submit(t.Context(), "desk", call); err == nil {
			t.Fatalf("accepted invalid call: %v", call)
		}
	}
	call := valid()
	call.CallId = strings.Repeat("x", 129)
	if _, err := c.Submit(t.Context(), "desk", call); err == nil {
		t.Fatal("accepted overlong call ID")
	}
}

func TestCancelValidatesID(t *testing.T) {
	c := &Client{}
	if _, err := c.Cancel(t.Context(), ""); err == nil {
		t.Fatal("empty call ID was sent instead of rejected locally")
	}
}

func TestReaderTeardownPreservesQueuedOutcome(t *testing.T) {
	transport := errors.New("transport failure")
	c := &Client{ops: make(map[uint64]*Updates), done: make(chan struct{})}
	resultStream, err := c.open(false)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := c.open(false)
	if err != nil {
		t.Fatal(err)
	}
	result := &circusv1.CallUpdate{CallId: "result", Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{}}}
	if err = c.enqueue(resultStream.id, result, 1); err != nil {
		t.Fatal(err)
	}
	c.err = transport
	close(c.done)
	if update, err := resultStream.Next(t.Context()); err != nil || update.GetResult() == nil {
		t.Fatalf("queued outcome was discarded: %v, %v", update, err)
	}
	if _, err = pending.Next(t.Context()); !errors.Is(err, transport) {
		t.Fatalf("blocked Next did not observe transport error: %v", err)
	}
}

func TestTerminalNextRacingCloseReleasesStream(t *testing.T) {
	for range 100 {
		c := &Client{ops: make(map[uint64]*Updates), done: make(chan struct{})}
		s, err := c.open(false)
		if err != nil {
			t.Fatal(err)
		}
		u := &circusv1.CallUpdate{CallId: "call", Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{}}}
		if err = c.enqueue(s.id, u, 1); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = s.Next(t.Context())
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = s.Close()
		}()
		close(start)
		wg.Wait()
		if len(c.ops) != 0 || c.bytes != 0 {
			t.Fatalf("terminal Next/Close race stranded an operation or charge: %d ops, %d bytes", len(c.ops), c.bytes)
		}
	}
}

func TestTerminalNextDrainsLaterQueuedUpdates(t *testing.T) {
	c := &Client{ops: make(map[uint64]*Updates), done: make(chan struct{})}
	s, err := c.open(false)
	if err != nil {
		t.Fatal(err)
	}
	result := &circusv1.CallUpdate{CallId: "call", Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{}}}
	if err = c.enqueue(s.id, result, 1); err != nil {
		t.Fatal(err)
	}
	// A stale update queued behind the terminal one (the operation already
	// ended, so a later one is dropped) is released with it.
	s.queue <- queued{result, 1}
	c.bytes++
	if _, err = s.Next(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.bytes != 0 || len(s.queue) != 0 {
		t.Fatalf("terminal Next retained charges=%d queued=%d", c.bytes, len(s.queue))
	}
}

// fakeServer runs script on the server side of one worker link after a real
// handshake, then holds the socket open until the client closes it.
func fakeServer(t *testing.T, script func(ctx context.Context, l *wire.Link), local ...wire.Options) *Client {
	t.Helper()
	served := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(served)
		conn, err := wire.AcceptWebSocket(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
		defer cancel()
		_, opts, err := wire.AcceptHello(ctx, conn, wire.WorkerUplink, wire.Options{MaxMessageBytes: 1 << 20}, func(*circusv1.Hello) *circusv1.Welcome {
			return &circusv1.Welcome{PrincipalId: "worker", InstanceId: "instance", ServerTimeUnixMs: 1}
		})
		if err != nil {
			return
		}
		link, err := wire.New(conn, wire.WorkerControlSide, opts)
		if err != nil {
			return
		}
		defer link.Close()
		script(ctx, link)
		// Keep the socket open until the client's own policy closes it;
		// otherwise EOF would race and hide the client's reason.
		for {
			if _, err := link.Receive(ctx); err != nil {
				return
			}
		}
	}))
	t.Cleanup(func() { server.Close(); <-served })
	o := Options{Credentials: credfile.File{ControlPlaneURL: server.URL, Role: credfile.RoleWorker, PrincipalID: "worker", Token: "token"}}
	if len(local) > 0 {
		o.Wire = local[0]
	}
	c, err := Dial(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func receive(t *testing.T, ctx context.Context, l *wire.Link) *circusv1.WorkerUplink {
	t.Helper()
	m, err := l.Receive(ctx)
	if err != nil {
		t.Error(err)
		return &circusv1.WorkerUplink{}
	}
	return m.(*circusv1.WorkerUplink)
}

func update(rid uint64, u *circusv1.CallUpdate) *circusv1.WorkerDownlink {
	return &circusv1.WorkerDownlink{RequestId: rid, Payload: &circusv1.WorkerDownlink_CallUpdate{CallUpdate: u}}
}

func accepted(id string) *circusv1.CallUpdate {
	return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Accepted_{Accepted: &circusv1.CallUpdate_Accepted{}}}
}
func dispatched(id string) *circusv1.CallUpdate {
	return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Dispatched_{Dispatched: &circusv1.CallUpdate_Dispatched{}}}
}
func result(id string) *circusv1.CallUpdate {
	return &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{}}}
}

func bash(id string) *circusv1.DeviceCall {
	return &circusv1.DeviceCall{CallId: id, SessionId: "session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "true"}}}
}

func TestUpdateQueueOverflowFailsLink(t *testing.T) {
	c := fakeServer(t, func(ctx context.Context, l *wire.Link) {
		rid := receive(t, ctx, l).GetRequestId()
		for range 9 {
			if l.Send(ctx, update(rid, accepted("overflow"))) != nil {
				return
			}
		}
	})
	_, err := c.Submit(t.Context(), "desk", bash("overflow"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
		if c.Err() == nil || !strings.Contains(c.Err().Error(), "update queue full") {
			t.Fatalf("link ended for wrong reason: %v", c.Err())
		}
	case <-time.After(wait.Timeout):
		t.Fatal("reader dropped an overflowing update without failing the link")
	}
	// Exercise the real Submit send path on its now-closed wire link. Clear
	// only the recorded reader error so open reaches Send rather than failing early.
	c.mu.Lock()
	c.err = nil
	c.ops = make(map[uint64]*Updates)
	c.mu.Unlock()
	_, err = c.Submit(t.Context(), "desk", bash("send-error"))
	if !errors.Is(err, ErrSubmitSent) {
		t.Fatalf("Submit send error was not wrapped in ErrSubmitSent: %v", err)
	}
}

// The local cap mirrors the server's: one operation over it is refused without
// sending; closing an open stream sends Unwatch for its request ID before the
// next operation, which then reuses the freed slot under a fresh ID.
func TestCapUnwatchAndRequestIDs(t *testing.T) {
	got := make(chan *circusv1.WorkerUplink, maxOperations+2)
	c := fakeServer(t, func(ctx context.Context, l *wire.Link) {
		for range maxOperations + 2 {
			got <- receive(t, ctx, l)
		}
	})
	streams := make([]*Updates, maxOperations)
	for i := range streams {
		var err error
		if streams[i], err = c.Submit(t.Context(), "desk", bash(fmt.Sprint("call", i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Submit(t.Context(), "desk", bash("over")); err == nil || errors.Is(err, ErrSubmitSent) {
		t.Fatalf("operation over the cap was not refused locally: %v", err)
	}
	if err := streams[1].Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Submit(t.Context(), "desk", bash("over")); err != nil {
		t.Fatalf("Close did not free the local slot at once: %v", err)
	}
	for i := range maxOperations {
		m := wait.Recv(t, got)
		if m.GetSubmitCall() == nil || m.GetRequestId() != uint64(i+1) {
			t.Fatalf("operation %d: %v", i, m)
		}
	}
	if m := wait.Recv(t, got); m.GetUnwatch() == nil || m.GetRequestId() != 2 {
		t.Fatalf("expected Unwatch of request 2 before the next operation, got %v", m)
	}
	if m := wait.Recv(t, got); m.GetSubmitCall().GetCall().GetCallId() != "over" || m.GetRequestId() != maxOperations+1 {
		t.Fatalf("reused slot did not get a fresh request ID: %v", m)
	}
}

// Updates are routed by request ID: a Query of a watched call gets its own
// answer, and updates for an unwatched request are dropped without harm.
func TestRoutingByRequestID(t *testing.T) {
	unwatched := make(chan struct{})
	c := fakeServer(t, func(ctx context.Context, l *wire.Link) {
		watch := receive(t, ctx, l)   // Submit "same", request 1
		other := receive(t, ctx, l)   // Submit "other", request 2
		query := receive(t, ctx, l)   // Query "same", request 3
		unwatch := receive(t, ctx, l) // Unwatch 2
		if unwatch.GetUnwatch() == nil || unwatch.GetRequestId() != other.GetRequestId() {
			t.Errorf("expected Unwatch of %d, got %v", other.GetRequestId(), unwatch)
		}
		close(unwatched)
		// A late update for the unwatched request is dropped by the client.
		_ = l.Send(ctx, update(other.GetRequestId(), result("other")))
		_ = l.Send(ctx, update(query.GetRequestId(), dispatched("same")))
		_ = l.Send(ctx, update(watch.GetRequestId(), result("same")))
	})
	watch, err := c.Submit(t.Context(), "desk", bash("same"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.Submit(t.Context(), "desk", bash("other"))
	if err != nil {
		t.Fatal(err)
	}
	type answer struct {
		u   *circusv1.CallUpdate
		err error
	}
	queried := make(chan answer, 1)
	go func() {
		u, err := c.Query(t.Context(), "same")
		queried <- answer{u, err}
	}()
	wait.For(t, func() bool { c.mu.Lock(); defer c.mu.Unlock(); return len(c.ops) == 3 })
	if err = other.Close(); err != nil {
		t.Fatal(err)
	}
	wait.Recv(t, unwatched)
	if a := wait.Recv(t, queried); a.err != nil || a.u.GetDispatched() == nil {
		t.Fatalf("Query of a watched call: %v %v", a.u, a.err)
	}
	ctx := wait.Ctx(t)
	if u, err := watch.Next(ctx); err != nil || u.GetResult() == nil {
		t.Fatalf("watched stream: %v %v", u, err)
	}
	if _, err := other.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("unwatched stream received a late update: %v", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil || len(c.ops) != 0 {
		t.Fatalf("link err %v, open operations %d", c.err, len(c.ops))
	}
}

// A Query whose caller gives up unwatches it, freeing the slot at once.
func TestAbandonedQueryUnwatches(t *testing.T) {
	got := make(chan *circusv1.WorkerUplink, 4)
	c := fakeServer(t, func(ctx context.Context, l *wire.Link) {
		for range 2 {
			got <- receive(t, ctx, l)
		}
	})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { wait.Recv(t, got); cancel() }()
	if _, err := c.Query(ctx, "slow"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if m := wait.Recv(t, got); m.GetUnwatch() == nil || m.GetRequestId() != 1 {
		t.Fatalf("abandoned Query did not unwatch: %v", m)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.ops) != 0 {
		t.Fatal("abandoned Query kept its slot")
	}
}

// Concurrent Close and Submit at the cap never let the server see more than
// maxOperations open operations: a slot freed by Close is reused only after
// its Unwatch is queued.
func TestConcurrentCloseAndSubmitStayWithinServerCap(t *testing.T) {
	const rounds = 400
	violation := make(chan string, 1)
	done := make(chan struct{})
	c := fakeServer(t, func(ctx context.Context, l *wire.Link) {
		open := map[uint64]bool{}
		for {
			m, err := l.Receive(ctx)
			if err != nil {
				return
			}
			up := m.(*circusv1.WorkerUplink)
			switch {
			case up.GetUnwatch() != nil:
				delete(open, up.GetRequestId())
			case up.GetSubmitCall() != nil:
				open[up.GetRequestId()] = true
				if len(open) > maxOperations {
					select {
					case violation <- fmt.Sprintf("%d operations open at request %d", len(open), up.GetRequestId()):
					default:
					}
				}
				if up.GetSubmitCall().GetCall().GetCallId() == "last" {
					close(done)
				}
			}
		}
	}, wire.Options{MaxQueuedMessages: 4096}) // a burst well above the default queue
	streams := make(chan *Updates, maxOperations)
	for i := range maxOperations {
		s, err := c.Submit(t.Context(), "desk", bash(fmt.Sprint("fill", i)))
		if err != nil {
			t.Fatal(err)
		}
		streams <- s
	}
	// Closers free slots while separate submitters take them, so a Submit
	// can only overtake a Close's Unwatch if the two are not ordered.
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range rounds / 4 {
				_ = (<-streams).Close()
			}
		}()
	}
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds / 4 {
				for {
					s, err := c.Submit(t.Context(), "desk", bash(fmt.Sprint("r", w, "-", i)))
					if err == nil {
						streams <- s
						break
					}
					if errors.Is(err, ErrSubmitSent) || c.Err() != nil {
						t.Error(err)
						return
					}
					runtime.Gosched()
				}
			}
		}()
	}
	wg.Wait()
	_ = (<-streams).Close()
	if _, err := c.Submit(t.Context(), "desk", bash("last")); err != nil {
		t.Fatal(err)
	}
	wait.Recv(t, done)
	select {
	case v := <-violation:
		t.Fatal(v)
	default:
	}
}
