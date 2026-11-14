package calls_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/storetest"
)

// The reference model of TestRandomSequences. It does not predict the exact
// state of every call: interleavings inside one operation are not under the
// sequence's control. Instead it records facts (what the tools executed and
// returned, which boots died, when devices were offline, which faults and
// crashes happened) and derives, per call and attempt, the set of outcomes
// those facts permit:
//
//   - a call_id executes at most once, across boots and control-plane
//     incarnations, and only on the boot its durable row names;
//   - completed requires an execution, and its result is that execution's
//     output (or the dispatcher's abandonment fault for a cancelled tool);
//   - offline, rejected and cancelled require that the attempt never
//     executed; cancelled also requires a cancel, DEVICE_SHUTTING_DOWN a
//     graceful stop of the dispatch boot, offline an offline device;
//   - Rejected{STORE_UNAVAILABLE} requires an injected fault, a row that is
//     absent or lost and never dispatched, and no execution until the next
//     accepted submission of that ID;
//   - lost requires the fact its reason names: a control-plane crash or
//     store fault for an undispatched attempt, a dead dispatch boot, a daemon
//     that never got the call or whose result expired, or a device offline
//     for the 30-minute result retention;
//   - unknown requires that the device was seen offline, and is never final;
//   - terminal metadata never changes, except a lost, never-dispatched call
//     restarted by an equal resubmission;
//   - every submission sees at most one terminal update, which agrees with
//     the final durable state, or ends without one only because the control
//     plane crashed or its subscriber stalled.
type seqModel struct {
	w     *seqWorld
	mu    sync.Mutex // guards execs and calls against tool goroutines
	calls []*seqCall
	byID  map[string]*seqCall
	boots map[string]*seqDaemon
	execs []*seqExec
	subs  []*seqSub
	async []string // violations seen by tool goroutines
	// before holds the nonterminal calls per device before an advance.
	before  map[*seqDevice][]*seqCall
	closing bool
	// seen counts each call's distinct durable states and each
	// submission's first update, for the summary.
	seen map[string]int
}

type seqExec struct {
	call                  *seqCall
	callID, boot, command string
	attempt               int
	ctx                   context.Context
	release               chan struct{}
	released, finished    bool
	stubborn              bool // ignores cancellation until released
	output                string
	doneAt                time.Time
}

type seqCall struct {
	id      string
	dev     *seqDevice
	content string // command of the request the durable row holds
	// The current attempt: a new ID, or an equal resubmission of a lost,
	// never-dispatched call. Facts below are reset per attempt.
	attempt            int
	nextAttempt        int // attempt numbers are never reused
	attemptIncarnation int
	attemptEpoch       int
	downAtAttempt      bool
	cancelRequested    bool
	storeFaulted       bool // a store fault was armed during the attempt
	ambiguous          bool // a submission returned an error, not an outcome
	retention          bool // its device was offline >= 30 minutes while active
	execs              []*seqExec
	// blocked forbids executions beyond blockedExecs (STORE_UNAVAILABLE)
	// until the next accepted attempt.
	blocked      bool
	blockedExecs int
	// f19 is set when a subscriber received the attempt's result although
	// its completion could not be committed (DESIGN.md F19).
	f19 bool
	// Last observed durable state, for monotonicity.
	seen                 bool
	lastState            store.CallState
	lastReason, lastBoot string
	lastAttempt          int
}

type seqSub struct {
	call        *seqCall
	kind        string // new, duplicate, mismatch, query
	attempt     int
	incarnation int
	s           *calls.Subscription
	updates     []string
	terminal    *circusv1.CallUpdate
	end         error
	stalled     bool
	stalledEver bool
	// notExecuted is set when an observed update claimed that the attempt
	// never executed (lost because the daemon did not know the call);
	// checkFinal re-checks such claims against the final executions.
	notExecuted bool
}

const seqResultRetention = 30 * time.Minute

func newSeqModel(w *seqWorld) *seqModel {
	return &seqModel{w: w, byID: make(map[string]*seqCall), boots: make(map[string]*seqDaemon), seen: make(map[string]int)}
}

func (m *seqModel) newCall(id string, dev *seqDevice) *seqCall {
	c := &seqCall{id: id, dev: dev}
	m.mu.Lock()
	m.calls = append(m.calls, c)
	m.byID[id] = c
	m.mu.Unlock()
	return c
}

