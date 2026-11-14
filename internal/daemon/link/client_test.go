package link_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	daemonlink "github.com/EricWvi/circus/internal/daemon/link"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wire/wiretest"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Run(m, leakcheck.Config{})) }

type tool struct {
	run func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error)
}

func (tool) Name() string { return "read" }
func (t tool) Run(ctx context.Context, c *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	return t.run(ctx, c)
}
func result(text string) *circusv1.DeviceResult {
	return &circusv1.DeviceResult{Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: text}}}}}
}
func call(id string) *circusv1.DeviceCall {
	return &circusv1.DeviceCall{CallId: id, SessionId: "session", Tool: &circusv1.DeviceCall_Read{Read: &circusv1.ReadRequest{Path: "/test"}}}
}
func recv[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	return wait.Recv(t, ch)
}
func eventually(t *testing.T, f func() bool) {
	t.Helper()
	wait.For(t, f)
}
func send(t *testing.T, l *wire.Link, m *circusv1.DaemonDownlink) {
	t.Helper()
	if err := l.Send(wait.Ctx(t), m); err != nil {
		t.Fatal(err)
	}
}
func receive(t *testing.T, l *wire.Link) *circusv1.DaemonUplink {
	t.Helper()
	m, err := l.Receive(wait.Ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	return m.(*circusv1.DaemonUplink)
}

var freshDispatchAtUnixMs = clock.New().Now().UnixMilli()

func dispatchAt(t *testing.T, l *wire.Link, boot string, c *circusv1.DeviceCall, dispatchedAt int64) {
	t.Helper()
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_DispatchCall{DispatchCall: &circusv1.DispatchCall{Call: c, TargetBootId: boot, DispatchedAtUnixMs: dispatchedAt}}})
}
func dispatch(t *testing.T, l *wire.Link, boot string, c *circusv1.DeviceCall) {
	t.Helper()
	dispatchAt(t, l, boot, c, freshDispatchAtUnixMs)
}
func ack(t *testing.T, l *wire.Link) *circusv1.CallAck {
	t.Helper()
	for {
		m := receive(t, l)
		if a := m.GetCallAck(); a != nil {
			return a
		}
	}
}

type pair struct {
	client, server *wiretest.Endpoint
	raw            wire.Conn
}
type harness struct {
	client *daemonlink.Client
	clk    *clock.Fake
	pairs  chan pair
	cancel context.CancelFunc
	done   chan error
	opts   wire.Options
}

