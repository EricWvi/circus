package sessions

import (
	"context"
	"errors"
	"math"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/store"
	"github.com/google/uuid"
)

// ReadFunc builds the reply of an operation that only reads. The link runs
// it in the operation's own goroutine under Service.Read. sent, when not
// nil, must be called once the reply has been handed to the link (not when
// the operation was unwatched or the send failed). An error means the
// outcome cannot be told to the worker (an acquired lease whose reply
// failed); the link must close, and the worker retries after reconnecting.
// Each read checks its fence when it starts, not when it was received.
type ReadFunc func(ctx context.Context) (reply *circusv1.SessionReply, sent func(), err error)

// plain adapts a read without a delivery hook.
func plain(fn func(ctx context.Context) *circusv1.SessionReply) ReadFunc {
	return func(ctx context.Context) (*circusv1.SessionReply, func(), error) { return fn(ctx), nil, nil }
}

const (
	maxUsageBytes   = 4 << 10
	maxLoadBytes    = 8 << 20
	maxJobStopCalls = 4
)

// Handle serves one SessionRequest from the connection's receive loop.
// Writes (and the lease write of AcquireLease) run here, inline, in receive
// order, each in one store transaction bounded by StoreTimeout, and return
// the reply. Reads return a ReadFunc instead, which the link runs off the
// receive loop under Service.Read; AcquireLease returns one too, for its
// Lease reply.
//
// rid is the operation's request id; ctx ends with the operation.
func (c *Conn) Handle(ctx context.Context, rid uint64, req *circusv1.SessionRequest) (*circusv1.SessionReply, ReadFunc) {
	if c.bootID == "" {
		return invalidReply("session operations need a connection with a boot id"), nil
	}
	id := req.GetSessionId()
	if !identifier.Valid(id, false) {
		return invalidReply("invalid session id"), nil
	}
	_, acquire := req.Op.(*circusv1.SessionRequest_AcquireLease)
	if acquire != (req.GetEpoch() == 0) || req.GetEpoch() > math.MaxInt64 {
		return invalidReply("epoch must be 0 for AcquireLease and nonzero otherwise"), nil
	}
	epoch := int64(req.GetEpoch())
	f := c.fence(id, epoch)
	switch op := req.Op.(type) {
	case *circusv1.SessionRequest_AcquireLease:
		return c.acquire(ctx, rid, id, op.AcquireLease.GetFresh())
	case *circusv1.SessionRequest_ReleaseLease:
		return c.release(ctx, f), nil
	case *circusv1.SessionRequest_LoadTranscript:
		return nil, plain(func(ctx context.Context) *circusv1.SessionReply {
			return c.load(ctx, c.fence(id, epoch), op.LoadTranscript)
		})
	case *circusv1.SessionRequest_ListTranscriptKeys:
		return nil, plain(func(ctx context.Context) *circusv1.SessionReply { return c.keys(ctx, c.fence(id, epoch)) })
	case *circusv1.SessionRequest_AppendTranscript:
		return c.append(ctx, f, op.AppendTranscript), nil
	case *circusv1.SessionRequest_TakeInputs:
		return c.take(ctx, f), nil
	case *circusv1.SessionRequest_ReportTurn:
		return c.report(ctx, f, op.ReportTurn), nil
	case *circusv1.SessionRequest_PatchState:
		return c.patch(ctx, f, op.PatchState), nil
	case *circusv1.SessionRequest_GetState:
		return nil, plain(func(ctx context.Context) *circusv1.SessionReply {
			return c.getState(ctx, c.fence(id, epoch), op.GetState)
		})
	case *circusv1.SessionRequest_PublishEvents:
		return c.publish(ctx, f, op.PublishEvents), nil
	case *circusv1.SessionRequest_RequestApproval:
		return c.requestApproval(ctx, f, op.RequestApproval), nil
	case *circusv1.SessionRequest_ConsumeGrant:
		return c.consumeGrant(ctx, f, op.ConsumeGrant), nil
	case *circusv1.SessionRequest_TranscriptRepaired:
		return c.repaired(ctx, f), nil
	}
	return invalidReply("missing operation"), nil
}

func (c *Conn) fence(id string, epoch int64) store.Fence {
	return store.Fence{SessionID: id, Epoch: epoch, WorkerID: c.workerID, BootID: c.bootID, ConnID: c.id, Now: c.s.nowMs()}
}

// tx runs fn in one store transaction bounded by StoreTimeout.
func (c *Conn) tx(ctx context.Context, fn func(ctx context.Context, tx store.Ops) error) error {
	tctx, cancel := storeCtx(ctx)
	defer cancel()
	return c.s.cfg.Store.Tx(tctx, fn)
}