func (m *seqModel) pickCall(keep func(*seqCall) bool) *seqCall {
	var out []*seqCall
	for _, c := range m.calls {
		if keep(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out[m.w.pick(len(out))]
}

// pickActive prefers calls whose durable state is not terminal.
func (m *seqModel) pickActive() *seqCall {
	if m.w.pick(5) > 0 {
		if c := m.pickCall(func(c *seqCall) bool { row := m.row(c.id); return row != nil && !row.State.Terminal() }); c != nil {
			return c
		}
	}
	return m.pickCall(func(*seqCall) bool { return true })
}

func (m *seqModel) pickSub() *seqSub {
	var out []*seqSub
	for _, s := range m.subs {
		if s.s != nil && s.end == nil {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	s := out[m.w.pick(len(out))]
	if !s.stalled {
		s.stalledEver = true
	}
	return s
}

// label names a boot by device and start order (desk#2): boot IDs are
// random, labels make traces comparable across runs.
func (m *seqModel) label(boot string) string {
	if d := m.boots[boot]; d != nil {
		return d.label
	}
	return boot
}

// unstarted returns a call the daemon acked as running whose tool has not
// started yet (its goroutine was admitted but not scheduled), or "".
func (m *seqModel) unstarted(d *seqDaemon) string {
	d.fmu.Lock()
	ids := sortedKeys(d.acked)
	d.fmu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		started := false
		if c := m.byID[id]; c != nil {
			for _, e := range c.execs {
				started = started || e.boot == d.boot
			}
		}
		if !started {
			return id
		}
	}
	return ""
}

// corrupted reports whether the harness rewrote the target boot of the
// call's DispatchCall on its way to the daemon of boot.
func (m *seqModel) corrupted(id, boot string) bool {
	d := m.boots[boot]
	if d == nil {
		return false
	}
	d.fmu.Lock()
	defer d.fmu.Unlock()
	return d.corrupted[id]
}

// dispatchSeen reports whether any DispatchCall for id reached (or was lost
// on the way to) the daemon of boot.
func (m *seqModel) dispatchSeen(id, boot string) bool {
	d := m.boots[boot]
	if d == nil {
		return false
	}
	d.fmu.Lock()
	defer d.fmu.Unlock()
	_, dropped := d.dropped[id]
	return d.dispatches[id] > 0 || dropped
}

func (m *seqModel) bootDead(boot string) bool {
	d := m.boots[boot]
	return d != nil && d.stopped
}

func (m *seqModel) daemonsOf(dev *seqDevice) []*seqDaemon {
	var out []*seqDaemon
	for _, boot := range sortedKeys(m.boots) {
		if d := m.boots[boot]; d.dev == dev {
			out = append(out, d)
		}
	}
	return out
}

// startExec runs on a tool goroutine.
func (m *seqModel) startExec(id, boot, command string, ctx context.Context) *seqExec {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := &seqExec{callID: id, boot: boot, command: command, ctx: ctx, release: make(chan struct{})}
	if c := m.byID[id]; c != nil {
		e.call, e.attempt = c, c.attempt
		c.execs = append(c.execs, e)
		if len(c.execs) > 1 {
			m.async = append(m.async, fmt.Sprintf("call %s executed %d times (boots %s)", id, len(c.execs), execBoots(c.execs)))
		}
		if command != c.content {
			m.async = append(m.async, fmt.Sprintf("call %s executed command %q, but its request is %q", id, command, c.content))
		}
	} else {
		m.async = append(m.async, fmt.Sprintf("execution of unknown call %s", id))
	}
	m.execs = append(m.execs, e)
	return e
}

func execBoots(execs []*seqExec) string {
	var boots []string
	for _, e := range execs {
		boots = append(boots, e.boot)
	}
	return strings.Join(boots, ",")
}

func (m *seqModel) finishExec(e *seqExec, output string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.finished, e.output, e.doneAt = true, output, m.w.clock.Now()
}

func (m *seqModel) execCounts() (execs, finished int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.execs {
		if e.finished {
			finished++
		}
	}
	return len(m.execs), finished
}

func (m *seqModel) finishedOn(boot string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.execs {
		if e.finished && e.boot == boot {
			n++
		}
	}
	return n
}

func (m *seqModel) toolsUnsettled() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.execs {
		if !e.finished && (e.released || e.ctx.Err() != nil) {
			return "tool of " + e.callID + " finishing"
		}
	}
	return ""
}

func (m *seqModel) blockedExecs() []*seqExec {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*seqExec
	for _, e := range m.execs {
		if !e.finished && !e.released && e.ctx.Err() == nil && !m.bootDead(e.boot) {
			out = append(out, e)
		}
	}
	// Tool goroutines start in scheduling order; pick deterministically by call ID.
	slices.SortFunc(out, func(a, b *seqExec) int { return strings.Compare(a.callID, b.callID) })
	return out
}

func (m *seqModel) pickBlocked() *seqExec {
	blocked := m.blockedExecs()
	if len(blocked) == 0 {
		return nil
	}
	return blocked[m.w.pick(len(blocked))]
}

func (m *seqModel) blockedOn(boot string) []*seqExec {
	var out []*seqExec
	for _, e := range m.blockedExecs() {
		if e.boot == boot {
			out = append(out, e)
		}
	}
	return out
}

func (m *seqModel) setStubborn(e *seqExec) {
	m.mu.Lock()
	e.stubborn = true
	m.mu.Unlock()
}

func (m *seqModel) isStubborn(e *seqExec) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return e.stubborn
}

func (m *seqModel) release(e *seqExec) {
	m.mu.Lock()
	e.released = true
	m.mu.Unlock()
	close(e.release)
}

func (m *seqModel) execsOf(c *seqCall) []*seqExec {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*seqExec(nil), c.execs...)
}

// releaseAll lets every blocked tool finish.
func (m *seqModel) releaseAll() bool {
	blocked := m.blockedExecs()
	for _, e := range blocked {
		m.mu.Lock()
		e.released = true
		m.mu.Unlock()
		close(e.release)
	}
	return len(blocked) > 0
}

