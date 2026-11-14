// Package link runs the daemon's single outbound control-plane connection.
//
// State: dial -> raw Hello/Welcome -> connected -> backoff -> dial. HTTP 401/403,
// any Reject and an incompatible Welcome (principal, version or message limit)
// terminate Run. Ordinary failures use equal jitter in
// [base/2, base]; base doubles from 500ms to 30s, and a connection lasting 60s
// resets it. GoAway supplies the next delay, but never less than base/2, so a
// zero or tiny delay cannot cause a tight reconnect loop. BootID never changes
// on reconnect.
//
// A DispatchCall carries its original control-plane commit time. Each Welcome
// anchors that timestamp to this connection's monotonic clock. The dispatcher
// deduplicates known IDs first, then refuses an unseen call older than
// MaxDispatchAge (12 hours by default) without recording it. This bound makes
// same-boot retransmission safe without assuming a minimum link throughput.
//
// Cancellation (daemon shutdown): Dispatcher.Submit starts rejecting call IDs it has never
// seen (SHUTTING_DOWN) while resends of known IDs still report their status;
// the Dispatcher and jobs shut down within ShutdownTimeout-FlushReserve; then
// wire.Shutdown writes what was handed to it, including the final interrupted
// results, within the remaining budget, so results already produced still reach the
// control plane over a graceful close. Run returns within
// ShutdownTimeout plus at most readerJoinTimeout.
//
// Uplink memory. The client keeps no uplink buffer. wire.Send only marshals
// and enqueues: it never waits for I/O or budget. So every producer calls it
// directly, and the uplink is bounded by the producers' own queues plus wire's:
//
//   - Dispatcher callback goroutine (OnResult): queue <= CallbackByteLimit
//     (default 128 MiB) encoded / 4096 results, plus one result in delivery
//     (encoded + decoded, conservatively <= 3M + 6 MiB + 4 KiB);
//   - jobs callback goroutine (OnEvent): queue <= 8 MiB / 4096 events, plus one
//     <= 64 KiB event in delivery;
//   - reader: one reply at a time, a CallAck/CallStatus clone (<= 3M + 6 MiB +
//     4 KiB) or a JobSnapshot (<= 8 MiB encoded, 4096 entries);
//   - wire: its send queue (MaxQueuedBytes, default 2M) within its documented
//     per-endpoint bound (about 736 MiB at defaults, receive side included).
//
// M is the negotiated message limit (default 64 MiB). Excluding wire, that is
// 136 MiB + 2 x (3M + 6 MiB + 4 KiB), about 532 MiB at defaults. These are
// live-ownership bounds, not RSS; retained results and jobs (TTL-bounded, no
// count cap), kernel/TLS buffers, runtime and GC garbage are excluded.
//
// Neither callback blocks Dispatcher or jobs state: their queues absorb a slow
// Send. A callback queue overflow or a refused Send (wire queue full, busy)
// closes the connection, and the control plane resyncs by query and job
// snapshot. Reader replies follow the same rule: a CallStatus,
// CallAck or JobSnapshot that wire's send queue cannot take (default 256
// messages or 2M bytes) closes the connection too, so the control plane must
// bound how many CallQuery/ListJobs requests it has outstanding during resync,
// or a large resync batch disconnects on every reconnect. Disconnected
// notifications are dropped; there is no outbox.
package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/loopback"
	"github.com/EricWvi/circus/internal/machineid"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
	"github.com/google/uuid"
)

// DialFunc permits transport fault injection; production uses wire.DialWebSocket.
type DialFunc func(context.Context, string, *websocket.DialOptions) (wire.Conn, *http.Response, error)

// Options configures the client and its owned execution components.
type Options struct {
	Credentials   credfile.File
	ClientVersion string
	Clock         clock.Clock
	Logger        *slog.Logger
	// Tools constructs tools using this boot's job manager; nil uses DefaultTools.
	Tools                             func(*jobs.Manager) []daemon.Tool
	Dispatcher                        daemon.Options
	Wire                              wire.Options
	Dial                              DialFunc
	HandshakeTimeout, ShutdownTimeout time.Duration
	// ShutdownTimeout (default 16s) bounds graceful shutdown. FlushReserve
	// (default min(3s, ShutdownTimeout/3)) is its final slice, kept for the
	// last wire.Shutdown; Dispatcher and jobs cleanup get the rest.
	FlushReserve time.Duration
	// MachineIDSource supplies the platform hash once at startup. Nil uses the OS.
	MachineIDSource func(context.Context) (string, error)
	// Jitter returns a value in [0,1]; nil uses rand.Float64.
	Jitter func() float64
}

