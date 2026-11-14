package calls_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/storetest"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

// Mid-flight faults of TestRandomSequences. Every daemon socket is wrapped
// in a seqConn, which sees each wire frame (messages here are far below the
// fragment size, so one frame is one message) and can cut, drop or hold the
// link at a message boundary. A fault is armed while the system is idle and
// fires on the frame the next operation produces, so runs stay
// deterministic.
//
// Heartbeats are disabled in the harness: a fake-clock jump of more than the
// heartbeat timeout would expire every link at once. A half-open link is
// therefore modelled as a dropped direction that the sequence cuts once
// shared time has passed the heartbeat timeout (15 s), which is what either
// side's heartbeat check would do.

// seqFaultKind names the armed fault of a daemon.
type seqFaultKind string

const (
	faultDropDispatch seqFaultKind = "drop-dispatch"  // the DispatchCall never reaches the daemon; cut
	faultDropAck      seqFaultKind = "drop-ack"       // the daemon executes, its CallAck is lost; cut
	faultDropResult   seqFaultKind = "drop-result"    // the DeviceResult is lost; cut
	faultResultClose  seqFaultKind = "result-close"   // the DeviceResult is written, then a graceful close
	faultHalfOpenDown seqFaultKind = "half-open-down" // downlink frames vanish until the cut
	faultHalfOpenUp   seqFaultKind = "half-open-up"   // uplink frames vanish until the cut
	faultCorrupt      seqFaultKind = "corrupt-target" // the DispatchCall arrives naming another boot
	faultStaleResend  seqFaultKind = "stale-resend"   // the target call's resend arrives older than MaxDispatchAge
	heartbeatTimeout               = 15 * time.Second // wire's default HeartbeatTimeout
)

// seqFault is one armed fault; since is when a half-open fault began.
// A half-open fault belongs to the connection it was armed on: the cut that
// ends it also ends the fault.
type seqFault struct {
	kind   seqFaultKind
	since  time.Time
	conn   *seqConn
	target string // the only call a stale-resend fault rewrites
}

func (f *seqFault) halfOpen() bool { return f.kind == faultHalfOpenDown || f.kind == faultHalfOpenUp }

// seqDrop records a DispatchCall that never reached its daemon.
type seqDrop struct {
	incarnation int
	at          time.Time
	conn        *seqConn
}

// seqConn wraps one daemon socket. A pump goroutine reads frames, so a
// frame can be injected (a misdelivered DispatchCall) ahead of real ones.
type seqConn struct {
	wire.Conn
	d         *seqDaemon
	frames    chan seqFrame
	inject    chan []byte
	closed    chan struct{}
	closeOnce sync.Once
}

type seqFrame struct {
	data []byte
	err  error
}

func newSeqConn(d *seqDaemon, inner wire.Conn) *seqConn {
	c := &seqConn{Conn: inner, d: d, frames: make(chan seqFrame), inject: make(chan []byte, 1), closed: make(chan struct{})}
	d.dev.w.pumps.Add(1)
	go c.pump()
	return c
}

func (c *seqConn) pump() {
	defer c.d.dev.w.pumps.Done()
	for {
		data, err := c.Conn.ReadMessage(context.Background())
		select {
		case c.frames <- seqFrame{data, err}:
		case <-c.closed:
			return
		}
		if err != nil {
			return
		}
	}
}

func (c *seqConn) markClosed() { c.closeOnce.Do(func() { close(c.closed) }) }

// cutAsync aborts the socket from a link goroutine without waiting on it.
func (c *seqConn) cutAsync() {
	c.markClosed()
	c.d.dev.w.pumps.Add(1)
	go func() { defer c.d.dev.w.pumps.Done(); _ = c.Close() }()
}

func (c *seqConn) Close() error {
	c.markClosed()
	return c.Conn.Close()
}

func (c *seqConn) Shutdown(ctx context.Context) error {
	err := c.Conn.Shutdown(ctx)
	c.markClosed()
	return err
}

// ReadMessage delivers the next downlink frame to the daemon, applying the
// armed fault.
func (c *seqConn) ReadMessage(ctx context.Context) ([]byte, error) {
	for {
		var f seqFrame
		select {
		case data := <-c.inject:
			f.data = data
		case f = <-c.frames:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-c.closed:
			return nil, net.ErrClosed
		}
		if f.err != nil {
			return nil, f.err
		}
		data, deliver := c.d.onDownlink(c, f.data)
		if !deliver {
			if c.isClosed() {
				return nil, net.ErrClosed
			}
			continue
		}
		return data, nil
	}
}