func (m *seqModel) exec(c *seqCall, attempt int) *seqExec {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range c.execs {
		if attempt == 0 || e.attempt == attempt {
			return e
		}
	}
	return nil
}

func (m *seqModel) row(id string) *store.Call {
	c, err := m.w.db.GetCall(m.w.ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		m.w.fail("read call %s: %v", id, err)
		return nil
	}
	return &c
}

func (m *seqModel) rows() []store.Call {
	var out []store.Call
	filter := store.CallFilter{Limit: store.MaxPageSize}
	for {
		page, err := m.w.db.ListCalls(m.w.ctx, filter)
		if err != nil {
			m.w.fail("list calls: %v", err)
			return nil
		}
		out = append(out, page...)
		if len(page) < store.MaxPageSize {
			return out
		}
		last := page[len(page)-1]
		filter.After = &store.CallCursor{SubmittedAt: last.SubmittedAt, CallID: last.CallID}
	}
}

func (m *seqModel) allTerminal() bool {
	for _, row := range m.rows() {
		if !row.State.Terminal() {
			return false
		}
	}
	return true
}

// faultPending reports an injected store failure or crash that may still
// hit a write.
func (m *seqModel) faultPending() bool {
	w := m.w
	return w.faultAll || w.faultTarget > w.fault.Writes() || w.crashArmed || w.fault.Crashed() || w.store.failDispatch.Load()
}

func (m *seqModel) ready(dev *seqDevice) bool {
	if dev.ghost || dev.daemon == nil || dev.daemon.stopped {
		return false
	}
	route, ok, err := m.w.registry.Lookup(m.w.ctx, dev.id)
	return err == nil && ok && route.Ready && route.Target.BootID == dev.daemon.boot
}

func (m *seqModel) deviceDown(dev *seqDevice) {
	if dev.downSince.IsZero() {
		dev.downSince = m.w.clock.Now()
		dev.downEpoch++
	}
}

// observe records device readiness after the system settled.
func (m *seqModel) observe() {
	for _, dev := range m.w.devices {
		if m.ready(dev) {
			dev.downSince = time.Time{}
			dev.everReady = true
		} else {
			m.deviceDown(dev)
		}
	}
}

func (m *seqModel) downSeen(c *seqCall) bool {
	return c.downAtAttempt || c.dev.downEpoch > c.attemptEpoch || !c.dev.downSince.IsZero()
}

func (m *seqModel) daemonStopped(d *seqDaemon) { m.deviceDown(d.dev) }

func (m *seqModel) controlPlaneCrashed() {
	for _, dev := range m.w.devices {
		m.deviceDown(dev)
	}
}

func (m *seqModel) storeFault() {
	for _, c := range m.calls {
		if row := m.row(c.id); row == nil || !row.State.Terminal() {
			c.storeFaulted = true
		}
	}
}

func (m *seqModel) beforeAdvance() {
	m.before = make(map[*seqDevice][]*seqCall)
	for _, c := range m.calls {
		if row := m.row(c.id); row != nil && !row.State.Terminal() {
			m.before[c.dev] = append(m.before[c.dev], c)
		}
	}
}

// afterAdvance grants the result-retention loss to active calls of devices
// offline for 30 minutes. The router counts offline time from the recorded
// disconnect (or its own start after a crash), never before the sequence
// first saw the device offline, so this errs on the permissive side.
func (m *seqModel) afterAdvance() {
	now := m.w.clock.Now()
	for dev, list := range m.before {
		if !dev.downSince.IsZero() && now.Sub(dev.downSince) >= seqResultRetention {
			for _, c := range list {
				c.retention = true
			}
		}
	}
}

// attemptFacts copies the per-attempt facts of c.
type attemptFacts struct {
	attempt, incarnation, epoch                                       int
	downAt, cancelRequested, storeFaulted, ambiguous, retention, used bool
}

func (m *seqModel) facts(c *seqCall) attemptFacts {
	return attemptFacts{c.attempt, c.attemptIncarnation, c.attemptEpoch, c.downAtAttempt, c.cancelRequested, c.storeFaulted, c.ambiguous, c.retention, true}
}

// restoreAttempt returns to the previous attempt's facts when a resubmission
// provably persisted nothing (its row is still the previous attempt's).
func (m *seqModel) restoreAttempt(c *seqCall, f attemptFacts) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// The failed submission keeps its own number; nextAttempt never reuses it.
	c.attempt, c.attemptIncarnation, c.attemptEpoch = f.attempt, f.incarnation, f.epoch
	c.downAtAttempt, c.cancelRequested, c.storeFaulted, c.ambiguous, c.retention = f.downAt, f.cancelRequested, f.storeFaulted, f.ambiguous, f.retention
}

func (m *seqModel) beginAttempt(c *seqCall, dev *seqDevice) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c.nextAttempt++
	c.attempt = c.nextAttempt
	c.attemptIncarnation = m.w.incarnation
	c.attemptEpoch = dev.downEpoch
	c.downAtAttempt = !dev.downSince.IsZero()
	c.cancelRequested = false
	c.storeFaulted = m.faultPending()
	c.ambiguous = false
	c.retention = false
	c.f19 = false
}