// Stats reports connection lifecycle diagnostics; it retains no payloads.
type Stats struct {
	Connected, ShuttingDown bool
	// Flushing is set once Dispatcher and jobs cleanup has ended and the final
	// wire.Shutdown is writing what remains.
	Flushing    bool
	Connections uint64
	// Overflows counts uplinks refused for capacity: a Dispatcher or jobs
	// callback queue overflow, or wire.Send reporting queue full, busy or too
	// large. Each closes the connection.
	Overflows uint64
	// Dropped counts uplinks discarded without a connection or refused by wire.
	Dropped uint64
	// Results counts result notifications, including dropped ones.
	Results    uint64
	RetryDelay time.Duration
	Wire       wire.Stats
}

type connection struct {
	link *wire.Link
	// maxUplink is the largest envelope an idle link accepts; larger results
	// become placeholders instead of failing the connection on every resync.
	maxUplink       int
	ctx             context.Context // ends the reader's Receive
	cancel          context.CancelFunc
	abort           chan struct{} // closed once, by fail
	failOnce        sync.Once
	failErr         error // written before abort closes
	readerDone      chan struct{}
	started         time.Time
	welcomeMono     time.Time
	welcomeServerMs int64
}

// fail asks serve to close the connection. It never blocks, so it may run
// under Dispatcher or jobs locks.
func (a *connection) fail(err error) {
	a.failOnce.Do(func() { a.failErr = err; close(a.abort) })
}

// readerJoinTimeout bounds waiting for a reader after its Receive is cancelled.
// Every reader branch is non-blocking, so this only covers scheduling latency.
const readerJoinTimeout = time.Second

var errCallbackOverflow = errors.New("daemon link: notification queue overflow")

// Client owns a Dispatcher, job manager and at most one active link.
// Run must be called exactly once. Construct one Client per process start.
type Client struct {
	opts       Options
	bootID     string
	dispatcher *daemon.Dispatcher
	jobs       *jobs.Manager
	http       *http.Client
	endpoint   string
	machineID  string
	hostname   string
	mu         sync.Mutex // guards stats; never held while calling other components
	stats      Stats
	running    atomic.Bool
	stopping   atomic.Bool
	live       atomic.Pointer[connection]
}