func start(t *testing.T, change func(*daemonlink.Options), wrap func(wire.Conn) wire.Conn) *harness {
	t.Helper()
	leakcheck.Track(t)
	h := &harness{clk: clock.New(), pairs: make(chan pair, 1), done: make(chan error, 1)}
	h.opts = wire.Options{Clock: h.clk, DisableHeartbeat: true, MaxMessageBytes: 1 << 20, MaxFragmentBytes: 4096, HeartbeatInterval: time.Second, HeartbeatTimeout: 2 * time.Minute}
	o := daemonlink.Options{Credentials: credfile.File{Role: credfile.RoleDaemon, ControlPlaneURL: "http://127.0.0.1", PrincipalID: "device", Token: "secret"}, Clock: h.clk, Wire: h.opts, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Jitter: func() float64 { return 1 }, Tools: func(*jobs.Manager) []daemon.Tool { return nil }}
	o.Dial = func(ctx context.Context, url string, options *websocket.DialOptions) (wire.Conn, *http.Response, error) {
		if url != "ws://127.0.0.1/v1/link/daemon" || options.HTTPHeader.Get("Authorization") != "Bearer secret" {
			return nil, nil, errors.New("bad endpoint or authorization")
		}
		a, b := wiretest.Pipe(wiretest.Options{Clock: h.clk, BufferMessages: 16})
		raw := wire.Conn(a)
		if wrap != nil {
			raw = wrap(raw)
		}
		select {
		case h.pairs <- pair{a, b, raw}:
			return raw, nil, nil
		case <-ctx.Done():
			_ = a.Close()
			return nil, nil, ctx.Err()
		}
	}
	if change != nil {
		change(&o)
	}
	if o.Tools != nil {
		factory := o.Tools
		o.Tools = func(m *jobs.Manager) []daemon.Tool { return leakcheck.AuditTools(t, h.clk.Now, factory(m)) }
	}
	var err error
	h.client, err = daemonlink.New(o)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() { h.done <- h.client.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(3 * time.Second):
			h.clk.Advance(time.Minute)
			select {
			case <-h.done:
			case <-time.After(time.Second):
				t.Error("client did not shut down")
			}
		}
	})
	return h
}
func (h *harness) handshake(t *testing.T) (*wire.Link, pair, *circusv1.Hello) {
	t.Helper()
	return h.handshakeAt(t, h.clk.Now().UnixMilli())
}
func (h *harness) handshakeAt(t *testing.T, serverTimeUnixMs int64) (*wire.Link, pair, *circusv1.Hello) {
	t.Helper()
	p := recv(t, h.pairs)
	hello := rawHello(t, p.server)
	w := &circusv1.Welcome{PrincipalId: "device", NegotiatedVersion: 1, ServerTimeUnixMs: serverTimeUnixMs}
	o, err := wire.FillWelcome(w, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	data, err := proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Welcome{Welcome: w}})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.server.WriteMessage(context.Background(), data); err != nil {
		t.Fatal(err)
	}
	l, err := wire.New(p.server, wire.DaemonControlSide, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	eventually(t, func() bool { return h.client.Stats().Connected })
	return l, p, hello
}
func rawHello(t *testing.T, raw wire.Conn) *circusv1.Hello {
	t.Helper()
	data, err := raw.ReadMessage(wait.Ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	m, err := wire.Decode(wire.DaemonUplink, data, wire.MaxMetadataBytes)
	if err != nil {
		t.Fatal(err)
	}
	hello := m.(*circusv1.DaemonUplink).GetHello()
	if hello == nil {
		t.Fatal("first message is not Hello")
	}
	return hello
}
func (h *harness) retry(t *testing.T, d time.Duration) {
	t.Helper()
	eventually(t, func() bool { return h.client.Stats().RetryDelay == d })
	h.clk.Advance(d)
}

func TestLostAckResendDoesNotExecuteAgain(t *testing.T) {
	var runs atomic.Int32
	started := make(chan struct{}, 1)
	finish := make(chan struct{})
	returned := make(chan struct{}, 1)
	h := start(t, func(o *daemonlink.Options) {
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(ctx context.Context, c *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				runs.Add(1)
				started <- struct{}{}
				select {
				case <-finish:
				case <-ctx.Done():
				}
				returned <- struct{}{}
				return result("completed"), nil
			}}}
		}
	}, nil)
	l, p, hello := h.handshake(t)
	if id, err := uuid.Parse(hello.BootId); err != nil || id.Version() != 7 || hello.MachineId == "" || hello.Os != runtime.GOOS || hello.Arch != runtime.GOARCH || hello.Hostname == "" {
		t.Fatalf("Hello: %v", hello)
	}
	p.client.SetFaults(wiretest.Faults{Silence: true})
	dispatch(t, l, hello.BootId, call("once"))
	recv(t, started)
	_ = l.Close()
	h.retry(t, 500*time.Millisecond)
	next, _, again := h.handshake(t)
	if again.BootId != hello.BootId {
		t.Fatal("boot changed on reconnect")
	}
	dispatch(t, next, again.BootId, call("once"))
	if a := ack(t, next); a.GetStatus().GetState() != circusv1.CallStatus_STATE_RUNNING {
		t.Fatalf("duplicate running: %v", a)
	}
	close(finish)
	recv(t, returned)
	for receive(t, next).GetResult() == nil {
	}
	dispatch(t, next, again.BootId, call("once"))
	a := ack(t, next)
	if a.GetStatus().GetResult().GetOutput().GetContent()[0].GetText() != "completed" || runs.Load() != 1 {
		t.Fatalf("duplicate completed: %v runs=%d", a, runs.Load())
	}
}