// submit performs one Submit and checks its immediate outcome.
func (m *seqModel) submit(c *seqCall, dev *seqDevice, command, kind string) {
	w := m.w
	row := m.row(c.id)
	fresh := kind != "mismatch" && (row == nil || (row.State == store.CallLost && row.DispatchedBootID == ""))
	var previous attemptFacts
	if fresh {
		if row == nil {
			c.content = command
		} else {
			previous = m.facts(c)
		}
		m.beginAttempt(c, dev)
	}
	pending := m.faultPending()
	sub := &seqSub{call: c, kind: kind, attempt: c.attempt, incarnation: w.incarnation}
	m.subs = append(m.subs, sub)
	ctx := w.ctx
	if row != nil && row.State == store.CallCompleted && kind == "duplicate" {
		// The duplicate queries the daemon like Query does, so it is
		// bounded the same way; its subscription is already terminal.
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(w.ctx, w.timeout(20*time.Second))
		defer func() {
			if sub.s != nil && sub.end == nil {
				m.drainSub(sub)
			}
			cancel()
		}()
	}
	s, err := w.router.Submit(ctx, "u", dev.name, seqRequest(c.id, command))
	if ctx.Err() != nil {
		w.fail("duplicate submit of completed %s did not return", c.id)
	}
	if err != nil {
		sub.end = err
		w.record("  -> error: %v", err)
		if fresh {
			// The row may still be the previous attempt's: permit both.
			c.ambiguous = true
			if previous.used {
				c.attemptIncarnation = min(c.attemptIncarnation, previous.incarnation)
				c.storeFaulted = c.storeFaulted || previous.storeFaulted
			}
		}
		completed := row != nil && row.State == store.CallCompleted && kind == "duplicate"
		switch {
		case errors.Is(err, calls.ErrStoreUnavailable) && pending:
		case errors.Is(err, storetest.ErrCrashed) && pending:
		case errors.Is(err, calls.ErrResultUnavailable) && completed:
			m.checkUnavailable(c, row)
		case errors.Is(err, calls.ErrDeviceOffline) && completed:
		default:
			w.fail("submit %s (%s): unexpected error %v", c.id, kind, err)
		}
		return
	}
	sub.s = s
	m.drainSub(sub)
	w.record("  -> %s", strings.Join(submitted(sub.updates), ", "))
	first := ""
	if len(sub.updates) > 0 {
		first = sub.updates[0]
		if strings.HasPrefix(first, "rejected") || strings.HasPrefix(first, "result") {
			m.seen["submit:"+strings.SplitN(first, ":", 3)[0]+":"+strings.SplitN(first+":", ":", 3)[1]]++
		}
	}
	switch {
	case first == "rejected:CODE_STORE_UNAVAILABLE", first == "rejected:CODE_OVERLOADED" && kind != "mismatch":
		if first == "rejected:CODE_STORE_UNAVAILABLE" && !pending {
			w.fail("submit %s: STORE_UNAVAILABLE without an injected fault", c.id)
		}
		after := m.row(c.id)
		if after != nil && !(after.State == store.CallLost && after.DispatchedBootID == "") {
			if first == "rejected:CODE_STORE_UNAVAILABLE" {
				w.fail("submit %s: STORE_UNAVAILABLE but a new attempt persisted: %+v", c.id, *after)
			}
			return // OVERLOADED of an attempt that was dispatched: its row says so
		}
		if previous.used {
			m.restoreAttempt(c, previous)
		}
		m.mu.Lock()
		c.blocked, c.blockedExecs = true, len(c.execs)
		m.mu.Unlock()
	case first == "accepted" && fresh:
		m.mu.Lock()
		c.blocked = false
		m.mu.Unlock()
	}
}

func (m *seqModel) cancel(c *seqCall) {
	w := m.w
	row := m.row(c.id)
	c.cancelRequested = true
	pending := m.faultPending()
	err := w.router.Cancel(w.ctx, "u", c.id)
	if err != nil {
		w.record("  -> error: %v", err)
	}
	switch {
	case err == nil:
	case errors.Is(err, store.ErrNotFound) && row == nil:
	case (errors.Is(err, store.ErrBusy) || errors.Is(err, storetest.ErrCrashed)) && pending:
	default:
		w.fail("cancel %s: unexpected error %v", c.id, err)
	}
}

// query runs Query and checks the answer; final also requires agreement
// with the terminal durable state.
func (m *seqModel) query(c *seqCall, final bool) { m.queryAsync(c, final)() }

// queryAsync runs Query, possibly on another goroutine, and returns the
// checks of its answer, which run on the sequence's goroutine.
func (m *seqModel) queryAsync(c *seqCall, final bool) func() {
	w := m.w
	ctx, cancel := context.WithTimeout(w.ctx, w.timeout(20*time.Second))
	defer cancel()
	u, err := w.router.Query(ctx, "u", c.id)
	timedOut := ctx.Err() != nil
	return func() { m.checkQuery(c, final, u, err, timedOut) }
}