// New validates credentials/TLS policy and creates a fresh UUIDv7 boot.
func New(o Options) (*Client, error) {
	if o.Credentials.Role != credfile.RoleDaemon || o.Credentials.Token == "" || o.Credentials.PrincipalID == "" {
		return nil, errors.New("daemon link: daemon credentials required")
	}
	endpoint, httpClient, err := loopback.Link(o.Credentials.ControlPlaneURL, o.Credentials.CAFile, "/v1/link/daemon")
	if err != nil {
		return nil, fmt.Errorf("daemon link: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("daemon boot ID: %w", err)
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Jitter == nil {
		o.Jitter = rand.Float64
	}
	if o.HandshakeTimeout <= 0 {
		o.HandshakeTimeout = 10 * time.Second
	}
	if o.Dispatcher.MaxDispatchAge <= 0 {
		o.Dispatcher.MaxDispatchAge = daemon.DefaultMaxDispatchAge
	}
	if o.Dispatcher.TombstoneTTL <= 0 {
		o.Dispatcher.TombstoneTTL = daemon.DefaultTombstoneTTL
	}
	// This constructor is the assembly point that knows the link handshake
	// budget and both dispatcher durations. Keep two accepted copies closer
	// together than the tombstone lifetime, including Welcome transit time.
	if o.Dispatcher.MaxDispatchAge >= o.Dispatcher.TombstoneTTL-o.HandshakeTimeout {
		return nil, fmt.Errorf("daemon link: max dispatch age %s plus handshake timeout %s must be less than tombstone TTL %s", o.Dispatcher.MaxDispatchAge, o.HandshakeTimeout, o.Dispatcher.TombstoneTTL)
	}
	if o.ShutdownTimeout <= 0 {
		o.ShutdownTimeout = 16 * time.Second
	}
	if o.FlushReserve == 0 {
		o.FlushReserve = min(3*time.Second, o.ShutdownTimeout/3)
	}
	if o.FlushReserve <= 0 || o.FlushReserve >= o.ShutdownTimeout {
		return nil, errors.New("daemon link: flush reserve must be between zero and shutdown timeout")
	}
	// Budget split at the defaults (16s = 13s cleanup + 3s flush reserve).
	// Cleanup cancels calls at t=0; a call ignoring cancellation is abandoned
	// at daemon.AbandonGrace (5s); only then do tool Shutdown hooks run, each
	// bounded by Dispatcher.ToolShutdownTimeout. Defaulting that bound to
	// cleanup-AbandonGrace (8s) lets a hook finish before the reserve starts.
	// Bash's hook needs at most about 7s: a 2s SIGTERM grace, then at most 5s
	// waiting for a killed process group to empty (bashtool groupExitWait).
	// Visible background jobs stop concurrently from t=0 (jobs.Shutdown).
	cleanup := o.ShutdownTimeout - o.FlushReserve
	if o.Dispatcher.ToolShutdownTimeout <= 0 {
		o.Dispatcher.ToolShutdownTimeout = max(cleanup-daemon.AbandonGrace, cleanup/2)
	}
	if o.MachineIDSource == nil {
		// The shared machine-role hash, so the daemon's Hello matches the
		// machine_id `circus join` enrolled with.
		o.MachineIDSource = func(context.Context) (string, error) { return machineid.Get() }
	}
	probe, cancelProbe := context.WithTimeout(context.Background(), o.HandshakeTimeout)
	machine, err := o.MachineIDSource(probe)
	cancelProbe()
	if err != nil || machine == "" {
		return nil, fmt.Errorf("daemon link: machine ID unavailable at startup: %w", errors.Join(errors.New("cannot identify this machine"), err))
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("daemon link: hostname: %w", err)
	}
	if o.Dial == nil {
		o.Dial = func(ctx context.Context, url string, options *websocket.DialOptions) (wire.Conn, *http.Response, error) {
			return wire.DialWebSocket(ctx, url, options)
		}
	}
	c := &Client{opts: o, bootID: id.String(), endpoint: endpoint, http: httpClient, machineID: machine, hostname: hostname}
	c.jobs = jobs.NewManager(jobs.Options{Clock: o.Clock, OnEvent: func(e *circusv1.JobEvent) {
		c.send(c.live.Load(), &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobEvent{JobEvent: e}})
	}, OnOverflow: c.overflow})
	d := o.Dispatcher
	d.Clock = o.Clock
	if o.Tools != nil {
		d.Tools = o.Tools(c.jobs)
	} else if d.Tools == nil {
		d.Tools = daemon.DefaultTools(c.jobs, bashtool.Options{})
	}
	d.OnResult = func(r *circusv1.DeviceResult) {
		c.send(c.live.Load(), &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: r}})
	}
	d.OnOverflow = c.overflow
	c.dispatcher, err = daemon.NewDispatcher(d)
	if err != nil {
		_ = c.jobs.Shutdown(context.Background())
		return nil, err
	}
	return c, nil
}

// BootID is the in-memory process epoch used to fence every DispatchCall.
func (c *Client) BootID() string { return c.bootID }

// Stats returns a snapshot of the lifecycle counters.
func (c *Client) Stats() Stats {
	c.mu.Lock()
	s := c.stats
	c.mu.Unlock()
	s.ShuttingDown = c.stopping.Load()
	if a := c.live.Load(); a != nil {
		s.Connected = true
		s.Wire = a.link.Stats()
	}
	return s
}

// overflow runs under Dispatcher or jobs locks; it only signals serve.
func (c *Client) overflow() {
	c.mu.Lock()
	c.stats.Overflows++
	c.mu.Unlock()
	if a := c.live.Load(); a != nil {
		a.fail(errCallbackOverflow)
	}
}