func TestInvalidIDsAndBootMismatchNeverExecute(t *testing.T) {
	var runs atomic.Int32
	h := start(t, func(o *daemonlink.Options) {
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				runs.Add(1)
				return result("unexpected"), nil
			}}}
		}
	}, nil)
	l, _, hello := h.handshake(t)
	for _, boot := range []string{"old-boot", ""} {
		dispatch(t, l, boot, call("wrong-boot"))
		if a := ack(t, l); a.GetRejection().GetCode() != circusv1.Rejection_CODE_BOOT_MISMATCH {
			t.Fatal(a)
		}
	}
	invalid := []string{"", strings.Repeat("x", 129), "a/b", "a.b", "a b", "中文"}
	for _, field := range []string{"call", "session", "agent"} {
		for _, id := range invalid {
			if field == "agent" && id == "" {
				continue
			}
			c := call("invalid")
			switch field {
			case "call":
				c.CallId = id
			case "session":
				c.SessionId = id
			case "agent":
				c.AgentId = id
			}
			dispatch(t, l, hello.BootId, c)
			a := ack(t, l)
			if a.GetRejection().GetCode() != circusv1.Rejection_CODE_INVALID || field == "call" && a.CallId != "" || strings.Contains(a.String(), "file name too long") {
				t.Fatalf("%s %q: %v", field, id, a)
			}
		}
	}
	for _, at := range []int64{0, -1, math.MinInt64} {
		dispatchAt(t, l, hello.BootId, call("invalid"), at)
		if a := ack(t, l); a.GetRejection().GetCode() != circusv1.Rejection_CODE_INVALID {
			t.Fatalf("dispatch time %d: %v", at, a)
		}
	}
	if runs.Load() != 0 {
		t.Fatalf("executed %d invalid calls", runs.Load())
	}
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: "invalid"}}})
	if m := receive(t, l); m.GetCallStatus().GetState() != circusv1.CallStatus_STATE_LOST {
		t.Fatal(m)
	}
}

func TestDispatchAgeWelcomeAnchorAndStaleAck(t *testing.T) {
	var runs atomic.Int32
	h := start(t, func(o *daemonlink.Options) {
		o.Dispatcher.MaxDispatchAge = time.Second
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				runs.Add(1)
				return result("done"), nil
			}}}
		}
	}, nil)
	// The server timestamp is deliberately unrelated to the daemon's wall clock.
	const serverTime = int64(123456)
	l, _, hello := h.handshakeAt(t, serverTime)
	dispatchAt(t, l, hello.BootId, call("fresh"), serverTime)
	if a := ack(t, l); a.GetRejection() != nil {
		t.Fatalf("fresh dispatch rejected using wall-clock skew: %v", a)
	}
	eventually(t, func() bool { return runs.Load() == 1 })
	h.clk.Advance(time.Second)
	dispatchAt(t, l, hello.BootId, call("boundary"), serverTime)
	if a := ack(t, l); a.GetRejection() != nil {
		t.Fatalf("dispatch exactly at MaxDispatchAge rejected: %v", a)
	}
	eventually(t, func() bool { return runs.Load() == 2 })
	h.clk.Advance(time.Millisecond)
	dispatchAt(t, l, hello.BootId, call("stale"), serverTime)
	if a := ack(t, l); a.GetRejection().GetCode() != circusv1.Rejection_CODE_EXPIRED {
		t.Fatalf("monotonic time after Welcome was not included in age: %v", a)
	}
	if runs.Load() != 2 {
		t.Fatalf("stale call executed; runs=%d", runs.Load())
	}
}

func TestDispatchAgeUsesServerTimeDelta(t *testing.T) {
	h := start(t, func(o *daemonlink.Options) { o.Dispatcher.MaxDispatchAge = time.Second }, nil)
	const serverTime = int64(123456)
	l, _, hello := h.handshakeAt(t, serverTime)
	dispatchAt(t, l, hello.BootId, call("stale"), serverTime-1001)
	if a := ack(t, l); a.GetRejection().GetCode() != circusv1.Rejection_CODE_EXPIRED {
		t.Fatalf("stale dispatch: %v", a)
	}
}