// failed maps err and, for a fenced operation on the lease this process
// believes bound here, re-derives the session from the store.
func (c *Conn) failed(ctx context.Context, f store.Fence, op string, err error) *circusv1.SessionReply {
	if errors.Is(err, store.ErrFenced) {
		s := c.s
		s.mu.Lock()
		if e := s.entries[f.SessionID]; e != nil && e.holder.epoch == f.Epoch {
			s.queueWake(f.SessionID, 0)
		}
		s.mu.Unlock()
	}
	return c.s.failure(ctx, op, err)
}

// acquire is AcquireLease: the write runs inline; the Lease reply is read
// by the returned ReadFunc.
func (c *Conn) acquire(ctx context.Context, rid uint64, id string, fresh bool) (*circusv1.SessionReply, ReadFunc) {
	s := c.s
	// Reserve the session's entry first, so an acquired lease is always
	// tracked (and extended); a full table refuses the acquisition.
	s.mu.Lock()
	e := s.entries[id]
	if e == nil {
		if len(s.entries) >= s.maxTracked {
			s.mu.Unlock()
			s.log.Warn("session table full; acquisition refused", "session_id", id)
			return rejected(circusv1.SessionReply_Rejected_CODE_STORE_UNAVAILABLE, "session table full; retry"), nil
		}
		e = &entry{}
		s.entries[id] = e
	}
	e.reserved++
	offered := e.offered == c
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if e := s.entries[id]; e != nil {
			e.reserved--
			s.forget(id, e)
		}
		s.mu.Unlock()
	}()
	now := s.nowMs()
	var res store.LeaseAcquired
	notOffered := false
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if !offered {
			l, err := tx.GetLease(ctx, id)
			if err != nil {
				return err
			}
			if !l.Live(now) || l.WorkerID != c.workerID || l.BootID != c.bootID {
				notOffered = true
				return nil
			}
		}
		var err error
		res, err = tx.AcquireLease(ctx, store.LeaseRequest{SessionID: id, WorkerID: c.workerID, BootID: c.bootID, ConnID: c.id,
			Now: now, ExpiresAt: now + LeaseLifetime.Milliseconds(), Fresh: fresh})
		return err
	})
	if err == nil && res.Loss != nil {
		s.lost(*res.Loss, c, false)
	}
	if err != nil || notOffered || res.Released {
		// Re-derive the session: an Offer to a session that is no longer
		// queued is withdrawn, one that still is stays.
		s.mu.Lock()
		if e := s.entries[id]; e != nil {
			s.forget(id, e)
		}
		s.queueWake(id, 0)
		s.mu.Unlock()
	}
	switch {
	case notOffered:
		return rejected(circusv1.SessionReply_Rejected_CODE_NOT_OFFERED, "no current offer to this connection"), nil
	case errors.Is(err, store.ErrStopped):
		return rejected(circusv1.SessionReply_Rejected_CODE_STOPPED, "session stopped"), nil
	case errors.Is(err, store.ErrBroken), errors.Is(err, store.ErrLeaseHeld):
		return rejected(circusv1.SessionReply_Rejected_CODE_NOT_OFFERED, "session not available"), nil
	case err != nil:
		return s.failure(ctx, "acquire", err), nil
	case res.Released && errors.Is(res.Refusal, store.ErrStopped):
		return rejected(circusv1.SessionReply_Rejected_CODE_STOPPED, "session stopped; lease released"), nil
	case res.Released:
		return rejected(circusv1.SessionReply_Rejected_CODE_NOT_OFFERED, "session not available; lease released"), nil
	}
	s.mu.Lock()
	e = s.track(id, res.Session.UserID) // reserved above, so present
	s.bind(id, e, res.Lease, c, true)
	e.interrupt = res.Session.InterruptTurnNo
	// Each acquisition has its own token, so the end of an earlier
	// operation never touches a later one's state.
	s.tokens++
	tok := s.tokens
	e.known, e.pending, e.abandoned, e.token = true, true, false, tok
	c.remember(rid, id, tok)
	s.mu.Unlock()
	epoch, reattached := res.Lease.Epoch, res.Reattached
	// When the operation ends (reply sent, Unwatch, link closed), a reply
	// that was not delivered abandons the lease: it is no longer extended.
	// An Unwatch that comes after the slot was released is caught by
	// Unwatched.
	context.AfterFunc(ctx, func() { s.abandon(id, tok, false) })
	return nil, func(ctx context.Context) (*circusv1.SessionReply, func(), error) {
		g := s.generation()
		r, err := c.leaseReply(ctx, id, epoch, reattached)
		if err != nil || r.GetLease() == nil {
			return r, nil, err
		}
		return r, func() { s.delivered(id, tok, g) }, nil
	}
}

// maxRecentAcquires bounds the AcquireLease request ids a connection
// remembers for a late Unwatch.
const maxRecentAcquires = 16

