// Package workerlink serves the worker link at /v1/link/worker: device calls
// through the call router, and session operations through the session
// service (Config.Sessions).
//
// Sessions: a worker Hello.boot_id must be empty (a call-only client) or a
// UUIDv7 not older than the worker's current boot (else BAD_HELLO); the
// connection is registered with the session service before its first
// message is read, which releases an older boot's leases. Attach opens a
// hint stream that holds its operation slot until Unwatch or disconnect; a
// second Attach or one breaking its field caps closes the link. Session
// writes, Attach and ResumeLeases are handled inline in the receive loop;
// their one SessionReply goes to the connection's reply queue, whose one
// sender hands it to the link, retrying a busy send pool, so the receive
// loop does not wait for the pool. The queue holds at most 16 MiB of encoded
// replies (one larger alone; only then does the loop wait), and a queued
// reply keeps its operation slot until it reaches the link. Reads run in
// their operation's goroutine under the service's global read permits; a
// reply the pool refuses gives its read permit back and is retried, without
// a permit when it is at most smallUpdateBytes, else under one of the push
// permits, or, with none free, rebuilt under a push permit taken once one
// has freed (that wait holds no permit; the permit is then kept through the
// rebuild and its send, all within one send deadline started at the drop).
// Every Unwatch is also passed to the session service, which abandons the
// lease of a recent AcquireLease whose reply may already have gone out. A
// slot is released before its reply is sent, as for calls. A SubmitCall carrying
// session_epoch or tool_use_id is admitted by the router against the lease
// bound to this connection (calls.Admission). With Config.Sessions nil the
// session payloads close the link as unexpected.
//
// Each SubmitCall, CallQuery and CancelCall opens one operation under the
// envelope's client-chosen request_id; every CallUpdate sent for it carries
// that ID. A zero ID, the ID of an operation that is still open, or a 65th
// open operation on the connection closes the link with a logged reason (the
// client mirrors the cap, so a conforming client never hits it). Submit
// streams updates until its terminal one; Query and Cancel send exactly one.
// An operation's slot is released before its last update becomes visible, so
// a client may reuse it as soon as that update arrives. Unwatch releases the
// named operation's slot inline in the receive loop, so a following operation
// in the same stream finds it free, then cancels the operation's context: a
// Submit's router subscription closes and nothing more is sent for it (an
// update already being sent may still arrive). Unwatch never cancels a call;
// unknown or finished IDs are a no-op.
//
// Router errors map as follows. store.ErrInvalid becomes Rejected INVALID.
// During Query or Cancel, store.ErrNotFound becomes Rejected NOT_FOUND with
// message "unknown call id", which answers only that operation and is not a
// call outcome. calls.ErrDeviceOffline becomes nonterminal Unknown with reason
// DEVICE_OFFLINE. calls.ErrResultUnavailable becomes Lost with reason
// RESULT_UNAVAILABLE. A router-produced CallUpdate, including a proven-safe
// OVERLOADED or STORE_UNAVAILABLE rejection, is forwarded unchanged. A Go
// calls.ErrOverloaded from Submit or Query, an ambiguous store failure,
// subscription overflow, or any other unmapped error closes the link with a
// logged reason. The worker reconnects and queries; transient admission
// failure is never fabricated into a terminal call outcome. A router rejection
// with an empty call ID also closes, because CallUpdate.call_id is required.
// Unknown keeps its subscription. Cancel records the intent inline, in receive
// order behind any earlier Submit (a failure to queue it to the daemon after
// it is recorded is the router's to replay, not an error), and then answers
// with the call's state read after that: Cancelled before dispatch,
// Dispatched/Unknown when the daemon's result will decide, the outcome of an
// already finished call, or the mapped rejection of the Cancel itself.
// Closing subscriptions never cancels execution.
//
// Connections: at most Config.MaxConnections (default 8) authenticated
// worker-role connections (long-lived workers and `circus call` CLIs, both
// use worker credentials), and at most four per credential, so one worker
// cannot lock out another credential. CLIs using the same credential as a
// worker count toward that worker's four. An authenticated connection holds its
// place from before the handshake until it closes. HTTP 503 means a cap is
// full, the authenticator failed (for example the store is unavailable), or
// the server is shutting down.
//
// Welcome advertises max_in_flight_messages = 1 on this link: at most one
// partially transferred fragmented message per direction, so a minimal client
// needs a single reassembly buffer and a single FIFO send queue.
//
// Outbound updates: every update, including the answer to a Query or
// Cancel, reaches this handler through a router subscription (Submit or
// QueryOnce), where it stays charged to the router's buffered-update budget
// until taken. A forwarding goroutine waits with calls.Subscription.Wait,
// which consumes nothing and reports the head update's charged cost. An
// update of at most 256 KiB (smallUpdateBytes) is then taken and sent at
// once. A larger one first takes one of four push permits shared by all
// connections, and holds it until the link has accepted the update. Waiting
// for the router, for the daemon behind a Query, or for a permit holds only
// the operation's logical slot, which Unwatch frees inline; the receive loop
// never waits for any of them. There is no per-connection goroutine bound
// beyond the 64 slots: an unwatched operation's goroutine leaves the table
// at once, and every point where it can block (the router call, Wait, a
// permit, a send retry) ends with its context.
//
// Sends retry wire.ErrBusy with bounded backoff for at most 10 seconds (below
// the router's default 15-second SubscriberStall and wire's default
// MaxConsumerStall), then close the link. The 256 MiB default send pool is
// shared by all connections, not reserved per link. A large update waiting
// for a permit is not being read, so the router's stall clock keeps running
// for it: when the send pool stays saturated, permit holders retry for up to
// 10 s each, and a large update queued behind enough of them can exceed the
// 15 s stall and close its link. Small updates never wait for a permit. No
// router callback waits for this handler.
//
// Worst case memory (live ownership, excluding runtime/GC, SQLite, kernel/TLS)
// with C = MaxConnections connections and M = the message limit:
//   - shared wire pools, twice MinBudget: with M >= MaxFragmentBytes+128,
//     4M send and 8M+12 MiB+8 KiB receive (256 + 524 MiB at M=64 MiB);
//   - per connection 5 MiB+64 KiB wire overhead plus one borrowed inbound
//     proto (the receive loop handles one message at a time), each
//     <=2M+6 MiB+4 KiB: C*(5 MiB+64 KiB + 2M+6 MiB+4 KiB);
//   - four push permits, each covering one large update taken from the
//     router, <=2M+6 MiB+4 KiB, independent of C and of the 64 slots;
//   - small updates taken without a permit: at most one per operation
//     goroutine, so C*64*256 KiB;
//   - router-owned buffers, including queued Query/Cancel answers (globally
//     <=256 MiB, pending <=256 MiB, counted by calls rather than again here).
//
// At defaults (C=8, M=64 MiB): 780 MiB + 8*139 MiB (1,112 MiB) + 4*134 MiB
// (536 MiB) + 8*64*256 KiB (128 MiB) = about 2,557 MiB (2.5 GiB), plus
// router budgets and bounded operation scratch: request-ID tables (<=64
// entries per connection, each holding a call ID of at most 64 KiB, so
// <=C*4 MiB), operation goroutines, and updates built here (rejections,
// Lost, Unknown), which are bounded by the metadata size. Handshakes borrow
// <=64 KiB each. Smaller M or C reduces these bounds. These are conservative
// ownership bounds, not RSS.
// After Shutdown's bounded join expires, remaining peer goroutines may still
// call the router; its closed admission path then returns calls.ErrClosed.
package workerlink

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