// Timestamps whose difference or sum exceeds time.Duration count as stale
// instead of wrapping to an accepted age.
func TestDispatchAgeOutOfRangeIsStale(t *testing.T) {
	const maxMs = int64(math.MaxInt64 / time.Millisecond)
	for _, tc := range []struct {
		name               string
		server, dispatched int64
		advance            time.Duration
	}{
		{"difference beyond Duration", math.MaxInt64, 1, 0},
		{"dispatch far in the future", 1, math.MaxInt64, 0},
		{"monotonic time pushes the sum beyond Duration", maxMs + 1, 1, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var runs atomic.Int32
			h := start(t, func(o *daemonlink.Options) {
				o.Tools = func(*jobs.Manager) []daemon.Tool {
					return []daemon.Tool{tool{func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
						runs.Add(1)
						return result("done"), nil
					}}}
				}
			}, nil)
			l, _, hello := h.handshakeAt(t, tc.server)
			h.clk.Advance(tc.advance)
			dispatchAt(t, l, hello.BootId, call("stale"), tc.dispatched)
			if a := ack(t, l); a.GetRejection().GetCode() != circusv1.Rejection_CODE_EXPIRED {
				t.Fatalf("out-of-range age: %v", a)
			}
			if runs.Load() != 0 {
				t.Fatalf("stale call executed; runs=%d", runs.Load())
			}
		})
	}
}

// Welcome's server time anchors dispatch age; a Welcome without it fails the
// handshake and the client retries.
func TestWelcomeWithoutServerTimeFailsHandshake(t *testing.T) {
	for _, serverTime := range []int64{0, -1} {
		t.Run(fmt.Sprint(serverTime), func(t *testing.T) {
			h := start(t, nil, nil)
			p := recv(t, h.pairs)
			rawHello(t, p.server)
			w := &circusv1.Welcome{PrincipalId: "device", NegotiatedVersion: 1, ServerTimeUnixMs: serverTime}
			if _, err := wire.FillWelcome(w, h.opts); err != nil {
				t.Fatal(err)
			}
			data, err := proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Welcome{Welcome: w}})
			if err != nil {
				t.Fatal(err)
			}
			if err = p.server.WriteMessage(context.Background(), data); err != nil {
				t.Fatal(err)
			}
			h.retry(t, 500*time.Millisecond)
			if s := h.client.Stats(); s.Connections != 0 {
				t.Fatalf("accepted a Welcome without server time: %+v", s)
			}
			h.handshake(t)
		})
	}
}

func TestTerminalRejectionsStopWithoutRetry(t *testing.T) {
	for _, code := range []circusv1.Reject_Code{circusv1.Reject_CODE_UNAUTHENTICATED, circusv1.Reject_CODE_REVOKED, circusv1.Reject_CODE_VERSION_UNSUPPORTED} {
		t.Run(code.String(), func(t *testing.T) {
			h := start(t, nil, nil)
			p := recv(t, h.pairs)
			rawHello(t, p.server)
			data, _ := proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Reject{Reject: &circusv1.Reject{Code: code, MinVersion: 2, MaxVersion: 4}}})
			if err := p.server.WriteMessage(context.Background(), data); err != nil {
				t.Fatal(err)
			}
			err := recv(t, h.done)
			if err == nil || !strings.Contains(err.Error(), strings.TrimPrefix(code.String(), "CODE_")) && code != circusv1.Reject_CODE_VERSION_UNSUPPORTED {
				t.Fatalf("error: %v", err)
			}
			if code == circusv1.Reject_CODE_VERSION_UNSUPPORTED && !strings.Contains(err.Error(), "2..4") {
				t.Fatal(err)
			}
			h.done <- nil
			h.clk.Advance(24 * time.Hour)
			if len(h.pairs) != 0 || h.client.Stats().Connections != 0 {
				t.Fatal("retried terminal rejection")
			}
		})
	}
	for _, status := range []int{401, 403} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var attempts atomic.Int32
			h := start(t, func(o *daemonlink.Options) {
				o.Dial = func(context.Context, string, *websocket.DialOptions) (wire.Conn, *http.Response, error) {
					attempts.Add(1)
					return nil, &http.Response{StatusCode: status}, errors.New("upgrade denied")
				}
			}, nil)
			err := recv(t, h.done)
			h.done <- nil
			h.clk.Advance(time.Hour)
			if err == nil || !strings.Contains(err.Error(), fmt.Sprint(status)) || attempts.Load() != 1 {
				t.Fatalf("attempts=%d error=%v", attempts.Load(), err)
			}
		})
	}
}