func (m *seqModel) checkQuery(c *seqCall, final bool, u *circusv1.CallUpdate, err error, timedOut bool) {
	w := m.w
	if timedOut {
		w.fail("query %s did not return", c.id)
		return
	}
	row := m.row(c.id)
	sub := &seqSub{call: c, kind: "query", attempt: c.attempt}
	if err != nil {
		w.record("  -> query %s error: %v", c.id, err)
	} else {
		w.record("  -> query %s: %s", c.id, describe(u))
	}
	completed := row != nil && row.State == store.CallCompleted
	switch {
	case err == nil:
		m.checkUpdate(sub, u, "query")
		if final && row != nil && !agrees(u, *row) {
			w.fail("final query of %s answered %s, durable state %s/%s", c.id, describe(u), row.State, row.OutcomeReason)
		}
	case errors.Is(err, store.ErrNotFound) && row == nil:
	case errors.Is(err, calls.ErrResultUnavailable) && completed:
		m.checkUnavailable(c, row)
	case errors.Is(err, calls.ErrDeviceOffline) && completed:
		if !final && !m.bootDead(row.DispatchedBootID) && m.ready(c.dev) && !m.faultPending() {
			w.fail("query %s: device offline although its dispatch boot is ready", c.id)
		}
		if final && !m.bootDead(row.DispatchedBootID) {
			w.fail("final query %s: device offline although every live daemon is connected", c.id)
		}
	case errors.Is(err, calls.ErrStoreUnavailable) && completed && m.faultPending():
	case errors.Is(err, storetest.ErrCrashed) && w.fault.Crashed():
	default:
		w.fail("query %s: unexpected error %v (durable %v)", c.id, err, row)
	}
}

// checkUnavailable permits ErrResultUnavailable for a completed call only
// once the daemon's 30-minute result retention has passed.
func (m *seqModel) checkUnavailable(c *seqCall, row *store.Call) {
	e := m.exec(c, 0)
	if e == nil || !e.finished || m.w.clock.Now().Sub(e.doneAt) < seqResultRetention {
		m.w.fail("call %s: result unavailable before its retention expired (exec %v)", c.id, e != nil)
	}
	_ = row
}

func describe(u *circusv1.CallUpdate) string {
	switch v := u.Update.(type) {
	case *circusv1.CallUpdate_Accepted_:
		return "accepted"
	case *circusv1.CallUpdate_Dispatched_:
		return "dispatched"
	case *circusv1.CallUpdate_Unknown_:
		return "unknown:" + v.Unknown.Reason
	case *circusv1.CallUpdate_Result:
		text := ""
		if blocks := v.Result.GetOutput().GetContent(); len(blocks) > 0 {
			text = blocks[0].GetText()
		}
		return "result:" + text
	case *circusv1.CallUpdate_Offline:
		return "offline"
	case *circusv1.CallUpdate_Cancelled_:
		return "cancelled"
	case *circusv1.CallUpdate_Rejected_:
		return "rejected:" + v.Rejected.Code.String()
	case *circusv1.CallUpdate_Lost_:
		return "lost:" + v.Lost.Reason
	}
	return fmt.Sprintf("%T", u.Update)
}

func isTerminal(u *circusv1.CallUpdate) bool {
	return u.GetResult() != nil || u.GetOffline() != nil || u.GetLost() != nil || u.GetRejected() != nil || u.GetCancelled() != nil
}

// agrees reports whether a terminal update matches the durable state.
func agrees(u *circusv1.CallUpdate, row store.Call) bool {
	switch {
	case u.GetResult() != nil:
		return row.State == store.CallCompleted
	case u.GetLost() != nil:
		return row.State == store.CallLost && u.GetLost().Reason == row.OutcomeReason
	case u.GetOffline() != nil:
		return row.State == store.CallOffline
	case u.GetCancelled() != nil:
		return row.State == store.CallCancelled
	case u.GetRejected() != nil:
		return row.State == store.CallRejected && u.GetRejected().Code.String() == "CODE_"+row.OutcomeReason
	case u.GetUnknown() != nil:
		return row.State == store.CallUnknown
	case u.GetDispatched() != nil:
		return row.State == store.CallDispatched || row.State == store.CallRunning
	case u.GetAccepted() != nil:
		return row.State == store.CallAccepted
	}
	return false
}

func (m *seqModel) drainSubs() {
	for _, s := range m.subs {
		if s.s != nil && s.end == nil && !s.stalled {
			m.drainSub(s)
		}
	}
}

func (m *seqModel) unstallAll() {
	for _, s := range m.subs {
		s.stalled = false
	}
}

var seqDone = func() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}()

// drainSub reads every queued update without blocking.
func (m *seqModel) drainSub(s *seqSub) {
	w := m.w
	for {
		u, err := s.s.Next(seqDone)
		if errors.Is(err, context.Canceled) {
			return
		}
		if err != nil {
			s.end = err
			switch {
			case errors.Is(err, io.EOF) && s.terminal != nil:
			case errors.Is(err, calls.ErrOverflow) && s.stalledEver:
			case errors.Is(err, calls.ErrClosed) && (s.incarnation < w.incarnation || m.closing || w.fault.Crashed()):
			default:
				w.fail("subscription of %s (%s) ended with %v after %v", s.call.id, s.kind, err, s.updates)
			}
			return
		}
		if s.terminal != nil {
			w.fail("subscription of %s (%s) received %s after terminal %s", s.call.id, s.kind, describe(u), describe(s.terminal))
		}
		if u.CallId != s.call.id {
			w.fail("subscription of %s received an update for %s", s.call.id, u.CallId)
		}
		s.updates = append(s.updates, describe(u))
		if isTerminal(u) {
			s.terminal = u
		}
		m.checkUpdate(s, u, "subscription")
	}
}