const (
	// DefaultMaxConnections is Config.MaxConnections when zero.
	DefaultMaxConnections = 8
	// maxPerCredential keeps one worker from taking every connection.
	maxPerCredential = 4
	maxPerUser       = 4
	maxOperations    = 64
	// pushPermits bounds, across all connections, the large router-owned
	// updates taken from subscriptions and not yet accepted by the link.
	pushPermits = 4
	// smallUpdateBytes is the largest charged cost (wire.Charge) an update
	// may have to be sent without a push permit.
	smallUpdateBytes = 256 << 10
	// maxInlineReplyBytes bounds the encoded inline session replies one
	// connection holds before they reach the link.
	maxInlineReplyBytes = 16 << 20
	// maxBootIDBytes bounds Hello.boot_id; longer is BAD_HELLO.
	maxBootIDBytes = 128
	// sendBusyLimit must stay below wire.Options.MaxConsumerStall (15s default).
	sendBusyLimit = 10 * time.Second
)

// Config shares the host's single authenticator and call router. Only a
// worker credential is admitted; Recheck (zero: the auth.RecheckPolicy
// defaults) revalidates it. Clock (zero: real time) times the handshake,
// rechecks and send retries; Wire.Clock times the links themselves.
// MaxConnections (zero: DefaultMaxConnections) bounds authenticated worker
// links; it sizes the worst-case memory (see the package comment). Each
// user and each credential may hold at most four authenticated links.
type Config struct {
	Auth           auth.LinkAuthenticator
	Router         calls.Service
	InstanceID     string
	Clock          clock.Clock
	Wire           wire.Options
	Logger         *slog.Logger
	Recheck        auth.RecheckPolicy
	MaxConnections int
	// Sessions serves Attach, SessionRequest, ResumeLeases and ListDevices
	// and records worker boots. Nil leaves the link a call link: those
	// payloads close it as unexpected.
	Sessions *sessions.Service
}

