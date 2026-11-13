// Package workerlink serves the M2 call subset at /v1/link/worker.
//
// Router errors map as follows. store.ErrInvalid becomes Rejected INVALID.
// During Query, store.ErrNotFound becomes Rejected NOT_FOUND with message
// "unknown call id", which answers only that Query and is not a call outcome.
// calls.ErrDeviceOffline becomes nonterminal Unknown with reason DEVICE_OFFLINE.
// calls.ErrResultUnavailable becomes Lost with reason RESULT_UNAVAILABLE.
// A router-produced CallUpdate, including a proven-safe OVERLOADED or
// STORE_UNAVAILABLE rejection, is forwarded unchanged. A Go calls.ErrOverloaded
// from Submit or Query, a full per-connection operation cap, an ambiguous store
// failure, subscription overflow, or any other unmapped error closes the link
// with a logged reason. The worker reconnects and queries; transient admission
// failure is never fabricated into a terminal call outcome. A router rejection
// with an empty call ID also closes because the client cannot route it.
// Unknown keeps its subscription. Cancel is inline and slot-free and never
// sends a reply: success, unknown IDs and invalid IDs only log, while other
// router errors close the link. A watched subscription carries the real terminal
// update, while a CLI can issue a subsequent Query. Closing subscriptions never
// cancels execution.
//
// M2 supports one long-lived worker plus one CLI connection at a time. HTTP 503
// means those two connection slots are full.
//
// Sends retry wire.ErrBusy with bounded backoff for at most 10 seconds (below
// the router's default 15-second SubscriberStall and wire's default
// MaxConsumerStall), then close the link. The 256 MiB default send pool is
// shared by both connections, not reserved per link. Every send uses wirepolicy;
// no router callback waits for this handler.
//
// Worst case memory (live ownership, excluding runtime/GC, SQLite, kernel/TLS):
// two connections and four subscriptions/queries each, no extra inbox/outbox;
// inline cancels take no operation slot, goroutine, or reply. The independent
// shared wire pools are twice MinBudget: with
// M >= MaxFragmentBytes+128, 4M send and 8M+12 MiB+8 KiB receive; at M=64 MiB
// this is 256/524 MiB+8 KiB. Add at most 2*(5 MiB+64 KiB) wire overhead, two
// borrowed inbound and eight borrowed outbound protos, each <=2M+6 MiB+4 KiB,
// and router-owned buffers (globally <=256 MiB, pending <=256 MiB, counted by
// calls rather than again here). Worker transport/borrowed ownership is thus
// <=2,131 MiB (about 2.1 GiB) at defaults, plus router budgets and bounded
// operation scratch. Handshakes borrow <=64 KiB each. Smaller M reduces these
// bounds. These are conservative ownership bounds, not RSS.
// After Shutdown's bounded join expires, remaining peer goroutines may still
// call the router; its closed admission path then returns calls.ErrClosed.
package workerlink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wirepolicy"
	"google.golang.org/protobuf/proto"
)

const (
	maxConnections = 2
	maxOperations  = 4
	// sendBusyLimit must stay below wire.Options.MaxConsumerStall (15s default).
	sendBusyLimit = 10 * time.Second
)

// Authenticator is the worker auth seam. The host supplies the single shared
// auth.Authenticator; tests may inject revalidation failures.
type Authenticator interface {
	Authenticate(context.Context, *http.Request) (auth.Principal, error)
	Valid(context.Context, string) (bool, error)
}

// Config shares the host's single authenticator and call router. Zero durations
// use 10s Hello, 1s recheck, 5s check timeout and 30s maximum credential
// staleness. Revalidation errors and timeouts are retried with an initial
// per-link phase jitter; only a definitive revoke closes immediately.
type Config struct {
	Auth                                          Authenticator
	Router                                        calls.Service
	InstanceID                                    string
	Wire                                          wire.Options
	Logger                                        *slog.Logger
	HelloTimeout, RecheckInterval, RecheckTimeout time.Duration
	RecheckMaxStale                               time.Duration
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
}
type peer struct{ cancel context.CancelFunc }