func (c *seqConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

// WriteMessage sends an uplink frame, applying the armed fault.
func (c *seqConn) WriteMessage(ctx context.Context, data []byte) error {
	switch c.d.onUplink(c, data) {
	case uplinkDrop:
		return nil
	case uplinkCut:
		return net.ErrClosed
	case uplinkThenClose:
		err := c.Conn.WriteMessage(ctx, data)
		closing, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Shutdown(closing)
		return err
	}
	return c.Conn.WriteMessage(ctx, data)
}

type uplinkAction int

const (
	uplinkSend uplinkAction = iota
	uplinkDrop
	uplinkCut
	uplinkThenClose
)

// onDownlink records a frame the daemon is about to read and reports whether
// it is delivered, possibly rewritten. It runs on the daemon link's reader
// goroutine.
func (d *seqDaemon) onDownlink(c *seqConn, data []byte) ([]byte, bool) {
	m := new(circusv1.DaemonDownlink)
	if proto.Unmarshal(data, m) != nil {
		return data, true
	}
	w := d.dev.w
	d.fmu.Lock()
	defer d.fmu.Unlock()
	if f := d.fault; f != nil && f.halfOpen() && f.conn != c {
		d.fault = nil // a new connection: the half-open one was cut
	}
	if f := d.fault; f != nil && f.kind == faultHalfOpenDown {
		return nil, false
	}
	switch p := m.Payload.(type) {
	case *circusv1.DaemonDownlink_DispatchCall:
		id := p.DispatchCall.GetCall().GetCallId()
		if p.DispatchCall.GetTargetBootId() == d.boot {
			if d.dispatches[id] > 0 {
				w.covered("resend")
				// Same-boot resend only inside the 10-minute horizon (D43).
				if first := d.firstDispatch[id]; w.clock.Now().Sub(first) >= 10*time.Minute {
					w.violation(fmt.Sprintf("%s resent to %s %s after its first dispatch, beyond the 10-minute horizon", id, d.label, w.clock.Now().Sub(first)))
				}
			} else {
				d.firstDispatch[id] = w.clock.Now()
			}
			d.dispatches[id]++
		}
		d.lastDispatch = slices.Clone(data)
		if f := d.fault; f != nil && f.kind == faultDropDispatch {
			d.fault = nil
			d.dropped[id] = seqDrop{incarnation: w.incarnation, at: w.clock.Now(), conn: c}
			w.covered("fault:" + string(faultDropDispatch))
			c.cutAsync() // the reader must not block on its own socket
			return nil, false
		}
		if f := d.fault; f != nil && f.kind == faultCorrupt && p.DispatchCall.GetTargetBootId() == d.boot {
			// A misrouting proxy rewrote the target: the daemon of the
			// dispatch boot refuses it with BOOT_MISMATCH on its own
			// connection, so the router applies that rejection.
			d.fault = nil
			d.corrupted[id] = true
			w.covered("fault:" + string(faultCorrupt))
			p.DispatchCall.TargetBootId = "elsewhere-" + d.boot
			if rewritten, err := proto.Marshal(m); err == nil {
				data = rewritten
			}
		}
		if f := d.fault; f != nil && f.kind == faultStaleResend && id == f.target && p.DispatchCall.GetTargetBootId() == d.boot && d.dispatches[id] > 1 {
			// Delay this resend in the network until it exceeds the daemon's
			// dispatch-age limit. Other calls resent in the same sync stay fresh.
			d.fault = nil
			d.staled[id] = true
			p.DispatchCall.DispatchedAtUnixMs = w.clock.Now().Add(-(daemon.DefaultMaxDispatchAge + time.Hour)).UnixMilli()
			if rewritten, err := proto.Marshal(m); err == nil {
				data = rewritten
			} else {
				w.violation(fmt.Sprintf("marshal stale resend of %s: %v", id, err))
			}
		}
	case *circusv1.DaemonDownlink_CallQuery:
		id := p.CallQuery.GetCallId()
		if drop, ok := d.dropped[id]; ok && d.dispatches[id] == 1 && drop.incarnation == w.incarnation && drop.conn != c {
			if w.clock.Now().Sub(drop.at) >= 10*time.Minute {
				w.covered("horizon-query")
			} else {
				// Retained (same process), never acked, inside the horizon:
				// the reconnect sync must resend it before any query.
				w.violation(fmt.Sprintf("%s was queried on %s's new connection instead of resent (lost dispatch %s ago, same control plane)", id, d.label, w.clock.Now().Sub(drop.at)))
			}
		}
	}
	return data, true
}

// onUplink records a frame the daemon writes and decides its fate. It runs
// on the daemon link's writer goroutine.
func (d *seqDaemon) onUplink(c *seqConn, data []byte) uplinkAction {
	m := new(circusv1.DaemonUplink)
	if proto.Unmarshal(data, m) != nil {
		return uplinkSend
	}
	w := d.dev.w
	d.fmu.Lock()
	defer d.fmu.Unlock()
	if ack := m.GetCallAck(); ack != nil {
		if ack.GetStatus().GetState() == circusv1.CallStatus_STATE_RUNNING {
			d.acked[ack.CallId] = true
		}
		if d.staled[ack.CallId] {
			// The answer to a stale copy: an unseen ID is refused EXPIRED, a
			// known one returns its status because deduplication runs first.
			delete(d.staled, ack.CallId)
			switch {
			case ack.GetRejection().GetCode() == circusv1.Rejection_CODE_EXPIRED && !d.recorded[ack.CallId]:
				w.covered("stale-resend")
			case ack.GetStatus() != nil && d.recorded[ack.CallId]:
				w.covered("stale-known-status")
			default:
				w.violation(fmt.Sprintf("stale copy of %s answered %v (known to the daemon: %v)", ack.CallId, ack, d.recorded[ack.CallId]))
			}
		}
		if ack.GetStatus() != nil {
			d.recorded[ack.CallId] = true
		}
		switch ack.GetRejection().GetCode() {
		case circusv1.Rejection_CODE_BOOT_MISMATCH:
			if d.corrupted[ack.CallId] {
				w.covered("boot-mismatch-applied") // reaches the router's rejection handling
			} else {
				w.covered("misdelivery-refused") // an old boot's call: the router's fence drops it
			}
		case circusv1.Rejection_CODE_SHUTTING_DOWN:
			if d.dispatches[ack.CallId] > 1 {
				w.covered("shutting-down-after-resend")
			}
		}
	}
	if f := d.fault; f != nil && f.halfOpen() && f.conn != c {
		d.fault = nil
	}
	f := d.fault
	if f == nil {
		return uplinkSend
	}
	switch {
	case f.kind == faultHalfOpenUp:
		return uplinkDrop
	case f.kind == faultDropAck && m.GetCallAck() != nil,
		f.kind == faultDropResult && m.GetResult() != nil:
		d.fault = nil
		w.covered("fault:" + string(f.kind))
		c.cutAsync()
		return uplinkCut
	case f.kind == faultResultClose && m.GetResult() != nil:
		d.fault = nil
		w.covered("fault:" + string(f.kind))
		return uplinkThenClose
	}
	return uplinkSend
}

func (d *seqDaemon) arm(kind seqFaultKind) {
	d.armFor(kind, "")
}

// armFor arms a fault that applies only to the given call.
func (d *seqDaemon) armFor(kind seqFaultKind, target string) {
	conn := d.current()
	d.fmu.Lock()
	d.fault = &seqFault{kind: kind, since: d.dev.w.clock.Now(), conn: conn, target: target}
	d.fmu.Unlock()
}

// knows reports whether the daemon answered the call with a status, so its
// dispatcher holds a record for it.
func (d *seqDaemon) knows(id string) bool {
	d.fmu.Lock()
	defer d.fmu.Unlock()
	return d.recorded[id]
}

func (d *seqDaemon) armed() *seqFault {
	d.fmu.Lock()
	defer d.fmu.Unlock()
	return d.fault
}

func (d *seqDaemon) armedOn() *seqFault {
	if d == nil {
		return nil
	}
	return d.armed()
}

func (d *seqDaemon) disarm() {
	d.fmu.Lock()
	d.fault = nil
	d.fmu.Unlock()
}

// halfOpen reports a half-open fault on the device's daemon: a Query sent
// there would wait for the heartbeat timeout, so operations avoid it.
func (dev *seqDevice) halfOpen() bool {
	if dev.daemon == nil {
		return false
	}
	f := dev.daemon.armed()
	return f != nil && f.halfOpen() && f.conn != nil && f.conn == dev.daemon.current() && !f.conn.isClosed()
}

// detectHalfOpen cuts half-open links whose heartbeat timeout has passed.
func (w *seqWorld) detectHalfOpen() {
	for _, dev := range w.devices {
		if !dev.halfOpen() {
			if f := dev.daemon.armedOn(); f != nil && f.halfOpen() {
				dev.daemon.disarm() // its connection already ended
			}
			continue
		}
		f := dev.daemon.armed()
		if w.clock.Now().Sub(f.since) >= heartbeatTimeout {
			w.record("  heartbeat timeout cuts %s's half-open link", dev.name)
			dev.daemon.disarm()
			dev.daemon.cut()
		}
	}
}

// violation records a broken invariant seen on a link goroutine; checkStep
// reports it.
func (w *seqWorld) violation(msg string) {
	w.covMu.Lock()
	w.violations = append(w.violations, msg)
	w.covMu.Unlock()
}

func (w *seqWorld) takeViolations() []string {
	w.covMu.Lock()
	defer w.covMu.Unlock()
	out := w.violations
	w.violations = nil
	return out
}

func (w *seqWorld) covered(path string) {
	w.covMu.Lock()
	w.cov[path]++
	w.covMu.Unlock()
}

// readyDevice returns a random device whose route is ready and that has no
// armed fault.
func (w *seqWorld) readyDevice() *seqDevice {
	var out []*seqDevice
	for _, dev := range w.devices {
		if w.m.ready(dev) && dev.daemon.armed() == nil {
			out = append(out, dev)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out[w.pick(len(out))]
}

// opFaultDispatch loses a new call's DispatchCall (or the daemon's ack of
// it) and then reconnects at once (resend), after the resend horizon
// (query only), or leaves the device to later operations.
func (w *seqWorld) opFaultDispatch() bool {
	dev := w.readyDevice()
	if dev == nil {
		return false
	}
	kind := faultDropDispatch
	if w.pick(3) == 0 {
		kind = faultDropAck
	}
	dev.daemon.arm(kind)
	id := fmt.Sprintf("c%03d", w.step)
	w.record("arm %s on %s; submit %s", kind, dev.name, id)
	w.m.submit(w.m.newCall(id, dev), dev, fmt.Sprintf("cmd-%d", w.step), "new")
	w.settle("lost dispatch")
	if dev.daemon.armed() != nil {
		dev.daemon.disarm() // no DispatchCall went out (rejected or offline)
		w.record("  no DispatchCall was sent; fault disarmed")
		return true
	}
	switch w.pick(4) {
	case 0, 1:
		w.record("  reconnect %s at once", dev.name)
		dev.daemon.kick(true)
	case 2:
		w.record("  advance 11m0s before %s reconnects", dev.name)
		w.advance(11 * time.Minute)
	case 3:
		// A copy of the call is resent after it exceeded MaxDispatchAge. If
		// the dispatch was dropped, the daemon never saw the ID and refuses it
		// EXPIRED (Lost). If only the ack was dropped, the daemon deduplicates
		// before checking age and returns the call's status.
		known := dev.daemon.knows(id)
		if w.m.faultPending() || (kind == faultDropAck && !known) {
			break
		}
		w.record("  reconnect %s with a stale resend of %s (known to the daemon: %v)", dev.name, id, known)
		dev.daemon.armFor(faultStaleResend, id)
		dev.daemon.cut()
		w.wait("backoff for stale resend on "+dev.name, func() bool {
			s := dev.daemon.client.Stats()
			return !s.Connected && s.RetryDelay > 0 && dev.daemon.clock.pending()
		})
		dev.daemon.kick(true)
		w.settle("stale resend")
		if dev.daemon.armed() != nil {
			w.fail("stale resend of %s was not delivered", id)
			return true
		}
		row := w.m.row(id)
		switch {
		case !known && (row == nil || row.State != store.CallLost || row.OutcomeReason != "EXPIRED"):
			w.fail("stale resend of unseen %s ended %v, want lost EXPIRED", id, row)
		case known && (row == nil || row.State == store.CallLost || row.State == store.CallRejected):
			w.fail("stale resend of known %s ended %v, want its daemon status", id, row)
		}
	}
	return true
}

// opFaultResult loses a finished tool's result in flight (or closes the
// link gracefully right after writing it), optionally reconnecting at once.
func (w *seqWorld) opFaultResult() bool {
	var candidates []*seqExec
	for _, e := range w.m.blockedExecs() {
		if dev := w.m.boots[e.boot].dev; w.m.ready(dev) && dev.daemon.armed() == nil {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return false
	}
	e := candidates[w.pick(len(candidates))]
	d := w.m.boots[e.boot]
	kind := faultDropResult
	if w.pick(3) == 0 {
		kind = faultResultClose
	}
	d.arm(kind)
	w.record("arm %s on %s; tool completes %s", kind, d.label, e.callID)
	w.m.release(e)
	w.settle("lost result")
	if d.armed() != nil {
		d.disarm()
		w.record("  no result was written; fault disarmed")
		return true
	}
	if w.pick(2) == 0 {
		w.record("  reconnect %s at once", d.dev.name)
		d.kick(true)
	}
	return true
}

// opHalfOpen makes one direction of a ready device's link vanish until a
// later advance passes the heartbeat timeout.
func (w *seqWorld) opHalfOpen() bool {
	dev := w.readyDevice()
	if dev == nil {
		return false
	}
	kind := faultHalfOpenDown
	if w.pick(2) == 0 {
		kind = faultHalfOpenUp
	}
	w.record("link of %s goes %s", dev.name, kind)
	dev.daemon.arm(kind)
	w.effect("half-open")
	w.covered("fault:" + string(kind))
	if blocked := w.m.blockedOn(dev.daemon.boot); kind == faultHalfOpenUp && len(blocked) > 0 && !w.m.faultPending() && w.pick(2) == 0 {
		// The result vanishes on the half-open link, which stays registered
		// (so no retention cutoff applies); the daemon's 30-minute result
		// retention passes before the reconnect asks for it.
		e := blocked[w.pick(len(blocked))]
		w.record("  tool completes %s; its result vanishes; advance 31m0s", e.callID)
		w.m.release(e)
		w.settle("vanished result")
		w.advance(31 * time.Minute)
		w.covered("result-expired-while-half-open")
	}
	return true
}

// opMisdeliver injects a DispatchCall captured from a dead boot of the
// device into its current link, as a misrouting proxy would: the daemon
// refuses it with BOOT_MISMATCH, and the router must ignore that reply,
// which names a call of another boot.
func (w *seqWorld) opMisdeliver() bool {
	var out []*seqDevice
	frames := make(map[*seqDevice][]byte)
	for _, dev := range w.devices {
		if dev.ghost || !w.m.ready(dev) || dev.daemon.armed() != nil {
			continue
		}
		for _, old := range w.m.daemonsOf(dev) {
			old.fmu.Lock()
			frame := old.lastDispatch
			old.fmu.Unlock()
			if old != dev.daemon && frame != nil {
				frames[dev] = frame
			}
		}
		if frames[dev] != nil {
			out = append(out, dev)
		}
	}
	if len(out) == 0 {
		return false
	}
	dev := out[w.pick(len(out))]
	conn := dev.daemon.current()
	if conn == nil {
		return false
	}
	w.record("misdeliver an old-boot DispatchCall to %s (%s)", dev.name, dev.daemon.label)
	conn.inject <- frames[dev]
	return true
}

// opCorruptTarget delivers a new call's DispatchCall to the right daemon but
// naming another boot. The daemon refuses it with BOOT_MISMATCH on the
// dispatch boot's own connection; the router must settle the call lost
// BOOT_MISMATCH (never executed, never retried on another boot).
func (w *seqWorld) opCorruptTarget() bool {
	dev := w.readyDevice()
	if dev == nil || w.m.faultPending() {
		return false
	}
	id := fmt.Sprintf("c%03d", w.step)
	w.record("arm %s on %s; submit %s", faultCorrupt, dev.name, id)
	dev.daemon.arm(faultCorrupt)
	w.m.submit(w.m.newCall(id, dev), dev, fmt.Sprintf("cmd-%d", w.step), "new")
	w.settle("corrupted dispatch")
	if dev.daemon.armed() != nil {
		dev.daemon.disarm()
		w.record("  no DispatchCall was sent; fault disarmed")
		return true
	}
	if row := w.m.row(id); row == nil || row.State != store.CallLost || row.OutcomeReason != "BOOT_MISMATCH" {
		w.fail("call %s refused with BOOT_MISMATCH by its dispatch boot ended %v, want lost BOOT_MISMATCH", id, row)
	}
	return true
}

// opDeferredQuery holds a reconnecting device's sync before it starts, so
// its route is registered but not ready, and queries a call of that boot:
// the query must wait for the sync (deferred) and then be answered.
func (w *seqWorld) opDeferredQuery() bool {
	dev := w.readyDevice()
	if dev == nil || w.m.faultPending() {
		return false
	}
	c := w.m.pickCall(func(c *seqCall) bool {
		row := w.m.row(c.id)
		return c.dev == dev && row != nil && row.DispatchedBootID == dev.daemon.boot && (row.State == store.CallDispatched || row.State == store.CallRunning || row.State == store.CallUnknown || row.State == store.CallCompleted)
	})
	if c == nil {
		return false
	}
	w.record("hold %s's next sync; reconnect; query %s while its route is not ready", dev.name, c.id)
	gate := w.hooks.holdSync(dev.id)
	dev.daemon.cut()
	w.m.deviceDown(dev)
	w.wait("cut of "+dev.name, func() bool { s := dev.daemon.client.Stats(); return !s.Connected && s.RetryDelay > 0 })
	dev.daemon.kick(true)
	w.wait("unready route of "+dev.name, func() bool { return gate.held() })
	// The gateway holds the sync, but the daemon may still be reading
	// Welcome: disturbing it now would abort its handshake.
	w.wait("daemon handshake of "+dev.name, func() bool { return dev.daemon.client.Stats().Connected })
	answer := make(chan func(), 1)
	var returned atomic.Bool
	go func() { finish := w.m.queryAsync(c, false); returned.Store(true); answer <- finish }()
	w.wait("deferred query", func() bool { return returned.Load() || w.router.Stats().QueryWaiters > 0 })
	if returned.Load() {
		w.fail("query of %s returned while the route of its boot was synchronizing", c.id)
	}
	w.covered("deferred-query")
	gate.release()
	select {
	case finish := <-answer:
		finish()
	case <-time.After(w.timeout(30 * time.Second)):
		w.fail("deferred query of %s was not answered after the sync", c.id)
	}
	return true
}

// opDeferredQueryBootChange defers a query of a completed call on its
// boot's unready route, then the daemon restarts: the new boot's sync must
// answer the old-boot waiter (the daemon that held the result is gone)
// instead of asking the new boot about a call it never saw.
func (w *seqWorld) opDeferredQueryBootChange() bool {
	dev := w.readyDevice()
	if dev == nil || w.m.faultPending() {
		return false
	}
	c := w.m.pickCall(func(c *seqCall) bool {
		row := w.m.row(c.id)
		return c.dev == dev && row != nil && row.DispatchedBootID == dev.daemon.boot && row.State == store.CallCompleted
	})
	if c == nil {
		return false
	}
	old := dev.daemon
	w.record("hold %s's next sync; query completed %s while its route is not ready; the daemon crashes", dev.name, c.id)
	gate := w.hooks.holdSync(dev.id)
	defer gate.release()
	old.cut()
	w.m.deviceDown(dev)
	w.wait("cut of "+dev.name, func() bool { s := old.client.Stats(); return !s.Connected && s.RetryDelay > 0 })
	old.kick(true)
	w.wait("unready route of "+dev.name, func() bool { return gate.held() })
	// The gateway holds the sync, but the daemon may still be reading
	// Welcome: disturbing it now would abort its handshake.
	w.wait("daemon handshake of "+dev.name, func() bool { return dev.daemon.client.Stats().Connected })
	answer := make(chan func(), 1)
	var returned atomic.Bool
	go func() { finish := w.m.queryAsync(c, false); returned.Store(true); answer <- finish }()
	w.wait("deferred query", func() bool { return returned.Load() || w.router.Stats().QueryWaiters > 0 })
	if returned.Load() {
		w.fail("query of %s returned while the route of its boot was synchronizing", c.id)
	}
	// The new boot registers while the old connection still lives (its
	// socket outlived the process), then the old process is gone.
	w.startDaemon(dev)
	w.record("  new boot %s", dev.daemon.label)
	w.wait("route of "+dev.daemon.label, func() bool {
		r, ok, _ := w.registry.Lookup(w.ctx, dev.id)
		return ok && r.Target.BootID == dev.daemon.boot
	})
	w.stopDaemon(old, false)
	w.m.daemonStopped(old)
	select {
	case finish := <-answer:
		finish()
		w.covered("deferred-query-old-boot")
	case <-time.After(w.timeout(30 * time.Second)):
		w.fail("deferred query of %s was not answered after the boot change", c.id)
	}
	return true
}

// opResendDuringShutdown covers the shutdown-refusal rule: a dispatch is lost,
// the device reconnects with its sync held, the daemon begins a graceful
// shutdown that a stubborn tool keeps open, and only then the sync resends
// the call. The daemon refuses the unseen ID with SHUTTING_DOWN. Deduplication
// precedes that refusal and the age bound keeps earlier records within the
// tombstone lifetime, so the router must reject the call as never executed.
func (w *seqWorld) opResendDuringShutdown() bool {
	dev := w.readyDevice()
	if dev == nil || w.m.faultPending() {
		return false
	}
	old := dev.daemon
	stubborn := w.m.blockedOn(old.boot)
	if len(stubborn) == 0 {
		return false
	}
	e := stubborn[w.pick(len(stubborn))]
	id := fmt.Sprintf("c%03d", w.step)
	w.record("resend during shutdown on %s: drop the DispatchCall of %s, hold the sync, stop the daemon (%s ignores cancellation)", dev.name, id, e.callID)
	old.arm(faultDropDispatch)
	w.m.submit(w.m.newCall(id, dev), dev, fmt.Sprintf("cmd-%d", w.step), "new")
	w.settle("lost dispatch")
	if w.failed {
		return true
	}
	if old.armed() != nil {
		old.disarm() // no DispatchCall went out (rejected)
		w.record("  no DispatchCall was sent; fault disarmed")
		return true
	}
	gate := w.hooks.holdSync(dev.id)
	old.kick(true)
	w.wait("held sync of "+dev.name, func() bool { return gate.held() })
	// Cancelling Run while the client still waits for Welcome would abort
	// the handshake, so the resend would never reach a live daemon.
	w.wait("daemon handshake of "+dev.name, func() bool { return old.client.Stats().Connected })
	old.graceful = true
	w.m.setStubborn(e)
	old.cancel()
	// Shutdown closes the dispatcher and cancels the running tools in one
	// critical section: once the stubborn tool's ctx is done, an unseen
	// call ID is refused.
	w.wait("dispatcher closed", func() bool { return e.ctx.Err() != nil })
	gate.release()
	refused := func() int { w.covMu.Lock(); defer w.covMu.Unlock(); return w.cov["shutting-down-after-resend"] }
	w.wait("refusal of the resend", func() bool { return refused() > w.shutdownSeen })
	if w.failed {
		old.fmu.Lock()
		w.t.Logf("diagnostics: %s dispatches=%d acked=%v dropped=%v; execs=%d; route=%v; gate held=%v; dials %d/%d; up %v; client=%+v", id, old.dispatches[id], old.acked[id], old.dropped[id].conn != nil, len(w.m.execsOf(w.m.byID[id])), w.m.ready(dev), gate.held(), old.dials.Load(), old.expected.Load(), dev.up.Load(), old.client.Stats())
		old.fmu.Unlock()
	}
	w.shutdownSeen = refused()
	w.settleHooks()
	if row := w.m.row(id); row == nil || row.State != store.CallRejected || row.OutcomeReason != "DEVICE_SHUTTING_DOWN" {
		w.fail("resend refused during shutdown ended %v, want rejected DEVICE_SHUTTING_DOWN", row)
	}
	w.m.release(e)
	w.stopDaemon(old, true)
	w.m.daemonStopped(old)
	w.settle("daemon stop")
	if w.failed {
		return true
	}
	w.startDaemon(dev)
	w.record("  new boot %s", dev.daemon.label)
	return true
}

// seqStore lets the sequence fail the next accepted → dispatched transition,
// wherever it happens (Submit or a reconnect sync's dispatch).
type seqStore struct {
	store.Store
	failDispatch atomic.Bool
}

func (s *seqStore) Tx(ctx context.Context, fn func(context.Context, store.Ops) error) error {
	return s.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error { return fn(ctx, &seqStoreOps{Ops: tx, s: s}) })
}

type seqStoreOps struct {
	store.Ops
	s *seqStore
}

func (o *seqStoreOps) TransitionCall(ctx context.Context, id string, expect store.CallExpect, to store.CallState, patch store.CallPatch) (bool, error) {
	if to == store.CallDispatched && o.s.failDispatch.CompareAndSwap(true, false) {
		return false, store.ErrBusy
	}
	return o.Ops.TransitionCall(ctx, id, expect, to, patch)
}

// quietBoot reports whether no call of the device's current boot is active:
// a sync that fails would otherwise race the replies to its earlier
// resends and queries with the link's close (nondeterministic outcomes).
func (w *seqWorld) quietBoot(dev *seqDevice) bool {
	return w.m.pickCall(func(c *seqCall) bool {
		row := w.m.row(c.id)
		return c.dev == dev && row != nil && row.DispatchedBootID == dev.daemon.boot && (row.State == store.CallDispatched || row.State == store.CallRunning || row.State == store.CallUnknown)
	}) == nil
}

// seqClaims is the timer runner's store. Its claims bypass the Faulty
// store's write points (but fail once it crashed): how often the runner
// polls depends on scheduling, and counting its claims would move an armed
// crash point from run to run. Handlers still write through Faulty.
type seqClaims struct{ *storetest.Faulty }

func (s seqClaims) ClaimDueTimers(ctx context.Context, now int64, instance string, lease int64, limit int) ([]store.Timer, error) {
	if s.Crashed() {
		return nil, storetest.ErrCrashed
	}
	return s.Inner().ClaimDueTimers(ctx, now, instance, lease, limit)
}

// opFailSyncDispatch fails the dispatch commit of the reconnect sync of a
// device that has accepted calls waiting: no DispatchCall may go out
// (commit before send, checked by the evidence bus), the sync fails and a
// later reconnect dispatches them.
func (w *seqWorld) opFailSyncDispatch() bool {
	if w.m.faultPending() {
		return false
	}
	var out []*seqDevice
	for _, dev := range w.devices {
		if dev.ghost || dev.daemon.stopped || w.m.ready(dev) || dev.daemon.armed() != nil || !w.quietBoot(dev) {
			continue
		}
		if w.m.pickCall(func(c *seqCall) bool {
			row := w.m.row(c.id)
			return c.dev == dev && row != nil && row.State == store.CallAccepted
		}) != nil {
			out = append(out, dev)
		}
	}
	if len(out) == 0 {
		return false
	}
	dev := out[w.pick(len(out))]
	w.record("reconnect %s with its sync's dispatch commit failing", dev.name)
	w.effect("store")
	w.m.storeFault()
	w.store.failDispatch.Store(true)
	dev.up.Store(true)
	dev.daemon.kick(true)
	w.settle("failed sync dispatch")
	if w.store.failDispatch.Swap(false) {
		w.record("  no dispatch was attempted")
	} else {
		w.covered("sync-dispatch-commit-failed")
	}
	return true
}

// opBusyLink refuses one DispatchCall at the bus (a full send pool): either
// a first send, which settles Rejected{OVERLOADED} without execution, or a
// resend after a lost dispatch, which must keep the call dispatched (the
// first DispatchCall may have executed).
func (w *seqWorld) opBusyLink() bool {
	dev := w.readyDevice()
	if dev == nil || w.m.faultPending() || !w.quietBoot(dev) {
		return false
	}
	w.effect("busy")
	id := fmt.Sprintf("c%03d", w.step)
	if w.pick(2) == 0 {
		w.record("busy send pool: first DispatchCall of %s to %s is refused", id, dev.name)
		w.evidence.busyNext.Store(true)
		w.m.submit(w.m.newCall(id, dev), dev, fmt.Sprintf("cmd-%d", w.step), "new")
		w.evidence.busyNext.Store(false)
		return true
	}
	w.record("busy send pool: lose the DispatchCall of %s on %s, then refuse its resend", id, dev.name)
	dev.daemon.arm(faultDropDispatch)
	w.m.submit(w.m.newCall(id, dev), dev, fmt.Sprintf("cmd-%d", w.step), "new")
	w.settle("lost dispatch")
	if dev.daemon.armed() != nil {
		dev.daemon.disarm()
		w.record("  no DispatchCall was sent; fault disarmed")
		return true
	}
	w.evidence.busyNext.Store(true)
	dev.daemon.kick(true)
	w.settle("refused resend")
	w.evidence.busyNext.Store(false)
	return true
}

// busyRefused records which kind of DispatchCall the bus refused.
func (w *seqWorld) busyRefused(id, boot string) {
	if d := w.m.boots[boot]; d != nil {
		d.fmu.Lock()
		_, dropped := d.dropped[id]
		seen := d.dispatches[id] > 0 || dropped
		d.fmu.Unlock()
		if seen {
			w.covered("busy-resend")
			return
		}
	}
	w.covered("busy-first-send")
}

// settleHooks waits until no hook runs; used inside operations that cannot
// settle fully yet.
func (w *seqWorld) settleHooks() {
	w.wait("idle hooks", func() bool { return w.hooks.active.Load() == 0 && w.router.Stats().Operations == 0 })
}

// beginStep starts a step's fault record with the faults still pending
// from earlier steps: an armed one-shot failure or crash point, failing
// writes, an armed dispatch-commit failure, a half-open link.
func (w *seqWorld) beginStep() {
	pending := w.store != nil && w.m.faultPending()
	halfOpen := false
	for _, dev := range w.devices {
		halfOpen = halfOpen || dev.halfOpen()
	}
	w.covMu.Lock()
	w.inEffect = map[string]bool{"store": pending, "half-open": halfOpen}
	w.covMu.Unlock()
}

// effect records a fault injected during the current step.
func (w *seqWorld) effect(kind string) {
	w.covMu.Lock()
	w.inEffect[kind] = true
	w.covMu.Unlock()
}

func (w *seqWorld) effective(kind string) bool {
	w.covMu.Lock()
	defer w.covMu.Unlock()
	return w.inEffect[kind]
}

// errorExplained reports whether a hook or timer error is the expected
// effect of an injected fault, a crash, a closing link or router, or a
// sync that a held or half-open link starved.
func (w *seqWorld) errorExplained(err error) bool {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, wire.ErrClosed), errors.Is(err, wire.ErrClosing):
		return true // the link or router closed during the operation
	case errors.Is(err, store.ErrBusy):
		return w.effective("store") // the injected write failure
	case errors.Is(err, wire.ErrBusy):
		return w.effective("busy") // the refused resend fails the sync
	}
	text := err.Error()
	switch {
	case strings.Contains(text, "storetest: store crashed"), strings.Contains(text, "calls: closed"):
		return true
	case strings.Contains(text, "did not answer synchronization requests"):
		return w.effective("half-open")
	}
	return false
}