// executed reports whether the attempt has an execution.
func (m *seqModel) executed(c *seqCall, attempt int) bool { return m.exec(c, attempt) != nil }

// checkUpdate checks one update a submission or query observed.
func (m *seqModel) checkUpdate(s *seqSub, u *circusv1.CallUpdate, where string) {
	w, c := m.w, s.call
	current := s.attempt == c.attempt
	switch v := u.Update.(type) {
	case *circusv1.CallUpdate_Result:
		if v.Result.CallId != c.id {
			w.fail("%s of %s: result names %s", where, c.id, v.Result.CallId)
		}
		if msg := m.checkResult(c, v.Result); msg != "" {
			w.fail("%s of %s: %s", where, c.id, msg)
		}
		if row := m.row(c.id); current && (row == nil || row.State != store.CallCompleted) {
			// Published after the completion Tx returned: the commit failed.
			if !c.storeFaulted {
				w.fail("%s of %s: result published while the call is %v and no store fault was injected", where, c.id, row)
			}
			c.f19 = true
		}
	case *circusv1.CallUpdate_Rejected_:
		switch code := v.Rejected.Code; code {
		case circusv1.CallUpdate_Rejected_CODE_CALL_MISMATCH:
			if s.kind != "mismatch" {
				w.fail("%s of %s (%s): CALL_MISMATCH for an equal request", where, c.id, s.kind)
			}
			return
		case circusv1.CallUpdate_Rejected_CODE_STORE_UNAVAILABLE, circusv1.CallUpdate_Rejected_CODE_OVERLOADED:
			// Checked by submit against the injected faults and the row;
			// a dispatched call may be settled OVERLOADED only by a refused
			// first send.
			if row := m.row(c.id); current && code == circusv1.CallUpdate_Rejected_CODE_OVERLOADED && row != nil && row.State == store.CallRejected && m.dispatchSeen(c.id, row.DispatchedBootID) {
				w.fail("%s of %s: rejected OVERLOADED although a DispatchCall had reached the daemon (a refused resend must keep it dispatched)", where, c.id)
			}
		case circusv1.CallUpdate_Rejected_CODE_DEVICE_SHUTTING_DOWN:
			if row := m.row(c.id); current && (row == nil || !m.boots[row.DispatchedBootID].graceful) {
				w.fail("%s of %s: DEVICE_SHUTTING_DOWN without a graceful stop of its dispatch boot", where, c.id)
			}
		default:
			w.fail("%s of %s: unexpected rejection %s", where, c.id, code)
		}
		if m.executed(c, s.attempt) {
			w.fail("%s of %s: rejected %s, but the attempt executed", where, c.id, v.Rejected.Code)
		}
	case *circusv1.CallUpdate_Offline:
		if m.executed(c, s.attempt) {
			w.fail("%s of %s: offline, but the attempt executed", where, c.id)
		}
		if current && !c.dev.ghost && !m.downSeen(c) && c.dev.everReady {
			w.fail("%s of %s: offline although %s was never seen offline", where, c.id, c.dev.name)
		}
	case *circusv1.CallUpdate_Cancelled_:
		if m.executed(c, s.attempt) || (current && !c.cancelRequested) {
			w.fail("%s of %s: cancelled (requested %v), executed %v", where, c.id, c.cancelRequested, m.executed(c, s.attempt))
		}
	case *circusv1.CallUpdate_Lost_:
		if !current {
			return
		}
		row := m.row(c.id)
		if row == nil {
			w.fail("%s of %s: lost without a row", where, c.id)
			return
		}
		if msg := m.checkLost(c, v.Lost.Reason, row.DispatchedBootID); msg != "" {
			w.fail("%s of %s: %s", where, c.id, msg)
		}
		if (v.Lost.Reason == "RESULT_UNAVAILABLE" || v.Lost.Reason == "EXPIRED") && !m.executed(c, s.attempt) {
			s.notExecuted = true // the daemon did not know the call
		}
	case *circusv1.CallUpdate_Unknown_:
		if current && !m.downSeen(c) && c.attemptIncarnation == w.incarnation {
			w.fail("%s of %s: unknown although %s was never seen offline", where, c.id, c.dev.name)
		}
	}
}

// checkResult compares a result with the execution's real output.
func (m *seqModel) checkResult(c *seqCall, r *circusv1.DeviceResult) string {
	e := m.exec(c, 0)
	if e == nil {
		return "result without any execution"
	}
	m.mu.Lock()
	finished, output := e.finished, e.output
	m.mu.Unlock()
	text := ""
	if blocks := r.GetOutput().GetContent(); len(blocks) > 0 {
		text = blocks[0].GetText()
	}
	if finished && text == output && r.Fault == nil {
		return ""
	}
	if f := r.GetFault(); f != nil && e.ctx.Err() != nil && (f.Code == circusv1.DaemonFault_CODE_ABANDONED_AFTER_TIMEOUT || f.Code == circusv1.DaemonFault_CODE_ABANDONED_AFTER_CANCEL) {
		return ""
	}
	return fmt.Sprintf("result %q (fault %v) differs from the tool's output %q (finished %v)", text, r.GetFault(), output, finished)
}