// Handler owns every admitted handshake, subscription and query. Shutdown
// aborts and bounds the join by its context; an ordinary ServeHTTP return joins
// all operation goroutines before releasing the link.
type Handler struct {
	cfg     Config
	mu      sync.Mutex
	closed  bool
	peers   map[*peer]struct{}
	changed chan struct{}
	// push is shared by every connection; see pushPermits.
	push semaphore
	// submit and query are Router.Submit and Router.QueryOnce; tests replace
	// them to drive updates.
	submit func(ctx context.Context, user, device string, call *circusv1.DeviceCall, a calls.Admission) (subscription, error)
	query  func(ctx context.Context, user, id string) (subscription, error)
	// maxInline is maxInlineReplyBytes (lowered by tests).
	maxInline int
	// sessionSend, when set by tests, runs before each SessionReply send
	// attempt; its error replaces the attempt's.
	sessionSend func(m *circusv1.WorkerDownlink) error
	// beforeSend and afterSend, when set by tests, run before each update is
	// handed to the link and after the link accepted it.
	beforeSend, afterSend func(context.Context, *circusv1.CallUpdate)
}
type peer struct {
	cancel     context.CancelFunc
	credential string
	user       string
}

// subscription is the part of *calls.Subscription a push uses.
type subscription interface {
	Wait(context.Context) (int64, error)
	Next(context.Context) (*circusv1.CallUpdate, error)
	Close() error
}

// semaphore is a counting semaphore; acquire gives up when ctx ends.
type semaphore chan struct{}

func (s semaphore) acquire(ctx context.Context) bool {
	select {
	case s <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}
func (s semaphore) release() { <-s }

// tryAcquire takes a permit only if one is free.
func (s semaphore) tryAcquire() bool {
	select {
	case s <- struct{}{}:
		return true
	default:
		return false
	}
}

// inlineReply is one inline SessionReply waiting for the connection's
// reply sender.
type inlineReply struct {
	rid   uint64
	op    *operation
	reply *circusv1.SessionReply
	size  int
}

// replyQueue is one connection's FIFO of inline replies, bounded by the
// encoded bytes it holds (a single reply larger than the bound is admitted
// alone).
type replyQueue struct {
	mu      sync.Mutex
	items   []inlineReply
	bytes   int
	max     int
	changed chan struct{}
}

func newReplyQueue(max int) *replyQueue { return &replyQueue{max: max, changed: make(chan struct{})} }

func (q *replyQueue) signal() { close(q.changed); q.changed = make(chan struct{}) }

// put queues it, waiting while the queue is full; false when ctx ended.
func (q *replyQueue) put(ctx context.Context, it inlineReply) bool {
	for {
		q.mu.Lock()
		if q.bytes == 0 || q.bytes+it.size <= q.max {
			q.items = append(q.items, it)
			q.bytes += it.size
			q.signal()
			q.mu.Unlock()
			return true
		}
		ch := q.changed
		q.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return false
		}
	}
}

// next takes the oldest reply, waiting for one; false when ctx ended.
// Its bytes stay counted until done.
func (q *replyQueue) next(ctx context.Context) (inlineReply, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			it := q.items[0]
			q.items[0] = inlineReply{}
			q.items = q.items[1:]
			q.mu.Unlock()
			return it, true
		}
		ch := q.changed
		q.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return inlineReply{}, false
		}
	}
}

// done releases the bytes of a reply handed to the link (or dropped).
func (q *replyQueue) done(size int) {
	q.mu.Lock()
	q.bytes -= size
	q.signal()
	q.mu.Unlock()
}

