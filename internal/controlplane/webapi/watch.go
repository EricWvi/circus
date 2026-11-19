package webapi

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/store"
)

// WatchSession timing defaults.
const (
	// DefaultWatchKeepalive is how long a WatchSession stream may stay
	// silent before the server sends a keepalive: an ephemeral SessionEvent
	// with kind KIND_UNSPECIFIED, seq 0 and no payload, which clients
	// ignore. A stream whose replay is empty also starts with one, since
	// response headers are sent with the first message. 25 seconds stays
	// under the 30- and 60-second idle timeouts common in reverse proxies
	// and load balancers.
	DefaultWatchKeepalive = 25 * time.Second
	// DefaultWatchRecheck is how often a stream checks that its browser
	// session is still signed in; it ends UNAUTHENTICATED once it is not.
	DefaultWatchRecheck = 30 * time.Second
	// DefaultWatchMaxLifetime ends a stream UNAVAILABLE after about this
	// long whatever happens (each stream draws its own lifetime within
	// ±10 %, so streams opened together do not all reconnect together);
	// the client reconnects with after_seq.
	DefaultWatchMaxLifetime = time.Hour
	// DefaultWatchWriteTimeout ends a SessionService response whose writes
	// made no progress for this long: a client that stopped reading loses
	// the stream, a slow one that keeps reading does not (streamWriter).
	DefaultWatchWriteTimeout = 30 * time.Second
)

// streamLifetime draws a stream's lifetime: base ± 10 %, uniformly.
func streamLifetime(base time.Duration) time.Duration {
	return time.Duration(float64(base) * (0.9 + 0.2*rand.Float64()))
}

// Replay pages: at most watchPageEvents events and watchPageBytes of
// payload per store read. One stored payload is at most 256 KiB, so a page
// still holds at least one event, and a stream holds at most one page.
const (
	watchPageEvents = store.MaxPageSize
	watchPageBytes  = store.MaxEventPayloadBytes
)

var (
	errShuttingDown = errors.New("the server is shutting down")
	errSignedOut    = errors.New("the browser session has ended; sign in again")
	errStreamExpiry = errors.New("the stream reached its maximum lifetime; reconnect with after_seq")
)

// WatchSession replays the durable events after after_seq from the store,
// page by page, then follows the session live through the bus.
//
// No gap and no duplicate between replay and live: the watcher subscribes
// before the replay's last read, so every durable commit after that read is
// announced to it, and every event is sent only when its seq is above the
// last one sent. A durable announcement makes the stream read the store
// from the last seq sent up to the announced seq, no further, before the
// items queued after the announcement. Ephemeral events are sent only
// live, in publication order relative to the announcements; one queued
// before an announcement the stream has already passed is stale and
// skipped. Best effort only: a durable event committed before its
// announcement reaches the bus may be read and sent by a catch-up first,
// and an ephemeral event published in that window still follows it. A slow
// stream loses ephemeral events only (the bus's queue bounds); durable ones
// are read from the store whatever was lost.
//
// The stream ends NOT_FOUND when the session is deleted, UNAVAILABLE when
// the server shuts down or the stream reached its maximum lifetime,
// UNAUTHENTICATED once its browser session is signed out or expired
// (checked every DefaultWatchRecheck), and CANCELED when the client goes
// away. Writes that make no progress for the write timeout end the
// stream. Deletion and shutdown cut a write in progress at once; the client
// then sees a transport error instead of a status and reconnects with
// after_seq. Otherwise the final status and the end of the response are
// written under the write timeout, so a client that does not read keeps the
// stream for up to one write timeout more.
func (s *Service) WatchSession(ctx context.Context, req *connect.Request[circusv1.WatchSessionRequest], stream *connect.ServerStream[circusv1.SessionEvent]) error {
	u, err := user(ctx)
	if err != nil {
		return err
	}
	id := req.Msg.GetSessionId()
	if err := sessionIDArg(id); err != nil {
		return err
	}
	if req.Msg.GetAfterSeq() > math.MaxInt64 {
		return invalidField("after_seq", "after_seq is out of range")
	}
	if _, err := ownSession(ctx, s.st, u, id, s.now().UnixMilli()); err != nil {
		return s.sessionError(ctx, "watch session", err)
	}
	w, err := s.bus.Watch(id, u.ID)
	switch {
	case errors.Is(err, sessions.ErrTooManyUserWatchers), errors.Is(err, sessions.ErrTooManyWatchers):
		return connect.NewError(connect.CodeResourceExhausted, errors.New("too many open session streams"))
	case errors.Is(err, sessions.ErrBusClosed):
		return connect.NewError(connect.CodeUnavailable, errShuttingDown)
	case err != nil:
		return s.internal(ctx, "watch session", err)
	}
	defer w.Close()
	ws := &watchStream{s: s, ctx: ctx, id: id, stream: stream, watcher: w, cursor: int64(req.Msg.GetAfterSeq())}
	if sw, ok := ctx.Value(streamWriterKey{}).(*streamWriter); ok {
		// Fail a write when the watcher ends; once the stream is over,
		// let its final status be written under the usual timeout.
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			select {
			case <-w.Done():
				sw.abort()
			case <-stop:
			}
		})
		defer func() {
			close(stop)
			wg.Wait()
			sw.resume()
		}()
	}
	if err := ws.catchUp(math.MaxInt64); err != nil {
		return ws.ended(err)
	}
	// Response headers go out with the first message: without a replayed
	// event, a keepalive opens the stream at once.
	if !ws.sent {
		if err := ws.keepalive(); err != nil {
			return ws.ended(err)
		}
	}
	ws.sent = false
	keepalive := time.NewTimer(s.keepalive)
	defer keepalive.Stop()
	recheck := time.NewTicker(s.recheck)
	defer recheck.Stop()
	lifetime := time.NewTimer(streamLifetime(s.maxLifetime))
	defer lifetime.Stop()
	for {
		select {
		case <-ctx.Done():
			return s.internal(ctx, "watch session", ctx.Err())
		case <-lifetime.C:
			return connect.NewError(connect.CodeUnavailable, errStreamExpiry)
		case <-recheck.C:
			ok, err := s.stillSignedIn(ctx, req.Header(), u.ID)
			switch {
			case err != nil && ctx.Err() != nil:
				return s.internal(ctx, "watch session", err)
			case err != nil:
				// A busy or failing store does not end the stream; the next
				// check decides.
				s.logger.WarnContext(ctx, "web session recheck failed", "error", err)
			case !ok:
				return connect.NewError(connect.CodeUnauthenticated, errSignedOut)
			}
		case <-keepalive.C:
			if err := ws.keepalive(); err != nil {
				return ws.ended(err)
			}
		case <-w.Ready():
			items, err := w.Take()
			if err != nil {
				return ws.ended(err)
			}
			if err := ws.deliver(items); err != nil {
				return ws.ended(err)
			}
		}
		// Any send restarts the silence; a timer that fired meanwhile is
		// drained by Reset's semantics (Go 1.23+ timers).
		if ws.sent {
			ws.sent = false
			keepalive.Reset(s.keepalive)
		}
	}
}