// New validates configuration and creates an independent, bounded wire budget.
func New(c Config) (*Handler, error) {
	if c.Auth == nil || c.Router == nil || c.InstanceID == "" || len(c.InstanceID) > 128 || c.Wire.Budget != nil {
		return nil, errors.New("workerlink: invalid dependencies")
	}
	if c.HelloTimeout == 0 {
		c.HelloTimeout = 10 * time.Second
	}
	if c.RecheckInterval == 0 {
		c.RecheckInterval = time.Second
	}
	if c.RecheckTimeout == 0 {
		c.RecheckTimeout = 5 * time.Second
	}
	if c.RecheckMaxStale == 0 {
		c.RecheckMaxStale = 30 * time.Second
	}
	if c.HelloTimeout <= 0 || c.RecheckInterval <= 0 || c.RecheckTimeout <= 0 || c.RecheckMaxStale <= 0 {
		return nil, errors.New("workerlink: invalid timeout")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
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
	return &Handler{cfg: c, peers: make(map[*peer]struct{}), changed: make(chan struct{})}, nil
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
	principal, err := h.cfg.Auth.Authenticate(ctx, r)
	switch {
	case errors.Is(err, auth.ErrUnauthenticated):
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	case errors.Is(err, auth.ErrRevoked):
		http.Error(w, "revoked", http.StatusForbidden)
		return
	case err != nil:
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return
	case principal.Kind != store.KindWorker:
		http.Error(w, "worker role required; use a worker credential", http.StatusForbidden)
		return
	}
	// Authentication is deliberately outside the bounded peer set: invalid
	// credentials cannot occupy every connection slot. Authenticated handshakes
	// count until they finish, so they cannot accumulate without bound.
	p := &peer{cancel: cancel}
	h.mu.Lock()
	if h.closed || len(h.peers) >= maxConnections {
		h.mu.Unlock()
		http.Error(w, "workerlink overloaded; retry after an existing worker link closes", http.StatusServiceUnavailable)
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
	handshake, finish := context.WithTimeout(ctx, h.cfg.HelloTimeout)
	err = h.hello(handshake, conn, principal)
	finish()
	if err != nil {
		return
	}
	link, err := wire.New(conn, wire.WorkerControlSide, h.cfg.Wire)
	if err != nil {
		return
	}
	defer link.Close()
	var wg sync.WaitGroup
	defer func() { cancel(); _ = link.Close(); wg.Wait() }()
	wg.Add(1)
	go func() { defer wg.Done(); h.recheck(ctx, cancel, principal.CredentialID) }()
	slots := make(chan struct{}, maxOperations)
	handlers := make(chan struct{}, maxOperations)
	send := func(u *circusv1.CallUpdate) {
		if u.GetRejected() != nil && u.GetCallId() == "" {
			h.cfg.Logger.WarnContext(ctx, "worker operation closed link", "reason", "router rejection has empty call ID")
			cancel()
			return
		}
		m := &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_CallUpdate{CallUpdate: u}}
		class, key, err := wirepolicy.Classify(m)
		if err == nil {
			err = sendWithBusyRetry(ctx, func(sendCtx context.Context) error { return link.Send(sendCtx, m, class, key) })
		}
		if err != nil && ctx.Err() == nil {
			h.cfg.Logger.WarnContext(ctx, "worker send closed link", "error", err)
			cancel()
		}
	}
	failure := func(id, op string, err error) {
		var u *circusv1.CallUpdate
		oneShot := op == "query"
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
		send(u)
	}
	admit := func(op string) bool {
		return acquireSendSlot(ctx, slots, handlers, func() {
			h.cfg.Logger.WarnContext(ctx, "worker operation closed link", "operation", op, "reason", "per-connection operation cap full")
			cancel()
		})
	}
	query := func(id string) {
		if !admit("query") {
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-handlers }()
			held := true
			defer func() {
				if held {
					<-slots
				}
			}()
			u, err := h.cfg.Router.Query(ctx, principal.UserID, id)
			// Release before the answer becomes visible, so a client that acts on
			// it cannot race the old operation's deferred slot release.
			<-slots
			held = false
			if err != nil {
				failure(id, "query", err)
			} else {
				send(u)
			}
		}()
	}
	for {
		m, err := link.Receive(ctx)
		if err != nil {
			return
		}
		switch v := m.(*circusv1.WorkerUplink).Payload.(type) {
		case *circusv1.WorkerUplink_SubmitCall:
			id := v.SubmitCall.GetCall().GetCallId()
			if !admit("submit") {
				continue
			}
			// Submit admission stays in receive order so a following Cancel cannot
			// overtake it. The only long block is a stuck store transaction, bounded
			// by the store's busy timeout; there is no worker-link send-slot wait.
			// The router copies content; the goroutine retains only sub.
			sub, err := h.cfg.Router.Submit(ctx, principal.UserID, v.SubmitCall.Device, v.SubmitCall.Call)
			if err != nil {
				<-slots
				<-handlers
				failure(id, "submit", err)
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-handlers }()
				held := true
				defer func() {
					if held {
						<-slots
					}
				}()
				defer sub.Close()
				for {
					u, err := sub.Next(ctx)
					if err != nil {
						if ctx.Err() == nil {
							failure(id, "subscription", err)
						}
						return
					}
					if Terminal(u) {
						// As with Query, terminal visibility follows slot release.
						<-slots
						held = false
						send(u)
						return
					}
					send(u)
				}
			}()
		case *circusv1.WorkerUplink_CallQuery:
			query(v.CallQuery.CallId)
		case *circusv1.WorkerUplink_CancelCall:
			id := v.CancelCall.CallId
			// Inline, slot-free cancellation cannot overtake an earlier Submit.
			// Cancel never has a reply; only genuine router failures close the link.
			// The only long block is a stuck store transaction, bounded by the
			// store's busy timeout; there is no worker-link send-slot wait.
			err := h.cfg.Router.Cancel(ctx, principal.UserID, id)
			switch {
			case err == nil:
			case errors.Is(err, store.ErrNotFound):
				h.cfg.Logger.DebugContext(ctx, "worker cancel found no call", "call_id", id)
			case errors.Is(err, store.ErrInvalid):
				h.cfg.Logger.DebugContext(ctx, "worker cancel rejected invalid call ID", "call_id", id)
			default:
				h.cfg.Logger.WarnContext(ctx, "worker cancel closed link", "error", err)
				cancel()
			}
		case *circusv1.WorkerUplink_Heartbeat:
		default:
			return
		}
		// WorkerJobEvent fanout belongs here in M3.
	}
}
func (h *Handler) hello(ctx context.Context, conn wire.Conn, p auth.Principal) error {
	data, err := conn.ReadMessage(ctx)
	if err != nil {
		return err
	}
	m, err := wire.Decode(wire.WorkerUplink, data, wire.MaxMetadataBytes)
	code := circusv1.Reject_CODE_BAD_HELLO
	if err == nil {
		hello := m.(*circusv1.WorkerUplink).GetHello()
		if hello != nil {
			if hello.ProtocolVersion != 1 {
				code = circusv1.Reject_CODE_VERSION_UNSUPPORTED
			} else {
				w := &circusv1.Welcome{PrincipalId: p.ID, InstanceId: h.cfg.InstanceID, NegotiatedVersion: 1, ServerTimeUnixMs: time.Now().UnixMilli()}
				if _, err = wire.FillWelcome(w, h.cfg.Wire); err != nil {
					return err
				}
				data, err = proto.Marshal(&circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_Welcome{Welcome: w}})
				if err != nil {
					return err
				}
				return conn.WriteMessage(ctx, data)
			}
		}
	}
	data, _ = proto.Marshal(&circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_Reject{Reject: &circusv1.Reject{Code: code, MinVersion: 1, MaxVersion: 1}}})
	_ = conn.WriteMessage(ctx, data)
	_ = conn.Shutdown(ctx)
	return fmt.Errorf("workerlink: rejected Hello: %s", code)
}