// New validates configuration and creates an independent, bounded wire budget.
func New(c Config) (*Handler, error) {
	if c.Auth == nil || c.Router == nil || c.InstanceID == "" || len(c.InstanceID) > 128 || c.Wire.Budget != nil || c.MaxConnections < 0 {
		return nil, errors.New("workerlink: invalid dependencies")
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = DefaultMaxConnections
	}
	var err error
	if c.Recheck, err = c.Recheck.Normalize(); err != nil {
		return nil, err
	}
	if c.Clock == nil {
		c.Clock = clock.Real{}
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	// One partial fragmented message per direction lets a minimal client
	// keep a single reassembly buffer and a single FIFO send queue.
	c.Wire.MaxInFlightMessages = 1
	opts, err := wire.FillWelcome(&circusv1.Welcome{}, c.Wire)
	if err != nil {
		return nil, err
	}
	s, r, err := wire.MinBudget(opts)
	if err != nil {
		return nil, err
	}
	opts.Budget, err = wire.NewBudget(2*s, 2*r)
	if err != nil {
		return nil, err
	}
	c.Wire = opts
	h := &Handler{cfg: c, peers: make(map[*peer]struct{}), changed: make(chan struct{}), push: make(semaphore, pushPermits), maxInline: maxInlineReplyBytes}
	h.submit = func(ctx context.Context, user, device string, call *circusv1.DeviceCall, a calls.Admission) (subscription, error) {
		s, err := c.Router.SubmitSession(ctx, user, device, call, a)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	h.query = func(ctx context.Context, user, id string) (subscription, error) {
		s, err := c.Router.QueryOnce(ctx, user, id)
		if err != nil {
			return nil, err
		}
		return s, nil
	}
	return h, nil
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/link/worker" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	principal, ok := auth.Admit(w, r, h.cfg.Auth, h.cfg.Logger, store.KindWorker)
	if !ok {
		return
	}
	// Authentication is deliberately outside the bounded peer set: invalid
	// credentials cannot occupy every connection slot. Authenticated handshakes
	// count until they finish, so they cannot accumulate without bound.
	p := &peer{cancel: cancel, credential: principal.CredentialID, user: principal.UserID}
	h.mu.Lock()
	sameCredential, sameUser := 0, 0
	for q := range h.peers {
		if q.credential == p.credential {
			sameCredential++
		}
		if q.user == p.user {
			sameUser++
		}
	}
	if h.closed || len(h.peers) >= h.cfg.MaxConnections || sameCredential >= maxPerCredential || sameUser >= maxPerUser {
		h.mu.Unlock()
		reason := "workerlink overloaded; retry after an existing worker link closes"
		switch {
		case sameCredential >= maxPerCredential:
			reason = "too many worker links for this credential; retry after one closes"
		case sameUser >= maxPerUser:
			reason = "too many worker links for this user; retry after one closes"
		}
		http.Error(w, reason, http.StatusServiceUnavailable)
		return
	}
	h.peers[p] = struct{}{}
	h.mu.Unlock()
	defer func() {
		cancel()
		h.mu.Lock()
		delete(h.peers, p)
		close(h.changed)
		h.changed = make(chan struct{})
		h.mu.Unlock()
	}()
	conn, err := wire.AcceptWebSocket(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	abortDone := make(chan struct{})
	go func() { defer close(abortDone); <-ctx.Done(); _ = conn.Close() }()
	defer func() { cancel(); <-abortDone }()
	handshake, finish := clock.WithTimeout(ctx, h.cfg.Clock, wire.HelloTimeout)
	hello, opts, err := wire.AcceptHello(handshake, conn, wire.WorkerUplink, h.cfg.Wire, func(hello *circusv1.Hello) *circusv1.Welcome {
		if len(hello.GetBootId()) > maxBootIDBytes {
			return nil
		}
		// A boot id that is not a UUIDv7, or older than the worker's
		// current boot (a replaced process), is BAD_HELLO and releases
		// nothing. A store failure here is not a refusal: Connect checks
		// again and closes the link, which the worker retries.
		if h.cfg.Sessions != nil && errors.Is(h.cfg.Sessions.CheckBoot(handshake, principal.ID, hello.GetBootId()), sessions.ErrBoot) {
			return nil
		}
		return &circusv1.Welcome{PrincipalId: principal.ID, InstanceId: h.cfg.InstanceID, ServerTimeUnixMs: h.cfg.Clock.Now().UnixMilli()}
	})
	finish()
	if err != nil {
		return
	}
	link, err := wire.New(conn, wire.WorkerControlSide, opts)
	if err != nil {
		return
	}
	defer link.Close()
	var wg sync.WaitGroup
	defer func() { cancel(); _ = link.Close(); wg.Wait() }()
	wg.Add(1)
	go func() { defer wg.Done(); h.recheck(ctx, cancel, principal.CredentialID) }()
	var sc *sessions.Conn
	if h.cfg.Sessions != nil {
		sc, err = h.cfg.Sessions.Connect(ctx, sessions.ConnInfo{WorkerID: principal.ID, UserID: principal.UserID, BootID: hello.GetBootId(),
			Fail: func(reason string) {
				h.cfg.Logger.WarnContext(ctx, "worker session hint closed link", "reason", reason)
				cancel()
			}})
		if err != nil {
			h.cfg.Logger.WarnContext(ctx, "worker connection refused", "error", err)
			return
		}
		defer sc.Close()
	}
	ops := newOperations(maxOperations)
	// protocolError closes the link for a client that broke the request-ID
	// or operation-cap rules; the client cannot be answered in band.
	protocolError := func(reason string, id uint64) {
		h.cfg.Logger.WarnContext(ctx, "worker protocol error closed link", "reason", reason, "request_id", id)
		cancel()
	}
	// send delivers u as the answer to operation rid. opCtx ends with the
	// operation (Unwatch) or the link; an ended operation's send is dropped
	// silently, any other send failure closes the link.
	send := func(opCtx context.Context, rid uint64, u *circusv1.CallUpdate) {
		if u.GetRejected() != nil && u.GetCallId() == "" {
			h.cfg.Logger.WarnContext(ctx, "worker operation closed link", "reason", "router rejection has empty call ID", "request_id", rid)
			cancel()
			return
		}
		if h.beforeSend != nil {
			h.beforeSend(opCtx, u)
		}
		m := &circusv1.WorkerDownlink{RequestId: rid, Payload: &circusv1.WorkerDownlink_CallUpdate{CallUpdate: u}}
		err := h.sendWithBusyRetry(opCtx, func(sendCtx context.Context) error { return link.Send(sendCtx, m) })
		if err != nil && opCtx.Err() == nil {
			h.cfg.Logger.WarnContext(ctx, "worker send closed link", "error", err)
			cancel()
		}
		if err == nil && h.afterSend != nil {
			h.afterSend(opCtx, u)
		}
	}
	failure := func(opCtx context.Context, rid uint64, id, op string, err error) {
		if opCtx.Err() != nil {
			return // unwatched or the link is closing; nobody is waiting
		}
		var u *circusv1.CallUpdate
		oneShot := op == "query" || op == "cancel"
		switch {
		case oneShot && errors.Is(err, store.ErrNotFound):
			u = &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Rejected_{Rejected: &circusv1.CallUpdate_Rejected{Code: circusv1.CallUpdate_Rejected_CODE_NOT_FOUND, Message: "unknown call id"}}}
		case errors.Is(err, store.ErrInvalid):
			u = &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Rejected_{Rejected: &circusv1.CallUpdate_Rejected{Code: circusv1.CallUpdate_Rejected_CODE_INVALID}}}
		case errors.Is(err, calls.ErrResultUnavailable):
			u = &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Lost_{Lost: &circusv1.CallUpdate_Lost{Reason: "RESULT_UNAVAILABLE"}}}
		case oneShot && errors.Is(err, calls.ErrDeviceOffline):
			u = &circusv1.CallUpdate{CallId: id, Update: &circusv1.CallUpdate_Unknown_{Unknown: &circusv1.CallUpdate_Unknown{Reason: "DEVICE_OFFLINE"}}}
		default:
			// In particular, calls.ErrOverloaded from Submit/Query is ambiguous
			// for an existing call. Never turn it into terminal Rejected.
			h.cfg.Logger.WarnContext(ctx, "worker operation closed link", "operation", op, "error", err)
			cancel()
			return
		}
		send(opCtx, rid, u)
	}
	// admit opens operation rid in receive order without waiting for
	// anything. Rule violations close the link.
	admit := func(rid uint64) (*operation, bool) {
		op, reason := ops.open(ctx, rid)
		if reason != "" {
			protocolError(reason, rid)
			return nil, false
		}
		return op, true
	}
	// forward sends sub's updates for operation rid until its last one: the
	// terminal update, or the only one when oneShot (a Query or Cancel
	// answer). The slot is released before the last update, or a failure's
	// replacement for it, becomes visible, so a client that acts on it cannot
	// race the release; an unwatched operation sends nothing more.
	forward := func(rid uint64, op *operation, id string, sub subscription, oneShot bool) {
		h.pushUpdates(op, sub, func(u *circusv1.CallUpdate) bool {
			if oneShot || wire.Terminal(u) {
				if ops.end(rid, op) {
					send(op.ctx, rid, u)
				}
				return false
			}
			send(op.ctx, rid, u)
			return true
		}, func(err error) {
			if ops.end(rid, op) {
				failure(op.ctx, rid, id, "subscription", err)
			}
		})
	}
	// reply answers a Query or Cancel in its own goroutine. Waiting for the
	// router (and the daemon behind it) holds only the slot; the answer is
	// then handed out exactly like a push.
	reply := func(rid uint64, op *operation, id, kind string, fetch func(context.Context) (subscription, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer op.cancel()
			defer ops.end(rid, op)
			sub, err := fetch(op.ctx)
			if err != nil {
				if ops.end(rid, op) {
					failure(op.ctx, rid, id, kind, err)
				}
				return
			}
			defer sub.Close()
			forward(rid, op, id, sub, true)
		}()
	}
	// sendReply sends rid's one SessionReply after its slot was released.
	// A failure other than the operation ending closes the link.
	// sendSession hands one SessionReply to the link (one attempt).
	sendSession := func(sendCtx context.Context, m *circusv1.WorkerDownlink) error {
		if h.sessionSend != nil {
			if err := h.sessionSend(m); err != nil {
				return err
			}
		}
		return link.Send(sendCtx, m)
	}
	// sendReply sends rid's one SessionReply after its slot was released,
	// retrying a busy send pool. A failure other than the operation ending
	// closes the link. It reports whether the reply reached the link.
	sendReply := func(opCtx context.Context, rid uint64, r *circusv1.SessionReply) bool {
		m := &circusv1.WorkerDownlink{RequestId: rid, Payload: &circusv1.WorkerDownlink_SessionReply{SessionReply: r}}
		err := h.sendWithBusyRetry(opCtx, func(sendCtx context.Context) error { return sendSession(sendCtx, m) })
		if err != nil && opCtx.Err() == nil {
			h.cfg.Logger.WarnContext(ctx, "worker send closed link", "error", err)
			cancel()
		}
		return err == nil
	}
	// Inline replies go through this connection's reply queue and its one
	// sender, so the receive loop never waits for the shared send pool. A
	// queued reply keeps its operation slot until it reaches the link, and
	// the queue holds at most maxInlineReplyBytes (one larger reply alone):
	// only when that much is queued does the receive loop wait for it.
	replies := newReplyQueue(h.maxInline)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			it, ok := replies.next(ctx)
			if !ok {
				return
			}
			if ops.end(it.rid, it.op) {
				sendReply(it.op.ctx, it.rid, it.reply)
			}
			it.op.cancel()
			replies.done(it.size)
		}
	}()
	answer := func(rid uint64, op *operation, r *circusv1.SessionReply) {
		if !replies.put(ctx, inlineReply{rid: rid, op: op, reply: r, size: proto.Size(r)}) {
			op.cancel()
		}
	}
	// read builds a read reply in the operation's own goroutine under one
	// of the service's global read permits and makes one send attempt. When
	// the send pool is busy, the read permit is given back before any wait:
	// a reply of at most smallUpdateBytes is retried without a permit (as
	// small call updates are); a larger one is retried under one of the
	// global push permits. With none free it is dropped; the goroutine waits
	// for a push permit holding nothing, keeps it, waits for a read permit
	// again, rebuilds the reply and sends it under the kept permit (a busy
	// pool is retried under that same permit, never a second one). Every
	// wait after the drop (push permit, read permit, the rebuilt reply's
	// busy retries) shares one sendBusyLimit deadline started at the drop;
	// past it the link closes. Reads are idempotent and recheck their fence. So at most
	// ReadConcurrency replies are being built, at most pushPermits large
	// ones wait for the pool or hold a permit for a rebuild (a push permit
	// is held across a read-permit wait for at most sendBusyLimit), and no
	// busy retry holds a read permit.
	read := func(rid uint64, op *operation, fn sessions.ReadFunc) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer op.cancel()
			defer ops.end(rid, op)
			var m *circusv1.WorkerDownlink
			var sent func()
			ended := false
			// build makes the reply; false when there is nothing to send.
			build := func() bool {
				r, after, err := fn(op.ctx)
				if err != nil {
					if op.ctx.Err() == nil {
						h.cfg.Logger.WarnContext(ctx, "worker session reply closed link", "error", err)
						cancel()
					}
					return false
				}
				if !ended {
					if !ops.end(rid, op) {
						return false
					}
					ended = true
				}
				m = &circusv1.WorkerDownlink{RequestId: rid, Payload: &circusv1.WorkerDownlink_SessionReply{SessionReply: r}}
				sent = after
				return true
			}
			attempt := func(sendCtx context.Context) error { return sendSession(sendCtx, m) }
			const (
				done = iota
				retry
				retryPermit
				rebuild
			)
			var err error
			// permit: a push permit this goroutine holds, taken while it held
			// nothing; it is kept for the rebuilt reply's send.
			permit := false
			// waitCtx bounds every wait: the operation's context until the
			// reply is first dropped, then a sendBusyLimit deadline started
			// at that drop and shared by every later wait (the push permit,
			// the read permit, the rebuilt reply's busy retries), so a
			// rebuild gets no fresh sendBusyLimit and a long first wait for a
			// read permit does not count against it.
			waitCtx, dropped := op.ctx, false
			stop := func() {}
			defer func() {
				stop()
				if permit {
					h.push.release()
				}
			}()
			for {
				if waitCtx.Err() != nil {
					if op.ctx.Err() == nil {
						h.cfg.Logger.WarnContext(ctx, "worker send closed link", "error", "send pool busy too long")
						cancel()
					}
					return
				}
				next := done
				ok := h.cfg.Sessions.Read(waitCtx, func() {
					if !build() {
						m = nil
						return
					}
					if err = attempt(op.ctx); !errors.Is(err, wire.ErrBusy) {
						return
					}
					switch {
					case proto.Size(m) <= smallUpdateBytes:
						next = retry
					case permit:
						next = retryPermit
					case h.push.tryAcquire():
						permit = true
						next = retryPermit
					default:
						next, m, sent = rebuild, nil, nil
					}
				})
				if !ok {
					continue // the deadline check above ends the loop
				}
				if next != rebuild {
					if next == retry || next == retryPermit {
						err = h.sendWithBusyRetry(waitCtx, attempt)
					}
					break
				}
				// Wait for a free push permit, holding nothing, keep it, and
				// start over: the rebuilt reply is sent under it.
				if !dropped {
					dropped = true
					waitCtx, stop = clock.WithTimeout(op.ctx, h.cfg.Clock, sendBusyLimit)
				}
				if !h.push.acquire(waitCtx) {
					continue // the deadline check above ends the loop
				}
				permit = true
			}
			if m == nil {
				return
			}
			if err != nil {
				if op.ctx.Err() == nil {
					h.cfg.Logger.WarnContext(ctx, "worker send closed link", "error", err)
					cancel()
				}
				return // not delivered: no sent()
			}
			if sent != nil {
				sent()
			}
		}()
	}
	for {
		m, err := link.Receive(ctx)
		if err != nil {
			return
		}
		up := m.(*circusv1.WorkerUplink)
		rid := up.GetRequestId()
		switch v := up.Payload.(type) {
		case *circusv1.WorkerUplink_SubmitCall:
			id := v.SubmitCall.GetCall().GetCallId()
			op, ok := admit(rid)
			if !ok {
				continue
			}
			// Submit admission stays in receive order so a following Cancel cannot
			// overtake it. The only long block is a stuck store transaction, bounded
			// by the store's busy timeout. The router copies content; the goroutine
			// retains only sub, whose lifetime is the operation's context.
			adm := calls.Admission{Epoch: v.SubmitCall.GetSessionEpoch(), ToolUseID: v.SubmitCall.GetToolUseId()}
			if adm != (calls.Admission{}) {
				adm.WorkerID, adm.BootID = principal.ID, hello.GetBootId()
				if sc != nil {
					adm.ConnID = sc.ID()
				}
			}
			sub, err := h.submit(op.ctx, principal.UserID, v.SubmitCall.Device, v.SubmitCall.Call, adm)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer op.cancel()
				defer ops.end(rid, op)
				if err != nil {
					// Off the receive loop: even a small rejection may wait
					// for the shared send pool.
					if ops.end(rid, op) {
						failure(op.ctx, rid, id, "submit", err)
					}
					return
				}
				defer sub.Close()
				forward(rid, op, id, sub, false)
			}()
		case *circusv1.WorkerUplink_CallQuery:
			id := v.CallQuery.CallId
			if op, ok := admit(rid); ok {
				reply(rid, op, id, "query", func(ctx context.Context) (subscription, error) {
					return h.query(ctx, principal.UserID, id)
				})
			}
		case *circusv1.WorkerUplink_CancelCall:
			id := v.CancelCall.CallId
			op, ok := admit(rid)
			if !ok {
				continue
			}
			// The cancellation itself stays inline, so it cannot overtake an
			// earlier Submit. The only long block is a stuck store transaction,
			// bounded by the store's busy timeout. Its reply is the call's state
			// read after the intent was recorded.
			cancelErr := h.cfg.Router.Cancel(op.ctx, principal.UserID, id)
			reply(rid, op, id, "cancel", func(ctx context.Context) (subscription, error) {
				if cancelErr != nil {
					return nil, cancelErr
				}
				return h.query(ctx, principal.UserID, id)
			})
		case *circusv1.WorkerUplink_Unwatch:
			// Inline, so a Submit that follows in the stream finds the slot free.
			if rid == 0 {
				protocolError("zero request id", rid)
				continue
			}
			ops.unwatch(rid)
			if sc != nil {
				sc.Unwatched(rid)
			}
		case *circusv1.WorkerUplink_Attach:
			if sc == nil {
				h.cfg.Logger.WarnContext(ctx, "worker protocol error closed link", "reason", "unexpected payload")
				return
			}
			op, ok := admit(rid)
			if !ok {
				continue
			}
			// The stream holds its slot until Unwatch or disconnect; its
			// events are sent by the session service's sender for this
			// connection, which ends with op.ctx.
			err := sc.Attach(op.ctx, v.Attach, func(evCtx context.Context, ev *circusv1.WorkerEvent) error {
				m := &circusv1.WorkerDownlink{RequestId: rid, Payload: &circusv1.WorkerDownlink_WorkerEvent{WorkerEvent: ev}}
				return h.sendWithBusyRetry(evCtx, func(sendCtx context.Context) error { return link.Send(sendCtx, m) })
			})
			if err != nil {
				protocolError(err.Error(), rid)
			}
		case *circusv1.WorkerUplink_Session:
			if sc == nil {
				h.cfg.Logger.WarnContext(ctx, "worker protocol error closed link", "reason", "unexpected payload")
				return
			}
			op, ok := admit(rid)
			if !ok {
				continue
			}
			r, fn := sc.Handle(op.ctx, rid, v.Session)
			if fn != nil {
				read(rid, op, fn)
			} else {
				answer(rid, op, r)
			}
		case *circusv1.WorkerUplink_ResumeLeases:
			if sc == nil {
				h.cfg.Logger.WarnContext(ctx, "worker protocol error closed link", "reason", "unexpected payload")
				return
			}
			if op, ok := admit(rid); ok {
				answer(rid, op, sc.Resume(op.ctx, v.ResumeLeases))
			}
		case *circusv1.WorkerUplink_ListDevices:
			if sc == nil {
				h.cfg.Logger.WarnContext(ctx, "worker protocol error closed link", "reason", "unexpected payload")
				return
			}
			if op, ok := admit(rid); ok {
				read(rid, op, sc.ListDevices())
			}
		case *circusv1.WorkerUplink_Heartbeat:
		default:
			h.cfg.Logger.WarnContext(ctx, "worker protocol error closed link", "reason", "unexpected payload")
			return
		}
	}
}