// stillSignedIn reports whether the request's session cookie still names a
// live browser session of userID. It mirrors the resolution of
// auth.WebService (UseWebSession, then the user), so a sign-out, an expiry
// or a deleted user ends the stream.
func (s *Service) stillSignedIn(ctx context.Context, header http.Header, userID string) (bool, error) {
	c, err := (&http.Request{Header: header}).Cookie(auth.SessionCookie)
	if err != nil || !auth.WellFormed(c.Value) {
		return false, nil
	}
	ws, err := s.st.UseWebSession(ctx, auth.HashToken(c.Value), s.now().UnixMilli())
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if ws.UserID != userID {
		return false, nil
	}
	u, err := s.st.GetUser(ctx, userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return u.Email != "", nil
}

// watchStream is one WatchSession's sending state.
type watchStream struct {
	s       *Service
	ctx     context.Context
	id      string
	stream  *connect.ServerStream[circusv1.SessionEvent]
	watcher *sessions.Watcher
	cursor  int64 // the highest durable seq sent, or after_seq
	sent    bool  // something was sent since the keepalive timer last restarted
}

// ended maps the error that ends a stream: the watcher's end (deletion,
// shutdown) wins over the write error it caused.
func (ws *watchStream) ended(err error) error {
	select {
	case <-ws.watcher.Done():
		_, werr := ws.watcher.Take()
		switch {
		case errors.Is(werr, sessions.ErrSessionDeleted):
			return connect.NewError(connect.CodeNotFound, errSessionNotFound)
		case errors.Is(werr, sessions.ErrBusClosed):
			return connect.NewError(connect.CodeUnavailable, errShuttingDown)
		}
	default:
	}
	if ws.ctx.Err() != nil {
		return ws.s.internal(ws.ctx, "watch session", ws.ctx.Err())
	}
	return err
}

func (ws *watchStream) send(ev *circusv1.SessionEvent) error {
	if err := ws.stream.Send(ev); err != nil {
		return err
	}
	ws.sent = true
	return nil
}

// keepalive sends an ephemeral event of kind KIND_UNSPECIFIED without a
// payload, which clients ignore.
func (ws *watchStream) keepalive() error {
	return ws.send(&circusv1.SessionEvent{Ephemeral: true, TsUnixMs: ws.s.now().UnixMilli()})
}

// catchUp sends the stored events after the cursor up to and including
// target, until it reaches target or the store has no more. Events above
// target wait for their own announcement, so ephemeral events queued
// before it go out first.
func (ws *watchStream) catchUp(target int64) error {
	for ws.cursor < target {
		limit := int64(watchPageEvents)
		if target-ws.cursor < limit {
			limit = target - ws.cursor
		}
		page, err := ws.s.st.ReadEvents(ws.ctx, store.EventFilter{SessionID: ws.id, AfterSeq: ws.cursor, Limit: int(limit), MaxBytes: watchPageBytes})
		if err != nil {
			return ws.s.sessionError(ws.ctx, "watch session", err)
		}
		if len(page.Events) == 0 {
			return nil
		}
		for _, e := range page.Events {
			if e.Seq <= ws.cursor {
				continue
			}
			if e.Seq > target {
				// The events up to target were evicted.
				ws.cursor = target
				return nil
			}
			if err := ws.send(protoEvent(e)); err != nil {
				return err
			}
			ws.cursor = e.Seq
		}
	}
	return nil
}

// deliver sends a batch of bus items in order.
func (ws *watchStream) deliver(items []sessions.Item) error {
	// Ephemeral events before a mark the stream has already passed (the
	// initial replay reads past marks queued meanwhile) were published
	// before durable events it has sent: they are stale.
	start := 0
	for i, it := range items {
		if it.Event == nil && int64(it.Seq) <= ws.cursor {
			start = i + 1
		}
	}
	for _, it := range items[start:] {
		if it.Event != nil {
			if err := ws.send(it.Event); err != nil {
				return err
			}
			continue
		}
		if err := ws.catchUp(int64(it.Seq)); err != nil {
			return err
		}
	}
	return nil
}