// A Welcome the daemon must not use (another principal, another protocol
// version, or a message limit above its local cap) ends Run without retry.
func TestIncompatibleWelcomeStopsWithoutRetry(t *testing.T) {
	for name, edit := range map[string]func(*circusv1.Welcome){
		"principal": func(w *circusv1.Welcome) { w.PrincipalId = "other" },
		"version":   func(w *circusv1.Welcome) { w.NegotiatedVersion = 2 },
		"limit":     func(w *circusv1.Welcome) { w.MaxMessageBytes = 2 * wire.DefaultMaxMessageBytes },
	} {
		t.Run(name, func(t *testing.T) {
			h := start(t, nil, nil)
			p := recv(t, h.pairs)
			rawHello(t, p.server)
			w := &circusv1.Welcome{PrincipalId: "device", NegotiatedVersion: 1, ServerTimeUnixMs: h.clk.Now().UnixMilli()}
			if _, err := wire.FillWelcome(w, h.opts); err != nil {
				t.Fatal(err)
			}
			edit(w)
			data, _ := proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Welcome{Welcome: w}})
			if err := p.server.WriteMessage(context.Background(), data); err != nil {
				t.Fatal(err)
			}
			err := recv(t, h.done)
			if err == nil || !strings.Contains(err.Error(), "incompatible Welcome") {
				t.Fatalf("error: %v", err)
			}
			h.done <- nil
			h.clk.Advance(24 * time.Hour)
			if len(h.pairs) != 0 || h.client.Stats().Connections != 0 {
				t.Fatal("retried an incompatible Welcome")
			}
		})
	}
}

func TestBackoffCapJitterAndStableReset(t *testing.T) {
	h := start(t, nil, nil)
	for _, d := range []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		l, _, _ := h.handshake(t)
		_ = l.Close()
		h.retry(t, d)
	}
	l, _, _ := h.handshake(t)
	h.clk.Advance(60 * time.Second)
	_ = l.Close()
	h.retry(t, 500*time.Millisecond)
	_, _, _ = h.handshake(t)
}
func TestGoAwayDelay(t *testing.T) {
	h := start(t, nil, nil)
	l, _, _ := h.handshake(t)
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{ReconnectAfterMs: 7000}}})
	eventually(t, func() bool { return h.client.Stats().RetryDelay == 7*time.Second })
	h.clk.Advance(6999 * time.Millisecond)
	if len(h.pairs) != 0 {
		t.Fatal("early reconnect")
	}
	h.clk.Advance(time.Millisecond)
	h.handshake(t)
}

func TestGoAwayZeroAndSmallDelaysRespectBackoffFloor(t *testing.T) {
	h := start(t, nil, nil)
	for i, request := range []uint32{0, 1, 1} {
		l, _, _ := h.handshake(t)
		send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{ReconnectAfterMs: request}}})
		h.retry(t, []time.Duration{500 * time.Millisecond, 500 * time.Millisecond, time.Second}[i])
	}
	h.handshake(t)
}

type blockedClose struct {
	wire.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *blockedClose) Shutdown(ctx context.Context) error {
	c.once.Do(func() { close(c.entered) })
	<-ctx.Done()
	_ = c.Conn.Close()
	return ctx.Err()
}
func TestGoAwayCloseObservesOuterCancellation(t *testing.T) {
	entered := make(chan struct{})
	h := start(t, nil, func(c wire.Conn) wire.Conn { return &blockedClose{Conn: c, entered: entered} })
	l, _, _ := h.handshake(t)
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{}}})
	recv(t, entered)
	h.cancel()
	if err := recv(t, h.done); err != nil {
		t.Fatal(err)
	}
	h.done <- nil
}

func TestMachineIDIsCachedAcrossReconnects(t *testing.T) {
	var probes atomic.Int32
	h := start(t, func(o *daemonlink.Options) {
		o.MachineIDSource = func(context.Context) (string, error) { probes.Add(1); return "cached-machine-hash", nil }
	}, nil)
	l, _, hello := h.handshake(t)
	if hello.MachineId != "cached-machine-hash" {
		t.Fatal(hello)
	}
	_ = l.Close()
	h.retry(t, 500*time.Millisecond)
	_, _, again := h.handshake(t)
	if again.MachineId != hello.MachineId || probes.Load() != 1 {
		t.Fatalf("probes=%d hello=%v", probes.Load(), again)
	}
}