// operation is one open request ID. ctx ends on Unwatch or with the link.
type operation struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// operations is one connection's request-ID table. Each open operation holds
// a logical slot (its table entry) from admission until its last update is
// about to be sent or it is unwatched. Router-owned updates are bounded by
// the Handler's global push and reply permits, not per connection.
type operations struct {
	mu    sync.Mutex
	byID  map[uint64]*operation
	limit int
}

func newOperations(limit int) *operations {
	return &operations{byID: make(map[uint64]*operation), limit: limit}
}

// open admits request ID id, or returns why the client broke the protocol.
func (o *operations) open(ctx context.Context, id uint64) (*operation, string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	switch {
	case id == 0:
		return nil, "zero request id"
	case o.byID[id] != nil:
		return nil, "duplicate request id"
	case len(o.byID) >= o.limit:
		return nil, "per-connection operation cap full"
	}
	op := &operation{}
	op.ctx, op.cancel = context.WithCancel(ctx)
	o.byID[id] = op
	return op, ""
}

// end releases op's slot once and reports whether this call released it: false
// means it was unwatched (or already ended), so nothing may be sent for it.
func (o *operations) end(id uint64, op *operation) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.byID[id] != op {
		return false
	}
	delete(o.byID, id)
	return true
}

// unwatch releases the slot of operation id at once and stops its work;
// unknown or finished IDs are a no-op.
func (o *operations) unwatch(id uint64) {
	o.mu.Lock()
	op := o.byID[id]
	delete(o.byID, id)
	o.mu.Unlock()
	if op != nil {
		op.cancel()
	}
}

