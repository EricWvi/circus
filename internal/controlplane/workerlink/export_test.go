package workerlink

import (
	"context"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/calls"
)

// Subscription is the push side of a router subscription.
type Subscription = subscription

// SetSubmit replaces Router.Submit for the Submit path.
func SetSubmit(h *Handler, f func(ctx context.Context, user, device string, call *circusv1.DeviceCall) (Subscription, error)) {
	h.submit = func(ctx context.Context, user, device string, call *circusv1.DeviceCall, _ calls.Admission) (Subscription, error) {
		return f(ctx, user, device, call)
	}
}

// SetQuery replaces Router.QueryOnce for Query and Cancel answers.
func SetQuery(h *Handler, f func(ctx context.Context, user, id string) (Subscription, error)) {
	h.query = f
}

// SetBeforeSend runs f before each update is handed to the link.
func SetBeforeSend(h *Handler, f func(context.Context, *circusv1.CallUpdate)) { h.beforeSend = f }

// SetAfterSend runs f after the link accepted an update.
func SetAfterSend(h *Handler, f func(context.Context, *circusv1.CallUpdate)) { h.afterSend = f }

// Fixed limits.
const (
	PushPermits      = pushPermits
	SmallUpdateBytes = smallUpdateBytes
	MaxPerCredential = maxPerCredential
	MaxOperations    = maxOperations
)

// SetSessionSend runs f before each SessionReply send attempt; a non-nil
// error replaces the attempt (wire.ErrBusy simulates a full send pool).
func SetSessionSend(h *Handler, f func(m *circusv1.WorkerDownlink) error) { h.sessionSend = f }

// SetMaxInlineReplyBytes lowers the bound of each connection's queue of
// inline session replies; call it before connections are served.
func SetMaxInlineReplyBytes(h *Handler, n int) { h.maxInline = n }

// HoldPushPermits takes every push permit until the returned release.
func HoldPushPermits(h *Handler) (release func()) {
	for range pushPermits {
		h.push <- struct{}{}
	}
	return func() {
		for range pushPermits {
			h.push.release()
		}
	}
}

// HoldPushPermit takes one push permit, waiting for it, until the returned
// release.
func HoldPushPermit(h *Handler) (release func()) {
	h.push <- struct{}{}
	return h.push.release
}

// PushPermitsInUse reports how many push permits are held.
func PushPermitsInUse(h *Handler) int { return len(h.push) }

// SetClock replaces the handler's clock (handshake, rechecks, send retries
// and their deadlines); call it before connections are served.
func SetClock(h *Handler, clk clock.Clock) { h.cfg.Clock = clk }

// Peers reports how many connections h holds.
func Peers(h *Handler) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.peers)
}

// SetAfterWelcome sets a hook that runs after each Welcome was sent and
// before the connection is registered with the session service.
func SetAfterWelcome(h *Handler, f func()) { h.afterWelcome = f }