// remember records that operation rid acquired session id under token tok;
// s.mu held. The oldest of maxRecentAcquires entries is overwritten.
func (c *Conn) remember(rid uint64, id string, tok uint64) {
	c.acquires[c.nextAcquire] = recentAcquire{rid: rid, id: id, tok: tok}
	c.nextAcquire = (c.nextAcquire + 1) % maxRecentAcquires
}

// Unwatched tells the service that the worker unwatched operation rid. If
// it was one of this connection's last maxRecentAcquires AcquireLeases,
// its lease is abandoned even if its reply was already handed to the link
// (the worker drops replies of unwatched operations, so it never learned
// the lease). This is best effort: a worker that unwatched an AcquireLease
// must assume it may hold the lease (wire.md).
func (c *Conn) Unwatched(rid uint64) {
	s := c.s
	s.mu.Lock()
	var id string
	var tok uint64
	for _, a := range c.acquires {
		if a.rid == rid && a.tok != 0 {
			id, tok = a.id, a.tok
		}
	}
	s.mu.Unlock()
	if tok != 0 {
		s.abandon(id, tok, true)
	}
}

// delivered runs once the Lease reply of epoch is with the link: the reasons
// the reply carried as of generation g (JOB in state.jobs, INTERRUPT in the
// interrupt level, STOP in stopped) are cleared, INPUT and APPROVAL stay
// until TakeInputs, whatever is still pending is nudged (after the reply),
// and the lease is extended from now on.
func (s *Service) delivered(id string, tok, g uint64) {
	s.mu.Lock()
	e := s.entries[id]
	if e == nil || e.token != tok || e.bound == nil {
		s.mu.Unlock()
		return
	}
	e.pending = false
	if e.abandoned {
		// Unwatched before the reply got out: the worker drops it.
		s.mu.Unlock()
		return
	}
	for _, v := range []reasonSet{reasonJob, reasonInterrupt, reasonStop} {
		if e.reasons&v != 0 && e.gen[bit(v)] <= g {
			e.reasons &^= v
		}
	}
	s.nudge(id, e)
	key := store.LeaseKey{SessionID: id, Epoch: e.holder.epoch, WorkerID: e.holder.worker, BootID: e.holder.boot, ConnID: e.holder.conn}
	s.mu.Unlock()
	// Extend now: the lease may have skipped an extension round while its
	// reply waited.
	now := s.nowMs()
	ctx, cancel := storeCtx(context.Background())
	lost, err := s.cfg.Store.ExtendLeases(ctx, []store.LeaseKey{key}, now, now+LeaseLifetime.Milliseconds())
	cancel()
	if err != nil || len(lost) > 0 {
		s.mu.Lock()
		s.queueWake(id, 0)
		s.mu.Unlock()
	}
}

// abandon runs when the AcquireLease that took token tok has ended without
// delivering its Lease reply (pending still set), or was unwatched
// (unwatched, delivered or not): the lease is no longer extended, so a lease
// the worker never learned of expires. A later acquisition or resumption of
// the session starts over.
func (s *Service) abandon(id string, tok uint64, unwatched bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[id]; e != nil && e.token == tok && (unwatched || e.pending) {
		e.abandoned = true
	}
}

// leaseReply reads the Lease reply of an acquired lease in one transaction.
func (c *Conn) leaseReply(ctx context.Context, id string, epoch int64, reattached bool) (*circusv1.SessionReply, error) {
	f := c.fence(id, epoch)
	lease := &circusv1.SessionReply_Lease{Epoch: uint64(epoch), Reattached: reattached}
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		rec, err := tx.GetRecovery(ctx, f)
		if err != nil {
			return err
		}
		sess, err := tx.GetSession(ctx, id, f.Now)
		if err != nil {
			return err
		}
		d := newDevices(tx, sess.UserID)
		if lease.State, err = buildState(ctx, tx, d, f, sess, store.RecordCwds|store.RecordFiles|store.RecordJobs|store.RecordGrants); err != nil {
			return err
		}
		calls, err := recoveryCalls(ctx, tx, d, f, rec)
		if err != nil {
			return err
		}
		lease.Recovery = &circusv1.Recovery{Calls: calls, PreviousTurnInterrupted: rec.LastTurnInterrupted}
		for _, b := range rec.LostBgTasks {
			lease.Recovery.LostBgtasks = append(lease.Recovery.LostBgtasks, &circusv1.PatchSessionState_BackgroundTask{
				TaskId: b.TaskID, ToolUseId: b.ToolUseID, Kind: b.Kind, Description: b.Description})
		}
		subpaths, err := tx.ListTranscriptSubpaths(ctx, f)
		if err != nil {
			return err
		}
		for _, sp := range subpaths {
			if sp.Subpath == "" && sp.LastSeq > 0 {
				lease.HasTranscript = true
			}
		}
		def, err := d.name(ctx, sess.DefaultDeviceID)
		if err != nil {
			return err
		}
		lease.Config = &circusv1.SessionConfig{DefaultDevice: def, Model: sess.Model, PermissionMode: sess.PermissionMode}
		for _, cw := range lease.State.Cwds {
			if cw.AgentId == "" {
				lease.Config.Cwds = append(lease.Config.Cwds, &circusv1.SessionConfig_Cwd{Device: cw.Device, Cwd: cw.Cwd})
			}
		}
		lease.ClaudeSessionId = sess.ClaudeSessionID
		lease.TranscriptGapTurnNo = uint64(max(sess.TranscriptGapTurnNo, 0))
		return nil
	})
	var fe *store.FencedError
	switch {
	case errors.As(err, &fe):
		return fencedReply(fe.Epoch), nil
	case err != nil:
		return nil, err
	}
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_Lease_{Lease: lease}}, nil
}

