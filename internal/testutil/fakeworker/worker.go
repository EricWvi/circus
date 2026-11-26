// Package fakeworker runs a scripted worker against a real control plane and
// generates Claude Agent SDK messages locally.
//
// The following inputs select a script. Other input, and every note the
// control plane pushes (KIND_NOTE, a user message whose view event names it
// in circus_note, synthetic unless it is a ledger note) or answer
// (KIND_ANSWER), gets a streamed Markdown reply quoting the input.
//
//	/fake bash <command>     Run Bash on the session's default device and cwd.
//	/fake read <path>        Read a file on that device, relative to the session cwd.
//	/fake approve <command> Deny Bash and request a grant. Approval retries Bash.
//	/fake ask               Ask two questions (AskUserQuestion, one multiSelect)
//	                        and end the turn; the answers start the next turn,
//	                        which quotes them.
//	/fake slow <seconds>    Stretch a streamed reply for testing interruption.
//	/fake think             Emit a thinking block before replying.
//	/fake crash             Drop the process mid-turn and return with a new boot.
//	/fake blip              Drop the link, resume the current boot and finish the turn.
//	/fake break             Report a transcript mirror failure. Recover in the Web UI.
//	/fake job <command>      Run Bash in the background and deliver the completion notice.
//	/fake error             End the turn with a scripted error.
//	/fake ledger-path       Answer "ledger path: <path>" (or "none") from the lease.
//	/fake ledger <json>     Change the lease's ledger snapshot: a JSON array of
//	                        operations on paths relative to it, one device call
//	                        each (see ledgerOp).
package fakeworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/loopback"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/google/uuid"
)

const (
	retryDelay = 100 * time.Millisecond
	outageTime = 2 * time.Second
	opTimeout  = 30 * time.Second
)

var (
	errFenced   = errors.New("lease fenced")
	errStopped  = errors.New("session stopped")
	errBroken   = errors.New("session transcript broken")
	errAbort    = errors.New("turn interrupted by user")
	errNow      = errors.New("turn preempted by NOW input")
	errShutdown = errors.New("worker shutting down")
	errCrash    = errors.New("scripted process crash")
	errProvider = errors.New("session provider unavailable")
)

func isAbort(err error) bool { return errors.Is(err, errAbort) || errors.Is(err, errNow) }

// Options configures a fake worker. Zero durations select the defaults.
type Options struct {
	Name       string
	Credential credfile.File
	// URL overrides the server URL in Credential when nonempty.
	URL         string
	MaxSessions uint32
	IdleRelease time.Duration // Defaults to five minutes.
	StreamDelay time.Duration // Defaults to 100 ms between chunks.
	Clock       clock.Clock
	Logger      *slog.Logger
	// LaunchEnv, when set, receives the reply of the GetLaunchEnv the
	// worker sends after each acquisition, standing in for a CLI start:
	// a LaunchEnv or a rejection. The reply holds secrets; the worker
	// never logs it.
	LaunchEnv func(sessionID string, reply *pb.SessionReply)
	// Offer, when set, is told of every Offer the worker receives, and
	// Acquired of every lease it acquires (its epoch). Both are called
	// synchronously and must not block.
	Offer    func(sessionID string)
	Acquired func(sessionID string, epoch uint64)
}

// Run starts a new UUIDv7 boot and reconnects until ctx is canceled. It
// returns after joining the link reader and all active sessions.
func Run(ctx context.Context, o Options) error {
	if o.URL == "" {
		o.URL = o.Credential.ControlPlaneURL
	}
	if o.Credential.Role != credfile.RoleWorker || o.Credential.PrincipalID == "" || o.Credential.Token == "" {
		return errors.New("fakeworker: worker credential required")
	}
	_, httpClient, err := loopback.Link(o.URL, o.Credential.CAFile, "/v1/link/worker")
	if err != nil {
		return err
	}
	httpClient.CloseIdleConnections()
	if o.Name == "" {
		o.Name = "fakeworker"
	}
	if o.MaxSessions == 0 {
		o.MaxSessions = 4
	}
	if o.MaxSessions > 64 || o.IdleRelease < 0 || o.StreamDelay < 0 {
		return errors.New("fakeworker: max sessions must be 1..64 and delays must be nonnegative")
	}
	if o.IdleRelease == 0 {
		o.IdleRelease = 5 * time.Minute
	}
	if o.StreamDelay == 0 {
		o.StreamDelay = 100 * time.Millisecond
	}
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	w := &worker{
		ctx: ctx, opts: o, log: o.Logger.With("worker", o.Name),
		changed: make(chan struct{}), sessions: make(map[string]*session), offers: make(map[string]bool),
	}
	defer w.discardSessions(context.Canceled)
	return w.run()
}