func TestMachineIDUnavailableFailsBeforeDial(t *testing.T) {
	for _, probeErr := range []error{nil, errors.New("no initialized machine ID")} {
		var dials atomic.Int32
		_, err := daemonlink.New(daemonlink.Options{Credentials: credfile.File{Role: credfile.RoleDaemon, ControlPlaneURL: "http://127.0.0.1", PrincipalID: "device", Token: "secret"}, MachineIDSource: func(context.Context) (string, error) { return "", probeErr }, Dial: func(context.Context, string, *websocket.DialOptions) (wire.Conn, *http.Response, error) {
			dials.Add(1)
			return nil, nil, errors.New("must not dial")
		}})
		if err == nil || !strings.Contains(err.Error(), "machine ID unavailable at startup") || dials.Load() != 0 {
			t.Fatalf("error=%v dials=%d", err, dials.Load())
		}
	}
}

func TestDispatchAgeAssemblyValidation(t *testing.T) {
	credentials := credfile.File{Role: credfile.RoleDaemon, ControlPlaneURL: "http://127.0.0.1", PrincipalID: "device", Token: "secret"}
	for _, tc := range []struct {
		name                   string
		maxAge, handshake, ttl time.Duration
		ok                     bool
	}{
		{"default age and handshake at boundary", 0, 0, daemon.DefaultMaxDispatchAge + 10*time.Second, false},
		{"default age and handshake just below boundary", 0, 0, daemon.DefaultMaxDispatchAge + 10*time.Second + time.Millisecond, true},
		{"configured values at boundary", time.Hour, 2 * time.Second, time.Hour + 2*time.Second, false},
		{"configured values just below boundary", time.Hour - time.Millisecond, 2 * time.Second, time.Hour + 2*time.Second, true},
		{"configured values beyond boundary", 2 * time.Hour, time.Second, time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := daemonlink.New(daemonlink.Options{Credentials: credentials, HandshakeTimeout: tc.handshake, Dispatcher: daemon.Options{MaxDispatchAge: tc.maxAge, TombstoneTTL: tc.ttl}, MachineIDSource: func(context.Context) (string, error) { return "machine", nil }})
			if tc.ok {
				if err != nil || c == nil {
					t.Fatalf("valid durations rejected: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "max dispatch age") || !strings.Contains(err.Error(), "tombstone TTL") {
				t.Fatalf("validation error: %v", err)
			}
		})
	}
}

func TestShutdownFlushesInterruptedResultAndRejectsNewCalls(t *testing.T) {
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	release := make(chan struct{})
	h := start(t, func(o *daemonlink.Options) {
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				started <- struct{}{}
				<-ctx.Done()
				cancelled <- struct{}{}
				<-release
				return &circusv1.DeviceResult{Detail: &circusv1.DeviceResult_Bash{Bash: &circusv1.BashResult{Interrupted: true}}}, nil
			}}}
		}
	}, nil)
	l, _, hello := h.handshake(t)
	dispatch(t, l, hello.BootId, call("interrupted"))
	recv(t, started)
	ack(t, l)
	h.cancel()
	recv(t, cancelled)
	dispatch(t, l, hello.BootId, call("late"))
	if a := ack(t, l); a.GetRejection().GetCode() != circusv1.Rejection_CODE_SHUTTING_DOWN {
		t.Fatal(a)
	}
	close(release)
	// Deliberately wait for daemon shutdown before consuming its final message.
	// A result the peer already received must survive a normal close.
	if err := recv(t, h.done); err != nil {
		t.Fatal(err)
	}
	h.done <- nil
	m := receive(t, l)
	if !m.GetResult().GetBash().GetInterrupted() {
		t.Fatalf("final result: %v", m)
	}
	if s := h.client.Stats(); s.Connected || !s.Flushing {
		t.Fatal(s)
	}
}

// reorderAck models delayed acknowledgement delivery, buffering at most one
// small frame. Real Dispatcher and both wire endpoints still produce all data.
type reorderAck struct {
	wire.Conn
	mu   sync.Mutex
	held []byte
}