// send hands one uplink to wire on connection a (nil drops it). wire.Send only
// marshals and enqueues, so callers never wait on the network. Any refusal
// except a link already shutting down closes the connection for resync.
func (c *Client) send(a *connection, m *circusv1.DaemonUplink) {
	if m.GetResult() != nil {
		c.mu.Lock()
		c.stats.Results++
		c.mu.Unlock()
	}
	err := errors.New("daemon link: not connected")
	if a != nil {
		m = fitUplink(m, a.maxUplink)
		err = a.link.Send(context.Background(), m)
	}
	if err == nil {
		return
	}
	c.mu.Lock()
	c.stats.Dropped++
	if errors.Is(err, wire.ErrQueueFull) || errors.Is(err, wire.ErrBusy) || errors.Is(err, wire.ErrTooLarge) {
		c.stats.Overflows++
	}
	c.mu.Unlock()
	if a != nil && !errors.Is(err, wire.ErrClosing) {
		a.fail(fmt.Errorf("daemon link: uplink refused: %w", err))
	}
}

// detach makes a no longer receive callback notifications.
func (c *Client) detach(a *connection) { c.live.CompareAndSwap(a, nil) }

type terminalError struct{ message string }

func (e *terminalError) Error() string { return e.message }
func rejectError(r *circusv1.Reject) error {
	if r.Code == circusv1.Reject_CODE_VERSION_UNSUPPORTED {
		return &terminalError{fmt.Sprintf("daemon link: protocol version unsupported; server supports %d..%d", r.MinVersion, r.MaxVersion)}
	}
	return &terminalError{fmt.Sprintf("daemon link: handshake rejected (%s); check daemon credentials", r.Code)}
}

// Run maintains the link until ctx cancellation or a terminal rejection.
// Cancellation completes graceful shutdown and returns its error, if any.
func (c *Client) Run(ctx context.Context) (err error) {
	if !c.running.CompareAndSwap(false, true) {
		return errors.New("daemon link: Run already called")
	}
	defer c.http.CloseIdleConnections()
	defer func() {
		if !c.stopping.Load() {
			err = errors.Join(err, c.stop(nil))
		}
	}()
	base := 500 * time.Millisecond
	for ctx.Err() == nil {
		a, connectErr := c.connect(ctx)
		delay := time.Duration(0)
		if connectErr == nil {
			start := a.started
			delay, connectErr = c.serve(ctx, a)
			if c.stopping.Load() {
				return connectErr
			}
			if c.opts.Clock.Now().Sub(start) >= 60*time.Second {
				base = 500 * time.Millisecond
			}
		}
		var fatal *terminalError
		if errors.As(connectErr, &fatal) {
			return fatal
		}
		if ctx.Err() != nil {
			break
		}
		jitter := min(1.0, max(0.0, c.opts.Jitter()))
		// A GoAway cannot bypass the current retry floor, even with a zero delay.
		if delay > 0 {
			delay = max(delay, base/2)
		} else {
			delay = time.Duration(float64(base) * (0.5 + 0.5*jitter))
		}
		base = min(30*time.Second, base*2)
		retry := c.opts.Clock.After(delay)
		c.opts.Logger.Warn("daemon link disconnected", "error", connectErr, "retry_after", delay)
		c.mu.Lock()
		c.stats.RetryDelay = delay
		c.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-retry:
		}
	}
	return c.stop(nil)
}