type worker struct {
	ctx  context.Context
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	link     *connection
	changed  chan struct{}
	sessions map[string]*session
	offers   map[string]bool
	wg       sync.WaitGroup
}

type outage int

const (
	unplanned outage = iota
	blip
	crash
)

type connection struct {
	c     *wc.Client
	ops   chan struct{}
	calls chan struct{}
	// outage is guarded by the worker mutex.
	outage outage
}

func (w *worker) run() error {
	newBoot := true
	var boot uuid.UUID
	backoff := retryDelay
	for w.ctx.Err() == nil {
		if newBoot {
			var err error
			if boot, err = uuid.NewV7(); err != nil {
				return err
			}
			newBoot = false
		}
		l, events, err := w.dial(boot.String())
		if err != nil {
			w.log.Warn("worker connection failed", "error", err, "retry_in", backoff)
			if !w.sleep(w.ctx, backoff) {
				break
			}
			backoff = min(2*backoff, 10*time.Second)
			continue
		}
		connectedAt := w.opts.Clock.Now()
		w.log.Info("worker attached", "boot_id", boot)
		for {
			event, err := events.Next(w.ctx)
			if err != nil {
				break
			}
			w.hint(l, event)
		}
		if w.ctx.Err() != nil {
			w.discardSessions(errShutdown)
		}
		w.mu.Lock()
		if w.link == l {
			w.setLink(nil)
		}
		mode := l.outage
		w.mu.Unlock()
		_ = l.c.Close()

		if mode == crash {
			w.discardSessions(errCrash)
			newBoot = true
		}
		if mode != unplanned || w.opts.Clock.Now().Sub(connectedAt) >= 10*time.Second {
			backoff = retryDelay
		}
		pause := backoff
		if mode != unplanned {
			pause = outageTime
		}
		if !w.sleep(w.ctx, pause) {
			break
		}
		if mode == unplanned {
			backoff = min(2*backoff, 10*time.Second)
		}
	}
	return w.ctx.Err()
}

func (w *worker) dial(boot string) (*connection, wc.Events, error) {
	c, err := wc.Dial(w.ctx, wc.Options{
		URL: w.opts.URL, Token: w.opts.Credential.Token, PrincipalID: w.opts.Credential.PrincipalID,
		CAFile: w.opts.Credential.CAFile, BootID: boot,
	})
	if err != nil {
		return nil, wc.Events{}, err
	}
	events, err := c.Attach(w.ctx, &pb.Attach{MaxSessions: w.opts.MaxSessions, SdkVersion: "fake", ClaudeVersion: "fake"})
	if err != nil {
		_ = c.Close()
		return nil, wc.Events{}, err
	}
	if err := w.resume(w.ctx, c); err != nil {
		_ = c.Close()
		return nil, wc.Events{}, err
	}
	// Reserve call slots independently so long calls never stall session ops.
	l := &connection{c: c, calls: make(chan struct{}, 48), ops: make(chan struct{}, 15)}
	w.mu.Lock()
	w.setLink(l)
	w.mu.Unlock()
	return l, events, nil
}

func (w *worker) resume(ctx context.Context, c *wc.Client) error {
	w.mu.Lock()
	var held []*pb.ResumeLeases_Held
	for id, s := range w.sessions {
		if s.epoch != 0 && s.ctx.Err() == nil {
			held = append(held, &pb.ResumeLeases_Held{SessionId: id, Epoch: s.epoch})
		}
	}
	w.mu.Unlock()
	for chunk := range slices.Chunk(held, 64) {
		if err := w.resumeChunk(ctx, c, chunk); err != nil {
			return err
		}
	}
	return nil
}