// pushUpdates forwards sub's updates until deliver returns false or the
// subscription or op ends. Each update stays in the router, charged to its
// budget, until Wait reports it ready. An update whose charged cost is at
// most smallUpdateBytes is then taken at once; a larger one only once a
// global push permit is held, kept until deliver has handed it to the link.
// So at most pushPermits large router-owned updates are outside the router
// across all connections, and small updates never wait behind large ones. A
// waiting push holds only its logical slot, which Unwatch frees inline.
func (h *Handler) pushUpdates(op *operation, sub subscription, deliver func(*circusv1.CallUpdate) bool, fail func(error)) {
	for {
		cost, err := sub.Wait(op.ctx)
		if err != nil {
			return // unwatched or the link is closing
		}
		permit := cost > smallUpdateBytes
		if permit && !h.push.acquire(op.ctx) {
			return
		}
		u, err := sub.Next(op.ctx)
		if err == nil {
			err = op.ctx.Err() // unwatched while waiting: send nothing
		}
		if err != nil {
			if permit {
				h.push.release()
			}
			fail(err)
			return
		}
		more := deliver(u)
		if permit {
			h.push.release()
		}
		if !more {
			return
		}
	}
}

// sendWithBusyRetry treats shared-pool exhaustion as recoverable. All other
// failures return immediately. The retry schedule and total wait are bounded.
func (h *Handler) sendWithBusyRetry(ctx context.Context, send func(context.Context) error) error {
	return sendWithBusyRetryLimit(ctx, h.cfg.Clock, sendBusyLimit, send)
}