func (c *Conn) release(ctx context.Context, f store.Fence) *circusv1.SessionReply {
	var loss store.LeaseLoss
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		loss, err = tx.ReleaseLease(ctx, f)
		return err
	})
	if err != nil {
		return c.failed(ctx, f, "release", err)
	}
	c.s.lost(loss, c, true)
	return ok(loss.Repeated)
}

func (c *Conn) load(ctx context.Context, f store.Fence, r *circusv1.LoadTranscript) *circusv1.SessionReply {
	if r.GetMaxBytes() < 1 || r.GetMaxBytes() > maxLoadBytes || r.GetAfterSeq() > math.MaxInt64 {
		return invalidReply("max_bytes must be 1..8 MiB")
	}
	var page store.TranscriptPage
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		page, err = tx.LoadTranscript(ctx, f, store.TranscriptPageRequest{Subpath: r.GetSubpath(), AfterSeq: int64(r.GetAfterSeq()),
			Limit: store.MaxTranscriptPageEntries, MaxBytes: int64(r.GetMaxBytes())})
		return err
	})
	if err != nil {
		return c.failed(ctx, f, "load", err)
	}
	out := &circusv1.SessionReply_TranscriptPage{NextSeq: r.GetAfterSeq(), Done: page.Done}
	for _, e := range page.Entries {
		out.Entries = append(out.Entries, &circusv1.TranscriptEntry{Uuid: e.UUID, Json: string(e.JSON)})
		out.NextSeq = uint64(e.Seq)
	}
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_TranscriptPage_{TranscriptPage: out}}
}

func (c *Conn) keys(ctx context.Context, f store.Fence) *circusv1.SessionReply {
	var subpaths []store.TranscriptSubpath
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		subpaths, err = tx.ListTranscriptSubpaths(ctx, f)
		return err
	})
	if err != nil {
		return c.failed(ctx, f, "keys", err)
	}
	out := &circusv1.SessionReply_TranscriptKeys{}
	for _, sp := range subpaths {
		out.Subpaths = append(out.Subpaths, sp.Subpath)
	}
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_TranscriptKeys_{TranscriptKeys: out}}
}

func (c *Conn) append(ctx context.Context, f store.Fence, r *circusv1.AppendTranscript) *circusv1.SessionReply {
	if r.GetBatchSeq() > math.MaxInt64 {
		return invalidReply("batch_seq out of range")
	}
	b := store.TranscriptBatch{Subpath: r.GetSubpath(), BatchSeq: int64(r.GetBatchSeq()), Entries: make([]store.TranscriptEntry, len(r.GetEntries()))}
	for i, e := range r.GetEntries() {
		b.Entries[i] = store.TranscriptEntry{UUID: e.GetUuid(), JSON: []byte(e.GetJson())}
	}
	var res store.TranscriptAppended
	var user string
	var seq int64
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		if res, err = tx.AppendTranscript(ctx, f, b, c.s.quota); err != nil || !res.Full {
			return err
		}
		sess, err := tx.GetSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		user = sess.UserID
		if res.Stopped {
			// The stop's view event, in the transaction of the stop.
			seq, err = AppendStopEvent(ctx, tx, f.SessionID, StopReasonTranscriptFull, f.Now, c.s.cfg.MaxViewBytes)
		}
		return err
	})
	if err != nil {
		return c.failed(ctx, f, "append", err)
	}
	if res.Full {
		// The follow-ups belong to the stop, once: a later refusal of an
		// already stopped session only answers.
		if res.Stopped {
			if seq > 0 {
				c.s.cfg.Bus.Durable(f.SessionID, uint64(seq))
			}
			c.s.stopped(f.SessionID, user)
		}
		return rejected(circusv1.SessionReply_Rejected_CODE_TRANSCRIPT_FULL, "transcript full; the session is stopped")
	}
	return ok(res.Deduplicated)
}

