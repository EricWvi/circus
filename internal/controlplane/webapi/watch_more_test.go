package webapi_test

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

// gatedStore holds one ReadEvents result (read before the hold) until
// released.
type gatedStore struct {
	store.Store
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (g *gatedStore) ReadEvents(ctx context.Context, f store.EventFilter) (store.EventPage, error) {
	page, err := g.Store.ReadEvents(ctx, f)
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		<-g.release
	}
	return page, err
}

// TestWatchEphemeralBeforeLaterDurable: while a stream is busy, the bus
// queues [mark 2, E, mark 3] with 2 and 3 already stored. The stream sends 2,
// then E, then 3: E was published before 3, so it must not follow it.
func TestWatchEphemeralBeforeLaterDurable(t *testing.T) {
	g := &gatedStore{entered: make(chan struct{}), release: make(chan struct{})}
	h, super := newHarness(t, func(c *webapi.Config) { g.Store = c.Store; c.Store = g })
	_, sess := sessionSetup(t, h, super)
	p := publisher{h, sess.SessionId}
	stream, _ := super.watch(t, sess.SessionId, 0)
	if !stream.Receive() || stream.Msg().Kind != circusv1.SessionEvent_KIND_UNSPECIFIED {
		t.Fatal("no opening keepalive", stream.Err())
	}
	g.armed.Store(true)
	p.durable(t, 1) // the stream takes [mark 1], reads [1] and holds
	<-g.entered
	p.durable(t, 2)
	p.ephemeral("published before 3")
	p.durable(t, 3)
	close(g.release)
	var order []string
	for len(order) < 4 {
		if !stream.Receive() {
			t.Fatal(stream.Err())
		}
		ev := stream.Msg()
		switch {
		case ev.Kind == circusv1.SessionEvent_KIND_UNSPECIFIED:
		case ev.Ephemeral:
			order = append(order, "E")
		default:
			order = append(order, fmt.Sprint(ev.Seq))
		}
	}
	if got := strings.Join(order, " "); got != "1 2 E 3" {
		t.Fatalf("order = %s, want 1 2 E 3", got)
	}
}

// TestWatchStaleEphemeralSkipped: events committed during the initial
// replay are read by the replay itself; an ephemeral event queued before
// their announcement is stale and skipped, one queued after it is sent.
func TestWatchStaleEphemeralSkipped(t *testing.T) {
	g := &gatedStore{entered: make(chan struct{}), release: make(chan struct{})}
	h, super := newHarness(t, func(c *webapi.Config) { g.Store = c.Store; c.Store = g })
	_, sess := sessionSetup(t, h, super)
	p := publisher{h, sess.SessionId}
	p.durable(t, 1)
	g.armed.Store(true) // the replay's first read holds
	go func() {
		<-g.entered
		p.ephemeral("stale")
		p.durable(t, 2)
		p.ephemeral("fresh")
		close(g.release)
	}()
	stream, _ := super.watch(t, sess.SessionId, 0)
	var order []string
	for len(order) < 3 {
		if !stream.Receive() {
			t.Fatal(stream.Err())
		}
		ev := stream.Msg()
		switch {
		case ev.Kind == circusv1.SessionEvent_KIND_UNSPECIFIED:
		case ev.Ephemeral:
			order = append(order, ev.PayloadJson)
		default:
			order = append(order, fmt.Sprint(ev.Seq))
		}
	}
	if got := strings.Join(order, " "); got != "1 2 fresh" {
		t.Fatalf("order = %s, want 1 2 fresh", got)
	}
}

// TestWatchIdleMultiPageReplay replays more than one page with nothing
// committed meanwhile.
func TestWatchIdleMultiPageReplay(t *testing.T) {
	h, super := newHarness(t)
	_, sess := sessionSetup(t, h, super)
	p := publisher{h, sess.SessionId}
	const n = 2*store.MaxPageSize + 10
	for i := range n {
		p.durable(t, i)
	}
	// Large payloads also cut pages by bytes.
	big := `"` + strings.Repeat("x", store.MaxEventPayloadBytes-10) + `"`
	for range 3 {
		if _, err := h.st.AppendEvents(t.Context(), sess.SessionId, nil, []store.EventInput{{Kind: "ASSISTANT", Payload: []byte(big), At: h.clock.Load()}}, store.DefaultMaxViewBytes); err != nil {
			t.Fatal(err)
		}
	}
	stream, _ := super.watch(t, sess.SessionId, 0)
	seqs, _ := collect(t, stream, n+3)
	wantContiguous(t, seqs, 1, n+3)
}