func (w *worker) resumeChunk(ctx context.Context, c *wc.Client, held []*pb.ResumeLeases_Held) error {
	var reply *pb.SessionReply
	for {
		attempt, cancel := context.WithTimeout(ctx, opTimeout)
		r, err := c.Resume(attempt, held...)
		cancel()
		if err == nil {
			if r.GetResumed() != nil {
				reply = r
				break
			}
			err = replyError(r)
			if !rejected(err, pb.SessionReply_Rejected_CODE_STORE_UNAVAILABLE) {
				return fmt.Errorf("resume: %v", r)
			}
		} else if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, wire.ErrBusy) {
			return err
		}
		if !w.sleep(ctx, retryDelay) {
			return context.Cause(ctx)
		}
	}
	outcomes := make(map[string]*pb.SessionReply_Resumed_Outcome)
	for _, o := range reply.GetResumed().GetLeases() {
		outcomes[o.GetSessionId()] = o
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, lease := range held {
		s := w.sessions[lease.GetSessionId()]
		if s == nil || s.epoch != lease.GetEpoch() {
			continue
		}
		o := outcomes[s.id]
		if !o.GetOk() || o.GetEpoch() != s.epoch {
			s.cancel(errFenced)
		} else if o.GetStopped() {
			s.pending.stop = true
			s.signal()
		}
	}
	return nil
}

// setLink is called with w.mu held. Leases can use the new connection
// only after Attach and ResumeLeases have been sent and resume has succeeded.
func (w *worker) setLink(l *connection) {
	w.link = l
	if l == nil {
		clear(w.offers)
	}
	close(w.changed)
	w.changed = make(chan struct{})
}

func (w *worker) connection(ctx context.Context) (*connection, error) {
	for ctx.Err() == nil {
		w.mu.Lock()
		l, changed := w.link, w.changed
		w.mu.Unlock()
		if l != nil {
			select {
			case <-l.c.Done():
			default:
				return l, nil
			}
		}
		select {
		case <-changed:
		case <-ctx.Done():
		}
	}
	return nil, context.Cause(ctx)
}

func (w *worker) disrupt(l *connection, mode outage) bool {
	w.mu.Lock()
	if w.link != l {
		w.mu.Unlock()
		return false
	}
	l.outage = mode
	w.setLink(nil)
	w.mu.Unlock()
	_ = l.c.Close()
	return true
}

func (w *worker) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-w.opts.Clock.After(d):
		return true
	case <-ctx.Done():
		return false
	}
}

func (w *worker) discardSessions(cause error) {
	w.mu.Lock()
	for _, s := range w.sessions {
		s.cancel(cause)
	}
	w.mu.Unlock()
	w.wg.Wait()
}

func (w *worker) hint(l *connection, event *pb.WorkerEvent) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.link != l || w.ctx.Err() != nil {
		return
	}
	if revoked := event.GetLeaseRevoked(); revoked != nil {
		if s := w.sessions[revoked.GetSessionId()]; s != nil && s.epoch == revoked.GetEpoch() {
			s.cancel(errFenced)
		}
		return
	}
	if r := event.GetReleaseIdle(); r != nil {
		// Only the lease held at that epoch; the session decides whether it
		// is idle (serve).
		if s := w.sessions[r.GetSessionId()]; s != nil && s.epoch != 0 && s.epoch == r.GetEpoch() {
			s.pending.release = true
			s.signal()
		}
		return
	}
	n := event.GetNudge()
	id := n.GetSessionId()
	offer := event.GetOffer()
	if offer != nil {
		id = offer.GetSessionId()
		if w.opts.Offer != nil {
			w.opts.Offer(id)
		}
	}
	if id == "" {
		return
	}
	s := w.sessions[id]
	if s == nil {
		if len(w.sessions) < int(w.opts.MaxSessions) {
			w.startSession(id)
		} else if offer != nil {
			w.offers[id] = true
		}
		return
	}
	// A new offer can arrive before Release or Fenced reaches the actor.
	if offer != nil && (s.epoch != 0 || s.ctx.Err() != nil) {
		s.offered = true
	}
	if s.epoch == 0 || n == nil {
		return
	}
	s.pending.interrupt = max(s.pending.interrupt, n.GetInterruptTurnNo())
	for _, reason := range n.GetReasons() {
		switch reason {
		case pb.WorkerEvent_Nudge_REASON_INPUT, pb.WorkerEvent_Nudge_REASON_APPROVAL:
			s.pending.inputs = true
		case pb.WorkerEvent_Nudge_REASON_JOB:
			s.pending.jobs = true
		case pb.WorkerEvent_Nudge_REASON_STOP:
			s.pending.stop = true
		}
	}
	s.signal()
}