// stopped runs the follow-ups of a stop the worker link caused (the
// transcript quota): Nudge STOP to the holder and stopping the session's
// device jobs.
func (s *Service) stopped(id, user string) {
	s.mu.Lock()
	if e := s.entries[id]; e != nil {
		s.addReasons(e, reasonStop)
		s.nudge(id, e)
	}
	s.mu.Unlock()
	if s.cfg.StopJobs == nil || user == "" {
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	select {
	case s.jobStops <- struct{}{}:
	default:
		s.mu.Unlock()
		s.log.Warn("too many concurrent job stops; session jobs not stopped", "session_id", id)
		return
	}
	s.jobs.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.jobs.Done()
		defer func() { <-s.jobStops }()
		ctx, cancel := context.WithTimeout(context.Background(), StoreTimeout)
		defer cancel()
		if err := s.cfg.StopJobs(ctx, user, id); err != nil {
			s.log.Warn("stopping the jobs of a session stopped by its transcript quota failed", "session_id", id, "error", err)
		}
	}()
}

func (c *Conn) take(ctx context.Context, f store.Fence) *circusv1.SessionReply {
	g := c.s.generation()
	var inputs []store.Input
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		inputs, err = tx.TakeInputs(ctx, f)
		return err
	})
	if err != nil {
		return c.failed(ctx, f, "take", err)
	}
	c.s.clearReasons(f.SessionID, f.Epoch, reasonInput|reasonApproval, g)
	out := &circusv1.SessionReply_Inputs{}
	for _, in := range inputs {
		kind := circusv1.SessionReply_Inputs_Input_KIND_USER
		if in.Kind == store.InputApproval {
			kind = circusv1.SessionReply_Inputs_Input_KIND_APPROVAL
		}
		prio := circusv1.InputPriority_INPUT_PRIORITY_NORMAL
		if in.Priority == store.InputNow {
			prio = circusv1.InputPriority_INPUT_PRIORITY_NOW
		}
		out.Inputs = append(out.Inputs, &circusv1.SessionReply_Inputs_Input{InputId: in.InputID, Kind: kind, Text: in.Text, Priority: prio})
	}
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_Inputs_{Inputs: out}}
}

func (c *Conn) report(ctx context.Context, f store.Fence, r *circusv1.ReportTurn) *circusv1.SessionReply {
	if len(r.GetUsageJson()) > maxUsageBytes || r.GetLocalSeq() > math.MaxInt64 {
		return invalidReply("usage_json over 4 KiB or local_seq out of range")
	}
	tr := store.TurnReport{LocalSeq: int64(r.GetLocalSeq()), Origin: turnOrigin(r.GetOrigin()), At: f.Now}
	switch r.GetPhase() {
	case circusv1.ReportTurn_PHASE_STARTED:
		tr.Phase = store.TurnStarted
	case circusv1.ReportTurn_PHASE_ENDED:
		tr.Phase, tr.Outcome, tr.Usage, tr.TranscriptIncomplete = store.TurnEnded, turnOutcome(r.GetOutcome()), usage(r.GetUsageJson()), r.GetTranscriptIncomplete()
	default:
		return invalidReply("phase must be STARTED or ENDED")
	}
	g := c.s.generation()
	var before store.Session
	var rep store.TurnReported
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		if tr.Persisted, err = persisted(ctx, tx, f, r.GetPersisted()); err != nil {
			return err
		}
		if before, err = tx.GetSession(ctx, f.SessionID, f.Now); err != nil {
			return err
		}
		rep, err = tx.ReportTurn(ctx, f, tr)
		return err
	})
	if err != nil {
		return c.failed(ctx, f, "report", err)
	}
	if rep.Session.LastEventSeq > before.LastEventSeq {
		c.s.cfg.Bus.Durable(f.SessionID, uint64(rep.Session.LastEventSeq))
	}
	c.s.updateInterrupt(f.SessionID, f.Epoch, rep.Session.InterruptTurnNo)
	if tr.Phase == store.TurnEnded {
		c.s.clearReasons(f.SessionID, f.Epoch, reasonInterrupt, g)
	}
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_Turn_{Turn: &circusv1.SessionReply_Turn{TurnNo: uint64(rep.Turn.TurnNo)}}}
}

func (s *Service) updateInterrupt(id string, epoch, level int64) {
	s.mu.Lock()
	if e := s.entries[id]; e != nil && e.holder.epoch == epoch {
		e.interrupt = level
	}
	s.mu.Unlock()
}