// nextDurable receives until a durable event, failing after a few seconds.
func nextDurable(t *testing.T, stream *connect.ServerStreamForClient[circusv1.SessionEvent], cancel context.CancelFunc) *circusv1.SessionEvent {
	t.Helper()
	timer := time.AfterFunc(3*time.Second, cancel)
	defer timer.Stop()
	for stream.Receive() {
		if !stream.Msg().Ephemeral {
			return stream.Msg()
		}
	}
	t.Fatalf("no durable event: %v", stream.Err())
	return nil
}

// TestWatchSeesUserSideEvents: the events the user-side RPCs record reach
// an open stream without any other announcement. Interrupt records none.
func TestWatchSeesUserSideEvents(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	_, sess := sessionSetup(t, h, super)
	id := sess.SessionId
	stream, cancel := super.watch(t, id, 0)
	if !stream.Receive() {
		t.Fatal(stream.Err())
	}
	want := func(kind circusv1.SessionEvent_Kind) {
		t.Helper()
		if ev := nextDurable(t, stream, cancel); ev.Kind != kind {
			t.Fatalf("event = %v, want %v", ev, kind)
		}
	}
	if _, err := super.send(t, id, "m1", "hello"); err != nil {
		t.Fatal(err)
	}
	want(circusv1.SessionEvent_KIND_INPUT_ACCEPTED)
	fence := h.lease(t, id)
	h.requestApproval(t, fence, "ap1", 600_000)
	if _, err := super.sessions.ResolveApproval(ctx, connect.NewRequest(&circusv1.ResolveApprovalRequest{ApprovalId: "ap1"})); err != nil {
		t.Fatal(err)
	}
	want(circusv1.SessionEvent_KIND_APPROVAL_RESOLVED)
	h.startTurn(t, fence, 1)
	if _, err := h.st.ReportTurn(ctx, fence, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnCompleted, At: h.clock.Load(), TranscriptIncomplete: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := super.sessions.RecoverSession(ctx, connect.NewRequest(&circusv1.RecoverSessionRequest{SessionId: id, AcceptTranscriptGap: true})); err != nil {
		t.Fatal(err)
	}
	want(circusv1.SessionEvent_KIND_TRANSCRIPT_GAP_ACCEPTED)
	if _, err := super.sessions.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: id})); err != nil {
		t.Fatal(err)
	}
	want(circusv1.SessionEvent_KIND_SESSION_STATE)
}

func TestWatchEndsOnSignOut(t *testing.T) {
	h, super := newHarness(t, func(c *webapi.Config) { c.WatchRecheck = 20 * time.Millisecond })
	_, sess := sessionSetup(t, h, super)
	stream, _ := super.watch(t, sess.SessionId, 0)
	if !stream.Receive() {
		t.Fatal(stream.Err())
	}
	// Still signed in: rechecks keep the stream.
	time.Sleep(100 * time.Millisecond)
	last := publisher{h, sess.SessionId}.durable(t, 1)
	collect(t, stream, last)
	if _, err := super.auth.Logout(t.Context(), connect.NewRequest(&circusv1.LogoutRequest{})); err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	wantCode(t, stream.Err(), connect.CodeUnauthenticated)
	wait.For(t, func() bool { return h.bus.Watchers() == 0 })
}

func TestWatchEndsOnSessionExpiry(t *testing.T) {
	h, super := newHarness(t, func(c *webapi.Config) { c.WatchRecheck = 20 * time.Millisecond })
	_, sess := sessionSetup(t, h, super)
	stream, _ := super.watch(t, sess.SessionId, 0)
	if !stream.Receive() {
		t.Fatal(stream.Err())
	}
	h.clock.Add(store.WebSessionIdleTimeout.Milliseconds() + 1)
	for stream.Receive() {
	}
	wantCode(t, stream.Err(), connect.CodeUnauthenticated)
}

func TestWatchMaxLifetime(t *testing.T) {
	h, super := newHarness(t, func(c *webapi.Config) {
		c.WatchMaxLifetime = 150 * time.Millisecond
		c.WatchKeepalive = 20 * time.Millisecond
	})
	_, sess := sessionSetup(t, h, super)
	stream, _ := super.watch(t, sess.SessionId, 0)
	start := time.Now()
	for stream.Receive() {
	}
	wantCode(t, stream.Err(), connect.CodeUnavailable)
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("ended after %v", d)
	}
}