func (c *Client) connect(ctx context.Context) (*connection, error) {
	handshake, cancel := clock.WithTimeout(ctx, c.opts.Clock, c.opts.HandshakeTimeout)
	defer cancel()
	raw, response, err := c.opts.Dial(handshake, c.endpoint, &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + c.opts.Credentials.Token}}})
	if err != nil {
		if response != nil && (response.StatusCode == 401 || response.StatusCode == 403) {
			return nil, &terminalError{fmt.Sprintf("daemon link: HTTP %d authentication rejected; rejoin or replace revoked credentials", response.StatusCode)}
		}
		return nil, err
	}
	accepted := false
	defer func() {
		if !accepted {
			_ = raw.Close()
		}
	}()
	hello := &circusv1.Hello{ProtocolVersion: wire.ProtocolVersion, BootId: c.bootID, ClientVersion: c.opts.ClientVersion, Os: runtime.GOOS, Arch: runtime.GOARCH, Hostname: c.hostname, MachineId: c.machineID}
	welcome, options, err := wire.DialHello(handshake, raw, wire.DaemonUplink, hello, c.opts.Credentials.PrincipalID, c.opts.Wire)
	var rejected *wire.RejectError
	switch {
	case errors.As(err, &rejected):
		return nil, rejectError(rejected.Reject)
	case errors.Is(err, wire.ErrIncompatible):
		return nil, &terminalError{"daemon link: " + err.Error()}
	case err != nil:
		return nil, err
	}
	welcomeMono := c.opts.Clock.Now()
	if welcome.ServerTimeUnixMs <= 0 {
		// The dispatch-age anchor; without it no age bound can be enforced.
		return nil, errors.New("daemon link: Welcome has no server time")
	}
	options.Clock = c.opts.Clock
	l, err := wire.New(raw, wire.DaemonSide, options)
	if err != nil {
		return nil, err
	}
	accepted = true
	runCtx, cancelRun := context.WithCancel(context.Background())
	a := &connection{
		link:            l,
		maxUplink:       min(options.MaxMessageBytes, options.MaxQueuedBytes, max(options.MaxFragmentBytes, options.MaxInFlightBytes)),
		ctx:             runCtx,
		cancel:          cancelRun,
		abort:           make(chan struct{}),
		readerDone:      make(chan struct{}),
		started:         welcomeMono,
		welcomeMono:     welcomeMono,
		welcomeServerMs: welcome.ServerTimeUnixMs,
	}
	// Reset retry state before publishing a, so Stats never reports a connected
	// link together with the previous retry delay.
	c.mu.Lock()
	c.stats.Connections++
	c.stats.RetryDelay = 0
	c.mu.Unlock()
	c.live.Store(a)
	return a, nil
}
func (c *Client) reader(a *connection) (time.Duration, error) {
	for {
		msg, err := a.link.Receive(a.ctx)
		if err != nil {
			return 0, err
		}
		d := msg.(*circusv1.DaemonDownlink)
		switch p := d.Payload.(type) {
		case *circusv1.DaemonDownlink_DispatchCall:
			ack := &circusv1.CallAck{CallId: p.DispatchCall.GetCall().GetCallId()}
			if !identifier.Valid(ack.CallId, false) {
				ack.CallId = ""
			} // Never echo unbounded invalid input into metadata.
			code := circusv1.Rejection_CODE_UNSPECIFIED
			var status *circusv1.CallStatus
			switch {
			case p.DispatchCall.GetTargetBootId() != c.bootID:
				code = circusv1.Rejection_CODE_BOOT_MISMATCH
			case p.DispatchCall.GetDispatchedAtUnixMs() <= 0:
				// INVALID settles the call as never executed. That is safe:
				// every copy of one dispatch attempt carries the same committed
				// timestamp, so if one copy is refused here, all copies are.
				code = circusv1.Rejection_CODE_INVALID
			default:
				var err error
				age := dispatchAge(c.opts.Clock.Now().Sub(a.welcomeMono), a.welcomeServerMs, p.DispatchCall.GetDispatchedAtUnixMs())
				status, err = c.dispatcher.SubmitDispatch(p.DispatchCall.GetCall(), age)
				switch {
				case err == nil:
				case errors.Is(err, daemon.ErrCallMismatch):
					code = circusv1.Rejection_CODE_CALL_MISMATCH
				case errors.Is(err, daemon.ErrCallExpired):
					code = circusv1.Rejection_CODE_EXPIRED
				case errors.Is(err, daemon.ErrShutdown):
					code = circusv1.Rejection_CODE_SHUTTING_DOWN
				default:
					code = circusv1.Rejection_CODE_INVALID
				}
			}
			if code != circusv1.Rejection_CODE_UNSPECIFIED {
				ack.Outcome = &circusv1.CallAck_Rejection{Rejection: &circusv1.Rejection{Code: code}}
			} else {
				ack.Outcome = &circusv1.CallAck_Status{Status: status}
			}
			c.send(a, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallAck{CallAck: ack}})
		case *circusv1.DaemonDownlink_CancelCall:
			c.dispatcher.Cancel(p.CancelCall.GetCallId())
		case *circusv1.DaemonDownlink_CallQuery:
			c.send(a, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: c.dispatcher.Query(p.CallQuery.GetCallId())}})
		case *circusv1.DaemonDownlink_ListJobs:
			// Cap the reply to what this link accepts, so an oversize snapshot is
			// truncated instead of failing the connection on every resync.
			snapshot, err := c.jobs.SnapshotSession(p.ListJobs.GetSessionId(), a.maxUplink)
			if err != nil {
				c.opts.Logger.Warn("incomplete job snapshot", "error", err, "seq", snapshot.Seq)
			}
			c.send(a, &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_JobSnapshot{JobSnapshot: snapshot}})
		case *circusv1.DaemonDownlink_StopSessionJobs:
			c.jobs.RequestStopSession(p.StopSessionJobs.GetSessionId())
		case *circusv1.DaemonDownlink_GoAway:
			delay := time.Duration(p.GoAway.GetReconnectAfterMs()) * time.Millisecond
			return delay, nil
		case *circusv1.DaemonDownlink_Heartbeat:
		default:
			return 0, errors.New("daemon link: unexpected post-handshake message")
		}
	}
}
func (c *Client) serve(ctx context.Context, a *connection) (time.Duration, error) {
	type readResult struct {
		delay time.Duration
		err   error
	}
	reads := make(chan readResult, 1)
	go func() { defer close(a.readerDone); delay, err := c.reader(a); reads <- readResult{delay, err} }()
	var r readResult
	select {
	case <-ctx.Done():
		return 0, c.stop(a)
	case r = <-reads:
	case <-a.abort:
		r.err = a.failErr
	}
	c.detach(a)
	if r.err == nil {
		// GoAway: write what wire accepted. Outer cancellation still bounds it.
		closing, cancel := clock.WithTimeout(ctx, c.opts.Clock, c.opts.ShutdownTimeout)
		r.err = a.link.Shutdown(closing)
		cancel()
	} else {
		_ = a.link.Close()
	}
	r.err = errors.Join(r.err, c.joinReader(a))
	if ctx.Err() != nil {
		return r.delay, c.stop(nil)
	}
	return r.delay, r.err
}