func sendWithBusyRetryLimit(ctx context.Context, clk clock.Clock, limit time.Duration, send func(context.Context) error) error {
	const initial = 10 * time.Millisecond
	retryCtx, cancel := clock.WithTimeout(ctx, clk, limit)
	defer cancel()
	backoff := initial
	for {
		err := send(retryCtx)
		if !errors.Is(err, wire.ErrBusy) {
			return err
		}
		select {
		case <-retryCtx.Done():
			if err := ctx.Err(); err != nil {
				return err
			}
			return context.DeadlineExceeded
		case <-clk.After(backoff):
		}
		backoff = min(2*backoff, 100*time.Millisecond)
	}
}

func (h *Handler) recheck(ctx context.Context, cancel context.CancelFunc, id string) {
	h.cfg.Recheck.Watch(ctx, h.cfg.Clock, func(ctx context.Context) (bool, error) {
		return h.cfg.Auth.Valid(ctx, id)
	}, func(definitive bool, lastValid time.Time, err error) {
		h.cfg.Logger.WarnContext(ctx, "worker credential revalidation closed link", "definitive_revoke", definitive, "last_valid", lastValid, "error", err)
		cancel()
	})
}

// Shutdown refuses new upgrades and aborts links. It joins peers until ctx
// expires, then returns the context error instead of waiting indefinitely.
func (h *Handler) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	for p := range h.peers {
		p.cancel()
	}
	h.mu.Unlock()
	for {
		h.mu.Lock()
		n, changed := len(h.peers), h.changed
		h.mu.Unlock()
		if n == 0 {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