func (c *Conn) patch(ctx context.Context, f store.Fence, r *circusv1.PatchSessionState) *circusv1.SessionReply {
	p := store.RecordsPatch{ReturnedCallIDs: r.GetReturnedCallIds(), AwaitingCallIDs: r.GetAwaitingCallIds(), BgTasksEnded: r.GetBgtasksEnded()}
	for _, b := range r.GetBgtasksStarted() {
		p.BgTasksStarted = append(p.BgTasksStarted, store.BgTask{TaskID: b.GetTaskId(), ToolUseID: b.GetToolUseId(), Kind: b.GetKind(), Description: b.GetDescription()})
	}
	files := make([][]byte, len(r.GetFiles()))
	for i, fr := range r.GetFiles() {
		h, valid := sha256Bytes(fr.GetSha256())
		if !valid {
			return invalidReply("sha256 must be empty or 64 hexadecimal digits")
		}
		files[i] = h
	}
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if _, err := tx.CheckFence(ctx, f); err != nil {
			return err
		}
		sess, err := tx.GetSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		d := newDevices(tx, sess.UserID)
		for _, cw := range r.GetCwds() {
			dev, err := d.id(ctx, cw.GetDevice())
			if err != nil {
				return err
			}
			p.Cwds = append(p.Cwds, store.SessionCwd{AgentID: cw.GetAgentId(), DeviceID: dev, Cwd: cw.GetCwd()})
		}
		for i, fr := range r.GetFiles() {
			dev, err := d.id(ctx, fr.GetDevice())
			if err != nil {
				return err
			}
			p.Files = append(p.Files, store.SessionFile{DeviceID: dev, Path: fr.GetPath(), SHA256: files[i], MtimeMs: fr.GetMtimeUnixMs()})
		}
		if p.HandedJobCalls, err = jobCalls(ctx, tx, f, r.GetHandedJobs()); err != nil {
			return err
		}
		if p.Persisted, err = persisted(ctx, tx, f, r.GetPersisted()); err != nil {
			return err
		}
		return tx.PatchSessionRecords(ctx, f, p)
	})
	if err != nil {
		return c.failed(ctx, f, "patch", err)
	}
	return ok(false)
}

func (c *Conn) getState(ctx context.Context, f store.Fence, r *circusv1.GetSessionState) *circusv1.SessionReply {
	recs, withCalls, valid := sections(r.GetSections())
	if !valid {
		return invalidReply("sections: at most five distinct, or SECTION_NONE alone")
	}
	g := c.s.generation()
	var st *circusv1.SessionState
	var level int64
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var rec store.Recovery
		var err error
		if withCalls {
			rec, err = tx.GetRecovery(ctx, f)
		} else {
			_, err = tx.CheckFence(ctx, f)
		}
		if err != nil {
			return err
		}
		sess, err := tx.GetSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		level = sess.InterruptTurnNo
		d := newDevices(tx, sess.UserID)
		if st, err = buildState(ctx, tx, d, f, sess, recs); err != nil {
			return err
		}
		if withCalls {
			st.Calls, err = recoveryCalls(ctx, tx, d, f, rec)
		}
		return err
	})
	if err != nil {
		return c.failed(ctx, f, "state", err)
	}
	c.s.updateInterrupt(f.SessionID, f.Epoch, level)
	if recs&store.RecordJobs != 0 {
		c.s.clearReasons(f.SessionID, f.Epoch, reasonJob, g)
	}
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_State{State: &circusv1.SessionReply_SessionStateReply{State: st}}}
}

func (c *Conn) publish(ctx context.Context, f store.Fence, r *circusv1.PublishEvents) *circusv1.SessionReply {
	if r.GetBatchSeq() > math.MaxInt64 {
		return invalidReply("batch_seq out of range")
	}
	var durable []store.EventInput
	for _, ev := range r.GetEvents() {
		kind := eventKind(ev.GetKind())
		if ev.GetEphemeral() && kind != "" && !controlPlaneOnly(kind) {
			continue
		}
		// Durable events, and ephemeral ones the store would refuse: those
		// make the store consume the batch as dropped, all or nothing.
		durable = append(durable, store.EventInput{Kind: kind, Format: "claude-sdk", Payload: []byte(ev.GetJson()), At: f.Now,
			Boundary: kind == "TURN_STARTED" || kind == "RESULT"})
	}
	var res store.EventsAppended
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		var err error
		res, err = tx.AppendEvents(ctx, f.SessionID, &store.EventPublish{Fence: f, BatchSeq: int64(r.GetBatchSeq())}, durable, c.s.cfg.MaxViewBytes)
		return err
	})
	switch {
	case err != nil:
		return c.failed(ctx, f, "publish", err)
	case res.Deduplicated:
		return ok(true)
	case res.Dropped:
		return invalidReply("batch dropped: " + res.DropReason.Error())
	}
	i := 0
	for _, ev := range r.GetEvents() {
		if !ev.GetEphemeral() {
			if seq := res.Seqs[i]; seq > 0 {
				c.s.cfg.Bus.Durable(f.SessionID, uint64(seq))
			}
			i++
			continue
		}
		payload, truncated := ev.GetJson(), false
		if len(payload) > store.MaxEventPayloadBytes {
			payload, truncated = "", true
		}
		// The payload (at most 256 KiB, its own string of the decoded
		// request) outlives the link's receive charge; the Bus charges it to
		// every watcher's queue, which bounds what it retains (bus.go).
		c.s.cfg.Bus.Ephemeral(f.SessionID, &circusv1.SessionEvent{TsUnixMs: f.Now, Kind: ev.GetKind(), PayloadJson: payload, Ephemeral: true, Truncated: truncated})
	}
	return ok(false)
}