func (w *worker) startSession(id string) {
	delete(w.offers, id)
	ctx, cancel := context.WithCancelCause(context.WithoutCancel(w.ctx))
	s := &session{
		w: w, id: id, ctx: ctx, cancel: cancel, wake: make(chan struct{}, 1),
		seen: make(map[string]bool), silent: make(map[string]string),
	}
	w.sessions[id] = s
	w.wg.Go(func() {
		defer cancel(context.Canceled)
		err := s.run()
		if err != nil && s.ctx.Err() == nil && !errors.Is(err, errCrash) && !errors.Is(err, errFenced) {
			w.log.Warn("session ended", "session_id", id, "error", err)
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		delete(w.sessions, id)
		if w.link == nil || w.ctx.Err() != nil {
			return
		}
		if s.offered {
			w.startSession(id)
			return
		}
		for id := range w.offers {
			w.startSession(id)
			break
		}
	})
}

type rejection struct{ reply *pb.SessionReply_Rejected }

func (r rejection) Error() string {
	return fmt.Sprintf("%s: %s", r.reply.GetCode(), r.reply.GetMessage())
}

func replyError(r *pb.SessionReply) error {
	if r.GetFenced() != nil {
		return errFenced
	}
	if r := r.GetRejected(); r != nil {
		switch r.GetCode() {
		case pb.SessionReply_Rejected_CODE_STOPPED, pb.SessionReply_Rejected_CODE_TRANSCRIPT_FULL:
			return errStopped
		case pb.SessionReply_Rejected_CODE_BROKEN:
			return errBroken
		default:
			return rejection{r}
		}
	}
	return nil
}

func rejected(err error, code pb.SessionReply_Rejected_Code) bool {
	var r rejection
	return errors.As(err, &r) && r.reply.GetCode() == code
}

func (s *session) request(req *pb.SessionRequest) (*pb.SessionReply, error) {
	return s.requestWith(s.ctx, req)
}

func (s *session) requestWith(ctx context.Context, req *pb.SessionRequest) (*pb.SessionReply, error) {
	for {
		l, err := s.w.connection(ctx)
		if err != nil {
			return nil, err
		}
		attempt, cancel := context.WithTimeout(ctx, opTimeout)
		r, err := sessionOp(attempt, l, req)
		cancel()
		if errors.Is(err, wire.ErrTooLarge) {
			return nil, err
		}
		if errors.Is(err, wire.ErrBusy) || errors.Is(err, context.DeadlineExceeded) {
			if !s.w.sleep(ctx, retryDelay) {
				return nil, context.Cause(ctx)
			}
			continue
		}
		if err != nil {
			// The operation timed out only after queuing Unwatch. Other
			// transport failures require reconnecting.
			if ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			_ = l.c.Close()
			continue
		}

		s.w.mu.Lock()
		current := s.w.link == l && ctx.Err() == nil
		if current && req.GetAcquireLease() != nil && r.GetLease() != nil {
			s.epoch = r.GetLease().GetEpoch()
		}
		s.w.mu.Unlock()
		if !current {
			continue
		}
		if err := replyError(r); !rejected(err, pb.SessionReply_Rejected_CODE_STORE_UNAVAILABLE) {
			return r, err
		}
		if !s.w.sleep(ctx, retryDelay) {
			return nil, context.Cause(ctx)
		}
	}
}

func sessionOp(ctx context.Context, l *connection, req *pb.SessionRequest) (*pb.SessionReply, error) {
	select {
	case l.ops <- struct{}{}:
		defer func() { <-l.ops }()
	case <-l.c.Done():
		return nil, l.c.Err()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	r, err := l.c.Send(ctx, req)
	if err != nil {
		return nil, err
	}
	return r.Wait(ctx)
}