// TestWatchBlockedSendEnds: a stream whose client stopped reading is
// blocked in a write; deleting the session or shutting down still ends it
// at once, long before the write timeout.
func TestWatchBlockedSendEnds(t *testing.T) {
	for _, how := range []string{"delete", "shutdown"} {
		t.Run(how, func(t *testing.T) {
			exited := make(chan struct{}, 1)
			h, super := newHarness(t, func(c *webapi.Config) { c.Interceptor = exitRecorder{c.Interceptor, exited} })
			ctx := t.Context()
			_, sess := sessionSetup(t, h, super)
			if _, err := super.sessions.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: sess.SessionId})); err != nil {
				t.Fatal(err)
			}
			stream, _ := super.watch(t, sess.SessionId, 0)
			wait.For(t, func() bool { return h.bus.Watchers() == 1 })
			// Far more than the connection buffers hold: the handler ends up
			// blocked in Send.
			// The bus drops what does not fit its queue, so publish at a pace
			// the handler can take: about 100 MiB offered over a second.
			p := publisher{h, sess.SessionId}
			chunk := strings.Repeat("e", 100<<10)
			for range 1000 {
				p.ephemeral(chunk)
				time.Sleep(time.Millisecond)
			}
			start := time.Now()
			switch how {
			case "delete":
				if _, err := super.sessions.DeleteSession(ctx, connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: sess.SessionId})); err != nil {
					t.Fatal(err)
				}
			case "shutdown":
				h.bus.Close()
			}
			select {
			case <-exited:
			case <-time.After(5 * time.Second):
				t.Fatal("the blocked handler did not return")
			}
			if d := time.Since(start); d > 5*time.Second {
				t.Fatalf("stream ended after %v", d)
			}
			stream.Close()
		})
	}
}

// exitRecorder signals when a WatchSession handler returns.
type exitRecorder struct {
	connect.Interceptor
	exited chan struct{}
}

func (e exitRecorder) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return e.Interceptor.WrapStreamingHandler(func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		err := next(ctx, conn)
		select {
		case e.exited <- struct{}{}:
		default:
		}
		return err
	})
}

// pipeListener serves connections made of net.Pipe: no buffering at all, so
// every server write waits for the client to read it.
type pipeListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newPipeListener() *pipeListener {
	return &pipeListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *pipeListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *pipeListener) Addr() net.Addr { return pipeAddr{} }

func (l *pipeListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	server, client := net.Pipe()
	select {
	case l.conns <- server:
		return client, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.done:
		return nil, net.ErrClosed
	}
}

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }

// throttledConn reads at most 8 KiB at a time at about 500 KiB/s: a slow
// link that keeps moving.
type throttledConn struct{ net.Conn }

func (c throttledConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p[:min(len(p), 8<<10)])
	time.Sleep(time.Duration(n) * 2 * time.Microsecond)
	return n, err
}

// TestWatchSlowLink: an event whose write takes longer than the write
// timeout still arrives on a link that keeps making progress.
func TestWatchSlowLink(t *testing.T) {
	const timeout = 300 * time.Millisecond
	h, _ := newHarness(t, func(c *webapi.Config) { c.WatchWriteTimeout = timeout })
	h.device(t, "d1", "box", "linux")
	l := newPipeListener()
	srv := &http.Server{Handler: h.server.Config.Handler}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { h.bus.Close(); _ = srv.Close() })
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := l.dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return throttledConn{c}, nil
	}}
	t.Cleanup(transport.CloseIdleConnections)
	jar, _ := cookiejar.New(nil)
	slow := &http.Client{Jar: jar, Transport: transport}
	const base = "http://pipe"
	auth := circusv1connect.NewAuthServiceClient(slow, base)
	if _, err := auth.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: "admin@example.com", Password: password})); err != nil {
		t.Fatal(err)
	}
	c := &client{http: slow, auth: auth, projects: circusv1connect.NewProjectServiceClient(slow, base),
		sessions: circusv1connect.NewSessionServiceClient(slow, base)}
	p := c.mustCreate(t, "Project", loc("box", "/work"))
	sess := c.mustSession(t, p.ProjectId)
	big := `"` + strings.Repeat("x", 200<<10) + `"`
	const events = 2
	for range events {
		if _, err := h.st.AppendEvents(t.Context(), sess.SessionId, nil, []store.EventInput{{Kind: "ASSISTANT", Payload: []byte(big), At: h.clock.Load()}}, store.DefaultMaxViewBytes); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	stream, _ := c.watch(t, sess.SessionId, 0)
	seqs, _ := collect(t, stream, events)
	wantContiguous(t, seqs, 1, events)
	// Each event took longer than the write timeout to write.
	if d := time.Since(start); d < events*timeout {
		t.Fatalf("the link was not slow: %v", d)
	}
}

// recheckStore lets a test make the browser-session recheck fail or find
// the user gone or without an email. Only the SessionService sees it; the
// auth service keeps the plain store.
type recheckStore struct {
	store.Store
	busy, gone, noEmail atomic.Bool
}

func (r *recheckStore) UseWebSession(ctx context.Context, hash []byte, at int64) (store.WebSession, error) {
	if r.busy.Load() {
		return store.WebSession{}, store.ErrBusy
	}
	return r.Store.UseWebSession(ctx, hash, at)
}