// acquireSendSlot admits a logical operation, then waits for its send-lifetime
// slot rather than exceeding the goroutine/outbound cap. A full logical cap is
// the only immediate overload; the logical slot is released on cancellation.
func acquireSendSlot(ctx context.Context, slots, handlers chan struct{}, onFull func()) bool {
	select {
	case slots <- struct{}{}:
	default:
		onFull()
		return false
	}
	select {
	case handlers <- struct{}{}:
		return true
	case <-ctx.Done():
		<-slots
		return false
	}
}

// sendWithBusyRetry treats shared-pool exhaustion as recoverable. All other
// failures return immediately. The retry schedule and total wait are bounded.
func sendWithBusyRetry(ctx context.Context, send func(context.Context) error) error {
	return sendWithBusyRetryLimit(ctx, sendBusyLimit, send)
}

func sendWithBusyRetryLimit(ctx context.Context, limit time.Duration, send func(context.Context) error) error {
	const initial = 10 * time.Millisecond
	retryCtx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	backoff := initial
	for {
		err := send(retryCtx)
		if !errors.Is(err, wire.ErrBusy) {
			return err
		}
		if retryCtx.Err() != nil {
			return retryCtx.Err()
		}
		timer := time.NewTimer(backoff)
		select {
		case <-retryCtx.Done():
			timer.Stop()
			return retryCtx.Err()
		case <-timer.C:
		}
		backoff = min(2*backoff, 100*time.Millisecond)
	}
}

func (h *Handler) recheck(ctx context.Context, cancel context.CancelFunc, id string) {
	lastValid := time.Now()
	// Jitter only the phase. The resulting periodic schedule still observes a
	// definitive revoke within one interval plus one check timeout.
	delay := time.Duration(rand.Float64() * float64(h.cfg.RecheckInterval))
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		check, finish := context.WithTimeout(ctx, h.cfg.RecheckTimeout)
		valid, err := h.cfg.Auth.Valid(check, id)
		timedOut := check.Err() != nil
		finish()
		if ctx.Err() != nil {
			return
		}
		if !timedOut && err == nil && valid {
			lastValid = time.Now()
		} else {
			definitive := !timedOut && (errors.Is(err, auth.ErrRevoked) || err == nil && !valid)
			if definitive || time.Since(lastValid) >= h.cfg.RecheckMaxStale {
				h.cfg.Logger.WarnContext(ctx, "worker credential revalidation closed link", "definitive_revoke", definitive, "last_valid", lastValid, "error", err)
				cancel()
				return
			}
		}
		delay = h.cfg.RecheckInterval
	}
}

// Terminal recognizes actual call outcomes. Unknown and the operation-scoped
// NOT_FOUND answer deliberately do not settle a call.
func Terminal(u *circusv1.CallUpdate) bool {
	if r := u.GetRejected(); r != nil && r.GetCode() == circusv1.CallUpdate_Rejected_CODE_NOT_FOUND {
		return false
	}
	return u.GetResult() != nil || u.GetOffline() != nil || u.GetLost() != nil || u.GetRejected() != nil || u.GetCancelled() != nil
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