// checkLost checks the reason of a loss against the facts it requires.
func (m *seqModel) checkLost(c *seqCall, reason, boot string) string {
	e := m.exec(c, 0)
	expired := false
	if e != nil {
		m.mu.Lock()
		expired = e.finished && m.w.clock.Now().Sub(e.doneAt) >= seqResultRetention
		m.mu.Unlock()
	}
	ok := false
	switch reason {
	case "CONTROL_PLANE_RESTARTED":
		ok = boot == "" && e == nil && c.attemptIncarnation < m.w.incarnation
	case "REQUEST_UNAVAILABLE":
		ok = boot == "" && e == nil && (c.storeFaulted || c.ambiguous || c.attemptIncarnation < m.w.incarnation)
	case "DAEMON_RESTARTED":
		ok = boot != "" && m.bootDead(boot)
	case "BOOT_MISMATCH":
		ok = boot != "" && e == nil && (m.bootDead(boot) || m.corrupted(c.id, boot))
	case "RESULT_UNAVAILABLE", "EXPIRED":
		ok = boot != "" && (e == nil || expired)
	case "RESULT_RETENTION_EXPIRED":
		ok = boot != "" && c.retention
	}
	if ok {
		return ""
	}
	return fmt.Sprintf("lost %s (dispatch boot %q, executed %v, expired %v, retention %v, incarnation %d/%d) is not permitted", reason, m.label(boot), e != nil, expired, c.retention, c.attemptIncarnation, m.w.incarnation)
}

// checkStep checks the durable state after one settled operation.
func (m *seqModel) checkStep() {
	w := m.w
	m.mu.Lock()
	async := m.async
	m.async = nil
	m.mu.Unlock()
	for _, msg := range async {
		w.fail("%s", msg)
	}
	for _, msg := range w.takeViolations() {
		w.fail("%s", msg)
	}
	for _, msg := range w.hooks.takeErrors() {
		w.fail("hook or timer error that no injected fault explains: %s", msg)
	}
	allRows := m.rows()
	// Once a new boot's route is ready, its sync has lost every call of the
	// device's older boots: none may still be active (Activate follows the
	// loss batches).
	for _, dev := range w.devices {
		if !m.ready(dev) {
			continue
		}
		for _, row := range allRows {
			if row.DeviceID == dev.id && (row.State == store.CallDispatched || row.State == store.CallRunning || row.State == store.CallUnknown) && row.DispatchedBootID != dev.daemon.boot {
				w.fail("%s is ready on %s, but call %s is still %s on %s", dev.name, dev.daemon.label, row.CallID, row.State, m.label(row.DispatchedBootID))
			}
		}
	}
	rows := make(map[string]store.Call)
	for _, row := range allRows {
		rows[row.CallID] = row
		if m.byID[row.CallID] == nil {
			w.fail("row for unknown call %s", row.CallID)
		}
	}
	for _, c := range m.calls {
		m.mu.Lock()
		execs, blocked, blockedExecs := len(c.execs), c.blocked, c.blockedExecs
		m.mu.Unlock()
		if execs > 1 {
			w.fail("call %s executed %d times", c.id, execs)
		}
		if blocked && execs > blockedExecs {
			w.fail("call %s executed after Rejected{STORE_UNAVAILABLE} without a new accepted submission", c.id)
		}
		row, ok := rows[c.id]
		if !ok {
			if execs > 0 {
				w.fail("call %s executed but has no durable row", c.id)
			}
			continue
		}
		m.checkRow(c, row)
	}
}

func (m *seqModel) checkRow(c *seqCall, row store.Call) {
	w := m.w
	e := m.exec(c, 0)
	if e != nil && row.DispatchedBootID != e.boot {
		w.fail("call %s executed on %s, but its row names dispatch boot %q", c.id, m.label(e.boot), m.label(row.DispatchedBootID))
	}
	switch row.State {
	case store.CallCompleted:
		if e == nil {
			w.fail("call %s completed without an execution", c.id)
			break
		}
		m.mu.Lock()
		running := !e.finished && e.ctx.Err() == nil
		m.mu.Unlock()
		if running {
			w.fail("call %s completed while its tool still runs", c.id)
		}
	case store.CallCancelled:
		if e != nil || !c.cancelRequested {
			w.fail("call %s cancelled (requested %v) but executed %v", c.id, c.cancelRequested, e != nil)
		}
	case store.CallOffline:
		if e != nil || (!c.dev.ghost && !m.downSeen(c) && c.dev.everReady) {
			w.fail("call %s offline (executed %v, device seen offline %v)", c.id, e != nil, m.downSeen(c))
		}
	case store.CallRejected:
		if e != nil {
			w.fail("call %s rejected %s but executed", c.id, row.OutcomeReason)
		}
		switch row.OutcomeReason {
		case "OVERLOADED":
			if m.dispatchSeen(c.id, row.DispatchedBootID) {
				w.fail("call %s rejected OVERLOADED although a DispatchCall had reached the daemon (a refused resend must keep it dispatched)", c.id)
			}
		case "DEVICE_SHUTTING_DOWN":
			if d := m.boots[row.DispatchedBootID]; d == nil || !d.graceful {
				w.fail("call %s rejected DEVICE_SHUTTING_DOWN without a graceful stop of %s", c.id, m.label(row.DispatchedBootID))
			}
		default:
			w.fail("call %s rejected with unexpected reason %s", c.id, row.OutcomeReason)
		}
	case store.CallLost:
		if msg := m.checkLost(c, row.OutcomeReason, row.DispatchedBootID); msg != "" {
			w.fail("call %s: %s", c.id, msg)
		}
	case store.CallUnknown:
		if !m.downSeen(c) && c.attemptIncarnation == w.incarnation {
			w.fail("call %s unknown although %s was never seen offline", c.id, c.dev.name)
		}
	}
	if c.seen && c.lastState.Terminal() && (row.State != c.lastState || row.OutcomeReason != c.lastReason) {
		restartable := c.lastState == store.CallLost && c.lastBoot == "" && c.attempt > c.lastAttempt
		if !restartable {
			w.fail("call %s changed terminal state %s/%s to %s/%s", c.id, c.lastState, c.lastReason, row.State, row.OutcomeReason)
		}
	}
	if !c.seen || row.State != c.lastState {
		m.seen["seen:"+string(row.State)]++
	}
	c.seen, c.lastState, c.lastReason, c.lastBoot, c.lastAttempt = true, row.State, row.OutcomeReason, row.DispatchedBootID, c.attempt
}

