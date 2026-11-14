package workerlink

import (
	"context"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

// Subscription is the push side of a router subscription.
type Subscription = subscription

// SetSubmit replaces Router.Submit for the Submit path.
func SetSubmit(h *Handler, f func(ctx context.Context, user, device string, call *circusv1.DeviceCall) (Subscription, error)) {
	h.submit = f
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