func (c *reorderAck) WriteMessage(ctx context.Context, data []byte) error {
	m, err := wire.Decode(wire.DaemonUplink, data, 1<<20)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	u := m.(*circusv1.DaemonUplink)
	if u.GetCallAck() != nil {
		c.held = append([]byte(nil), data...)
		return nil
	}
	if err = c.Conn.WriteMessage(ctx, data); err != nil {
		return err
	}
	if u.GetResult() != nil && c.held != nil {
		err = c.Conn.WriteMessage(ctx, c.held)
		c.held = nil
	}
	return err
}
func TestResultBeforeAck(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	h := start(t, func(o *daemonlink.Options) {
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(context.Context, *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				started <- struct{}{}
				<-release
				return result("done"), nil
			}}}
		}
	}, func(c wire.Conn) wire.Conn { return &reorderAck{Conn: c} })
	l, p, hello := h.handshake(t)
	dispatch(t, l, hello.BootId, call("order"))
	recv(t, started)
	adapter := p.raw.(*reorderAck)
	eventually(t, func() bool { adapter.mu.Lock(); defer adapter.mu.Unlock(); return adapter.held != nil })
	close(release)
	if m := receive(t, l); m.GetResult().GetCallId() != "order" {
		t.Fatalf("result first: %v", m)
	}
	if m := receive(t, l); m.GetCallAck().GetCallId() != "order" {
		t.Fatalf("ack second: %v", m)
	}
}

func TestSlowUplinkDisconnectsAndRecoversByQuery(t *testing.T) {
	const count = 12
	release := make(chan struct{})
	var runs, finished atomic.Int32
	h := start(t, func(o *daemonlink.Options) {
		o.Wire.MaxQueuedBytes = 16384
		o.Wire.MaxQueuedMessages = 128
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(ctx context.Context, c *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				runs.Add(1)
				select {
				case <-release:
				case <-ctx.Done():
				}
				finished.Add(1)
				return result(strings.Repeat(c.CallId, 500)), nil
			}}}
		}
	}, nil)
	l, p, hello := h.handshake(t)
	p.client.SetFaults(wiretest.Faults{BlockWrites: true})
	for i := 0; i < count; i++ {
		dispatch(t, l, hello.BootId, call(fmt.Sprintf("call-%02d", i)))
	}
	eventually(t, func() bool { return runs.Load() == count })
	close(release)
	eventually(t, func() bool { s := h.client.Stats(); return !s.Connected && s.RetryDelay != 0 })
	eventually(t, func() bool { return finished.Load() == count && h.client.Stats().Results == count })
	s := h.client.Stats()
	if s.Overflows == 0 || s.Dropped == 0 {
		t.Fatalf("unbounded/undetected overflow: %+v", s)
	}
	h.retry(t, 500*time.Millisecond)
	next, _, hello := h.handshake(t)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("call-%02d", i)
		send(t, next, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: id}}})
		for {
			m := receive(t, next)
			if status := m.GetCallStatus(); status != nil {
				if status.State != circusv1.CallStatus_STATE_COMPLETED || status.CallId != id || status.Result.GetOutput().GetContent()[0].GetText() != strings.Repeat(id, 500) {
					t.Fatalf("recovery: %v", status)
				}
				break
			}
		}
		dispatch(t, next, hello.BootId, call(id))
		if a := ack(t, next); a.GetStatus().GetState() != circusv1.CallStatus_STATE_COMPLETED {
			t.Fatal(a)
		}
	}
	if runs.Load() != count {
		t.Fatalf("re-executed %d calls", runs.Load())
	}
}

type backgroundJob struct {
	done chan struct{}
	once sync.Once
}