// joinReader cancels a's Receive and waits, bounded, for the reader to return.
func (c *Client) joinReader(a *connection) error {
	a.cancel()
	join, cancel := clock.WithTimeout(context.Background(), c.opts.Clock, readerJoinTimeout)
	defer cancel()
	return waitDone(join, a.readerDone)
}

// stop shuts down execution, then the link. Cleanup gets the budget minus
// FlushReserve; the reserve is left for the final wire.Shutdown. The reader
// keeps answering during cleanup, so resent known calls report their status.
func (c *Client) stop(a *connection) error {
	total, cancel := clock.WithTimeout(context.Background(), c.opts.Clock, c.opts.ShutdownTimeout)
	defer cancel()
	cleanup, cancelCleanup := clock.WithTimeout(total, c.opts.Clock, c.opts.ShutdownTimeout-c.opts.FlushReserve)
	c.stopping.Store(true)
	jobsDone := make(chan error, 1)
	go func() { jobsDone <- c.jobs.Shutdown(cleanup) }()
	err := c.dispatcher.Shutdown(cleanup)
	err = errors.Join(err, <-jobsDone) // bounded: Shutdown returns when cleanup ends
	cancelCleanup()
	if a == nil {
		return err
	}
	// A nil Dispatcher.Shutdown means its final OnResult calls have already
	// handed the interrupted results to wire; Shutdown writes them.
	c.mu.Lock()
	c.stats.Flushing = true
	c.mu.Unlock()
	c.detach(a)
	err = errors.Join(err, a.link.Shutdown(total))
	return errors.Join(err, c.joinReader(a))
}

// withTimeout uses the shared clock and joins its one timer watcher on cancel.
// waitDone checks completion before cancellation so a finished worker does not
// spuriously lose a race with an expired shutdown context.
func waitDone(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// maxAgeMs is the largest millisecond count a time.Duration can hold.
const maxAgeMs = int64(math.MaxInt64 / time.Millisecond)

// dispatchAge is how long ago the control plane committed a dispatch: the
// monotonic time since Welcome plus Welcome's server time minus the dispatch
// timestamp. Both timestamps are positive, so their difference cannot wrap.
// A result outside the range of time.Duration saturates to the maximum, which
// is always stale.
func dispatchAge(sinceWelcome time.Duration, welcomeServerMs, dispatchedMs int64) time.Duration {
	diff := welcomeServerMs - dispatchedMs
	if diff > maxAgeMs || diff < -maxAgeMs {
		return math.MaxInt64
	}
	d := time.Duration(diff) * time.Millisecond
	if (sinceWelcome > 0 && d > math.MaxInt64-sinceWelcome) || (sinceWelcome < 0 && d < math.MinInt64-sinceWelcome) {
		return math.MaxInt64
	}
	return sinceWelcome + d
}