func (c *Conn) requestApproval(ctx context.Context, f store.Fence, r *circusv1.RequestApproval) *circusv1.SessionReply {
	if err := store.CheckID("tool use id", r.GetToolUseId()); err != nil {
		return invalidReply("tool_use_id is required, at most 128 bytes")
	}
	approvalID, err := uuid.NewV7()
	if err != nil {
		return c.s.failure(ctx, "approval", err)
	}
	var res store.ApprovalRequested
	err = c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if _, err := tx.CheckFence(ctx, f); err != nil {
			return err
		}
		sess, err := tx.GetSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		dev, err := newDevices(tx, sess.UserID).id(ctx, r.GetDevice())
		if err != nil {
			return err
		}
		res, err = tx.RequestApproval(ctx, f, store.Approval{ApprovalID: approvalID.String(), SessionID: f.SessionID, ToolUseID: r.GetToolUseId(),
			AgentID: r.GetAgentId(), Tool: r.GetTool(), DeviceID: dev, MatchKey: r.GetMatchKey(), Display: r.GetDisplay(), Reason: r.GetReason(),
			CreatedAt: f.Now, ExpiresAt: f.Now + ApprovalLifetime.Milliseconds()})
		return err
	})
	if err != nil {
		return c.failed(ctx, f, "approval", err)
	}
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_Approval_{Approval: &circusv1.SessionReply_Approval{
		ApprovalId: res.Approval.ApprovalID, Deduplicated: !res.Created}}}
}

func (c *Conn) consumeGrant(ctx context.Context, f store.Fence, r *circusv1.ConsumeGrant) *circusv1.SessionReply {
	if err := store.CheckID("tool use id", r.GetToolUseId()); err != nil {
		return invalidReply("tool_use_id is required, at most 128 bytes")
	}
	allowed := false
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if _, err := tx.CheckFence(ctx, f); err != nil {
			return err
		}
		sess, err := tx.GetSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		dev, err := newDevices(tx, sess.UserID).id(ctx, r.GetDevice())
		if errors.Is(err, store.ErrNotFound) {
			// No device of that name: no grant can match. The store's
			// stop and broken refusals still apply.
			dev = "unknown-device"
		} else if err != nil {
			return err
		}
		allowed, err = tx.ConsumeGrant(ctx, f, store.GrantKey{Tool: r.GetTool(), DeviceID: dev, MatchKey: r.GetMatchKey()}, r.GetToolUseId())
		return err
	})
	if err != nil {
		return c.failed(ctx, f, "grant", err)
	}
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_Grant_{Grant: &circusv1.SessionReply_Grant{Allowed: allowed}}}
}

func (c *Conn) repaired(ctx context.Context, f store.Fence) *circusv1.SessionReply {
	err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
		_, err := tx.TranscriptRepaired(ctx, f)
		return err
	})
	if err != nil {
		return c.failed(ctx, f, "repaired", err)
	}
	// Inputs and job notices that arrived while broken were not delivered.
	s := c.s
	s.mu.Lock()
	if e := s.entries[f.SessionID]; e != nil && e.holder.epoch == f.Epoch {
		s.addReasons(e, reasonInput|reasonJob)
		s.nudge(f.SessionID, e)
	}
	s.mu.Unlock()
	return ok(false)
}