func (j *backgroundJob) Wait() (int, error) { <-j.done; return 143, jobs.ErrKilled }
func (j *backgroundJob) Stop() error        { j.once.Do(func() { close(j.done) }); return nil }
func TestJobEventBeforeResultAndSessionStop(t *testing.T) {
	release := make(chan struct{})
	job := &backgroundJob{done: make(chan struct{})}
	h := start(t, func(o *daemonlink.Options) {
		o.Tools = func(manager *jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(ctx context.Context, c *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				_, err := manager.Start(c.SessionId, c.CallId, "command", func(string) (jobs.Job, string, error) { return job, "/output", nil })
				if err != nil {
					return nil, err
				}
				select {
				case <-release:
				case <-ctx.Done():
				}
				return result("backgrounded"), nil
			}}}
		}
	}, nil)
	l, _, hello := h.handshake(t)
	dispatch(t, l, hello.BootId, call("creator"))
	var running *circusv1.JobEvent
	for running == nil {
		m := receive(t, l)
		if m.GetResult() != nil {
			t.Fatal("result preceded job")
		}
		running = m.GetJobEvent()
	}
	if running.CallId != "creator" || running.Seq != 1 || running.State != circusv1.JobEvent_STATE_RUNNING {
		t.Fatal(running)
	}
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_ListJobs{ListJobs: &circusv1.ListJobs{SessionId: "session"}}})
	for {
		m := receive(t, l)
		if snap := m.GetJobSnapshot(); snap != nil {
			if snap.Seq != 1 || len(snap.Jobs) != 1 || !proto.Equal(snap.Jobs[0], running) {
				t.Fatal(snap)
			}
			break
		}
	}
	close(release)
	for receive(t, l).GetResult() == nil {
	}
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_StopSessionJobs{StopSessionJobs: &circusv1.StopSessionJobs{SessionId: "session"}}})
	m := receive(t, l)
	ended := m.GetJobEvent()
	if ended.GetSeq() != 2 || ended.GetCallId() != "creator" || ended.GetState() != circusv1.JobEvent_STATE_KILLED {
		t.Fatal(m)
	}
	send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_ListJobs{ListJobs: &circusv1.ListJobs{}}})
	if snap := receive(t, l).GetJobSnapshot(); snap.GetSeq() != 2 || len(snap.Jobs) != 1 || snap.Jobs[0].State != circusv1.JobEvent_STATE_KILLED {
		t.Fatal(snap)
	}
}

func TestCallMismatchExpiryAndIdempotentCancel(t *testing.T) {
	h := start(t, func(o *daemonlink.Options) {
		o.Dispatcher.ResultTTL = time.Second
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				<-ctx.Done()
				return result("cancelled"), nil
			}}}
		}
	}, nil)
	l, _, hello := h.handshake(t)
	dispatch(t, l, hello.BootId, call("cancel"))
	ack(t, l)
	mismatch := call("cancel")
	mismatch.GetRead().Path = "different"
	dispatch(t, l, hello.BootId, mismatch)
	if a := ack(t, l); a.GetRejection().GetCode() != circusv1.Rejection_CODE_CALL_MISMATCH {
		t.Fatal(a)
	}
	for _, id := range []string{"unknown", "cancel", "cancel"} {
		send(t, l, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CancelCall{CancelCall: &circusv1.CancelCall{CallId: id}}})
	}
	for receive(t, l).GetResult() == nil {
	}
	h.clk.Advance(time.Second)
	dispatch(t, l, hello.BootId, call("cancel"))
	if a := ack(t, l); a.GetRejection().GetCode() != circusv1.Rejection_CODE_EXPIRED {
		t.Fatal(a)
	}
}
func TestHandshakeTimeoutAndJitter(t *testing.T) {
	h := start(t, func(o *daemonlink.Options) { o.Jitter = func() float64 { return 0 } }, nil)
	p := recv(t, h.pairs)
	rawHello(t, p.server)
	h.clk.Advance(10 * time.Second)
	h.retry(t, 250*time.Millisecond)
	h.handshake(t)
}
func TestShutdownStalledUplinkIsBounded(t *testing.T) {
	started := make(chan struct{}, 1)
	h := start(t, func(o *daemonlink.Options) {
		o.Tools = func(*jobs.Manager) []daemon.Tool {
			return []daemon.Tool{tool{func(ctx context.Context, _ *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
				started <- struct{}{}
				<-ctx.Done()
				return result("interrupted"), nil
			}}}
		}
	}, nil)
	l, p, hello := h.handshake(t)
	p.client.SetFaults(wiretest.Faults{BlockWrites: true})
	dispatch(t, l, hello.BootId, call("blocked"))
	recv(t, started)
	h.cancel()
	eventually(t, func() bool { return h.client.Stats().ShuttingDown })
	h.clk.Advance(16 * time.Second)
	if err := recv(t, h.done); err == nil {
		t.Fatal("stalled shutdown unexpectedly succeeded")
	}
	h.done <- nil
	if s := h.client.Stats(); s.Connected || !s.Flushing {
		t.Fatal(s)
	}
}