func (r *recheckStore) GetUser(ctx context.Context, id string) (store.User, error) {
	u, err := r.Store.GetUser(ctx, id)
	switch {
	case r.gone.Load():
		return store.User{}, store.ErrNotFound
	case r.noEmail.Load():
		u.Email = ""
	}
	return u, err
}

func TestWatchRecheckUser(t *testing.T) {
	for _, how := range []string{"deleted", "no email"} {
		t.Run(how, func(t *testing.T) {
			rs := &recheckStore{}
			h, super := newHarness(t, func(c *webapi.Config) {
				rs.Store, c.Store = c.Store, rs
				c.WatchRecheck = 20 * time.Millisecond
			})
			_, sess := sessionSetup(t, h, super)
			stream, _ := super.watch(t, sess.SessionId, 0)
			if !stream.Receive() {
				t.Fatal(stream.Err())
			}
			if how == "deleted" {
				rs.gone.Store(true)
			} else {
				rs.noEmail.Store(true)
			}
			for stream.Receive() {
			}
			wantCode(t, stream.Err(), connect.CodeUnauthenticated)
		})
	}
}

// TestWatchRecheckStoreError: a failing recheck keeps the stream; the next
// successful one decides.
func TestWatchRecheckStoreError(t *testing.T) {
	rs := &recheckStore{}
	h, super := newHarness(t, func(c *webapi.Config) {
		rs.Store, c.Store = c.Store, rs
		c.WatchRecheck = 20 * time.Millisecond
	})
	_, sess := sessionSetup(t, h, super)
	stream, _ := super.watch(t, sess.SessionId, 0)
	if !stream.Receive() {
		t.Fatal(stream.Err())
	}
	rs.busy.Store(true)
	time.Sleep(200 * time.Millisecond) // about ten failed rechecks
	last := publisher{h, sess.SessionId}.durable(t, 1)
	collect(t, stream, last)
	rs.busy.Store(false)
	if _, err := super.auth.Logout(t.Context(), connect.NewRequest(&circusv1.LogoutRequest{})); err != nil {
		t.Fatal(err)
	}
	for stream.Receive() {
	}
	wantCode(t, stream.Err(), connect.CodeUnauthenticated)
}

// gatedConn stops reading once it has read a response containing marker,
// until release is closed: a client that took a stream's final status and
// then stopped reading.
type gatedConn struct {
	net.Conn
	marker  []byte
	release chan struct{}
	shut    *atomic.Bool
}

func (c gatedConn) Read(p []byte) (int, error) {
	if c.shut.Load() {
		<-c.release
	}
	n, err := c.Conn.Read(p)
	if bytes.Contains(p[:n], c.marker) {
		c.shut.Store(true)
	}
	return n, err
}

// TestWatchFinalWriteBounded: net/http writes the end of a chunked response
// (its terminator) after the handler returns. When the client stops
// reading right after the final status, that write fails after the write
// timeout and the connection closes, instead of blocking forever.
func TestWatchFinalWriteBounded(t *testing.T) {
	const timeout = 200 * time.Millisecond
	h, super := newHarness(t, func(c *webapi.Config) { c.WatchWriteTimeout = timeout })
	_, sess := sessionSetup(t, h, super)
	if _, err := super.sessions.StopSession(t.Context(), connect.NewRequest(&circusv1.StopSessionRequest{SessionId: sess.SessionId})); err != nil {
		t.Fatal(err)
	}
	l := newPipeListener()
	closed := make(chan struct{})
	srv := &http.Server{Handler: h.server.Config.Handler, ConnState: func(_ net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			close(closed)
		}
	}}
	go func() { _ = srv.Serve(l) }()
	release := make(chan struct{})
	t.Cleanup(func() { close(release); _ = srv.Close() })
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := l.dial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return gatedConn{Conn: c, marker: []byte(`"not_found"`), release: release, shut: &atomic.Bool{}}, nil
	}}
	// The watching browser: same cookie as the super user.
	u, _ := url.Parse(h.server.URL)
	jar, _ := cookiejar.New(nil)
	base, _ := url.Parse("http://pipe")
	jar.SetCookies(base, super.http.Jar.Cookies(u))
	watcher := circusv1connect.NewSessionServiceClient(&http.Client{Jar: jar, Transport: transport}, "http://pipe")
	stream, err := watcher.WatchSession(t.Context(), connect.NewRequest(&circusv1.WatchSessionRequest{SessionId: sess.SessionId}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatal(stream.Err())
	}
	// The client keeps reading in the background, up to the gate.
	go func() {
		for stream.Receive() {
		}
	}()
	if _, err := super.sessions.DeleteSession(t.Context(), connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: sess.SessionId})); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closed:
	case <-time.After(10 * timeout):
		t.Fatal("the response's last write is still blocked")
	}
}