// Resume serves ResumeLeases inline: each held (session, epoch) of this
// worker and boot that has not expired is rebound to this connection and
// extended; each resumed session's pending Nudge reasons are sent again.
func (c *Conn) Resume(ctx context.Context, r *circusv1.ResumeLeases) *circusv1.SessionReply {
	if c.bootID == "" {
		return invalidReply("session operations need a connection with a boot id")
	}
	s := c.s
	out := &circusv1.SessionReply_Resumed{Leases: make([]*circusv1.SessionReply_Resumed_Outcome, len(r.GetLeases()))}
	var keys []store.SessionEpoch
	var at []int
	for i, l := range r.GetLeases() {
		out.Leases[i] = &circusv1.SessionReply_Resumed_Outcome{SessionId: l.GetSessionId()}
		if identifier.Valid(l.GetSessionId(), false) && l.GetEpoch() >= 1 && l.GetEpoch() <= math.MaxInt64 {
			keys = append(keys, store.SessionEpoch{SessionID: l.GetSessionId(), Epoch: int64(l.GetEpoch())})
			at = append(at, i)
		}
	}
	if len(keys) > store.MaxResumeLeases {
		return invalidReply("at most 64 leases")
	}
	// Reserve an entry for every session that may resume, so a resumed
	// lease is always tracked; a full table refuses the whole request.
	s.mu.Lock()
	missing := 0
	for _, k := range keys {
		if s.entries[k.SessionID] == nil {
			missing++
		}
	}
	if len(s.entries)+missing > s.maxTracked {
		s.mu.Unlock()
		s.log.Warn("session table full; resume refused")
		return rejected(circusv1.SessionReply_Rejected_CODE_STORE_UNAVAILABLE, "session table full; retry")
	}
	for _, k := range keys {
		e := s.entries[k.SessionID]
		if e == nil {
			e = &entry{}
			s.entries[k.SessionID] = e
		}
		e.reserved++
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		for _, k := range keys {
			if e := s.entries[k.SessionID]; e != nil {
				e.reserved--
				s.forget(k.SessionID, e)
			}
		}
		s.mu.Unlock()
	}()
	now := s.nowMs()
	var res []store.LeaseResumed
	sessions := make([]store.Session, len(keys))
	if len(keys) > 0 {
		err := c.tx(ctx, func(ctx context.Context, tx store.Ops) error {
			var err error
			if res, err = tx.ResumeLeases(ctx, c.workerID, c.bootID, c.id, keys, now, now+LeaseLifetime.Milliseconds()); err != nil {
				return err
			}
			for j, lr := range res {
				if lr.Err == nil {
					if sessions[j], err = tx.GetSession(ctx, lr.SessionID, now); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return s.failure(ctx, "resume", err)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for j, lr := range res {
		o := out.Leases[at[j]]
		var fe *store.FencedError
		e := s.track(lr.SessionID, c.userID) // reserved, so present
		switch {
		case lr.Err == nil && e != nil:
			o.Ok, o.Epoch, o.Stopped = true, uint64(lr.Lease.Epoch), lr.Stopped
			sess := sessions[j]
			fresh := !e.known
			s.bind(lr.SessionID, e, lr.Lease, c, true)
			e.known, e.pending, e.abandoned, e.token = true, false, false, 0
			e.interrupt = sess.InterruptTurnNo
			if fresh {
				// Nothing is known of this lease in this process (it was
				// acquired before a restart): hints sent before may be
				// lost, so ask the holder to look, and to interrupt an
				// open turn at or below the interrupt level.
				s.addReasons(e, reasonInput|reasonJob)
				if sess.State == store.SessionRunning && sess.InterruptTurnNo > 0 && sess.InterruptTurnNo >= sess.TurnCount {
					s.addReasons(e, reasonInterrupt)
				}
			}
			if lr.Stopped {
				s.addReasons(e, reasonStop)
			}
			s.nudge(lr.SessionID, e)
		case errors.As(lr.Err, &fe):
			o.Epoch = uint64(max(fe.Epoch, 0))
			if e != nil && e.holder.worker == c.workerID && e.holder.epoch == keys[j].Epoch {
				s.queueWake(lr.SessionID, 0)
			}
		}
		if e != nil && !o.Ok {
			s.forget(lr.SessionID, e)
		}
	}
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_Resumed_{Resumed: out}}
}

// ListDevices returns the read that answers ListDevices: the user's live
// devices by name, at most 256.
func (c *Conn) ListDevices() ReadFunc {
	return plain(func(ctx context.Context) *circusv1.SessionReply {
		tctx, cancel := storeCtx(ctx)
		defer cancel()
		ids, err := c.s.cfg.Store.ListIdentities(tctx, store.KindDevice, c.userID)
		if err != nil {
			return c.s.failure(ctx, "devices", err)
		}
		var online map[string]bool
		if c.s.cfg.Online != nil {
			if online, err = c.s.cfg.Online(tctx, c.userID); err != nil {
				return c.s.failure(ctx, "devices", err)
			}
		}
		out := &circusv1.SessionReply_Devices{}
		for _, d := range ids[:min(len(ids), 256)] {
			out.Devices = append(out.Devices, &circusv1.SessionReply_Devices_Device{Name: d.Name, Online: online[d.ID], Os: clip(d.OS),
				Arch: clip(d.Arch), Hostname: clip(d.Hostname), LastSeenUnixMs: max(d.LastConnectedAt, d.LastDisconnectedAt)})
		}
		return &circusv1.SessionReply{Result: &circusv1.SessionReply_Devices_{Devices: out}}
	})
}

func clip(s string) string {
	if len(s) > maxAttachString {
		return strings.ToValidUTF8(s[:maxAttachString], "")
	}
	return s
}