func (m *seqModel) finalQueries() {
	for _, c := range m.calls {
		if m.w.failed {
			return // each further query could wait for its timeout
		}
		if m.row(c.id) != nil {
			m.query(c, true)
		}
	}
}

// checkFinal runs after quiescence: every call is terminal, and every
// submission's stream ended in one outcome that agrees with it, or ended
// without one for a permitted reason.
func (m *seqModel) checkFinal() {
	w := m.w
	m.checkStep()
	for _, row := range m.rows() {
		if !row.State.Terminal() {
			w.fail("call %s is still %s after quiescence", row.CallID, row.State)
		}
	}
	for _, s := range m.subs {
		c := s.call
		if s.s == nil {
			continue // Submit returned an error: no stream
		}
		if s.terminal == nil {
			if s.end == nil && !s.stalled {
				w.fail("submission of %s (%s, step updates %v) has no outcome after quiescence", c.id, s.kind, s.updates)
			}
			continue
		}
		if s.kind == "mismatch" {
			continue
		}
		// Re-check every claim of non-execution against the final record.
		if claimsNotExecuted(s) && m.executed(c, s.attempt) {
			w.fail("submission of %s (%s) ended %s, but that attempt executed later", c.id, s.kind, describe(s.terminal))
		}
		if s.attempt != c.attempt {
			continue
		}
		if code := s.terminal.GetRejected().GetCode(); code == circusv1.CallUpdate_Rejected_CODE_STORE_UNAVAILABLE || code == circusv1.CallUpdate_Rejected_CODE_OVERLOADED {
			continue
		}
		row := m.row(c.id)
		if row == nil || agrees(s.terminal, *row) {
			continue
		}
		// F19 only: the result reached the subscriber while its completion
		// could not be committed, and the call was lost afterwards.
		if s.terminal.GetResult() != nil && c.f19 && row.State == store.CallLost {
			continue
		}
		w.fail("submission of %s (%s) ended %s, durable state %s/%s", c.id, s.kind, describe(s.terminal), row.State, row.OutcomeReason)
	}
}

// claimsNotExecuted reports whether a submission's terminal update says its
// attempt never executed.
func claimsNotExecuted(s *seqSub) bool {
	u := s.terminal
	switch {
	case u.GetOffline() != nil, u.GetCancelled() != nil:
		return true
	case u.GetRejected() != nil:
		return u.GetRejected().Code != circusv1.CallUpdate_Rejected_CODE_CALL_MISMATCH
	case u.GetLost() != nil:
		reason := u.GetLost().Reason
		return reason == "CONTROL_PLANE_RESTARTED" || reason == "REQUEST_UNAVAILABLE" || s.notExecuted
	}
	return false
}

// summary counts final outcomes and facts, to show what a seed exercised.
func (m *seqModel) summary() string {
	counts := make(map[string]int)
	for _, row := range m.rows() {
		key := string(row.State)
		if row.OutcomeReason != "" && row.State != store.CallCompleted {
			key += "/" + row.OutcomeReason
		}
		counts[key]++
	}
	execs, _ := m.execCounts()
	counts["executions"] = execs
	counts["boots"] = len(m.boots)
	counts["incarnations"] = m.w.incarnation
	for k, v := range m.seen {
		counts[k] = v
	}
	var parts []string
	for _, k := range sortedKeys(counts) {
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, " ")
}

// submitted returns the updates Submit itself queued before returning: its
// first update, and the dispatched of its own dispatch commit after an
// accepted. Updates from hooks (the dispatched of the first RUNNING ack, a
// daemon's rejection) may also be queued by the time the harness drains,
// depending on scheduling; the model checks them, the trace leaves them out
// so a seed replays exactly.
func submitted(updates []string) []string {
	if len(updates) > 1 && updates[0] == "accepted" && updates[1] == "dispatched" {
		return updates[:2]
	}
	return updates[:min(len(updates), 1)]
}
