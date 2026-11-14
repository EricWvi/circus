// Package gateway defines authenticated daemon links and their registry.
// The HTTP host supplies TLS (or explicitly allowed loopback development HTTP).
//
// All daemon links share ONE wire.Budget (SendPoolBytes/ReceivePoolBytes;
// zero default: twice wire.MinBudget). Pool sizing rule (D43 item 18):
// with (Smin, Rmin) = wire.MinBudget(LinkOptions) (about 262 MiB receive
// with the default 64 MiB message limit), pools below 2*Smin / 2*Rmin are
// rejected (PoolError). Let K be the number of devices that may stream
// large (8–64 MiB) results at once; size the receive pool at
// max(2*Rmin, K*Rmin) plus headroom: large links measured about 250–300 MiB
// each, and the conservative K*462 MiB (one link's peak receive phase)
// showed no starvation at 4/16/64 devices (TestMeasureReceivePoolShare,
// opt-in CIRCUS_MEASURE=1). ErrBudgetExhausted or forced reconnects on
// large-result links mean K exceeds what the pool was sized for. There is
// no per-link share yet: a few large transfers can delay or starve other
// devices' results when K is not bounded; a per-link cap (Rmin) plus a
// small-message reserve is planned before multi-user (M4). See
// calls/DESIGN.md "Capacity planning (Q33)".
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/wire"
)

// ErrNoRoute means the target connection generation is not current.
var ErrNoRoute = errors.New("gateway: route unavailable")

// Target fences delivery to one connection and boot on one control-plane
// instance. Each field is nonempty and at most 128 bytes.
type Target struct {
	InstanceID, ConnectionID, DeviceID, BootID string
}

// PoolError reports send or receive pools below twice wire.MinBudget. It
// carries the configured and smallest accepted values so the host can name
// its flags and the minimum (see the sizing rule in this package's doc).
type PoolError struct {
	Send, Receive       int
	MinSend, MinReceive int
}

func (e *PoolError) Error() string {
	return fmt.Sprintf("gateway: send pool %d and receive pool %d below the minimum %d and %d (twice wire.MinBudget)",
		e.Send, e.Receive, e.MinSend, e.MinReceive)
}

// Connection identifies a registry generation. Target fences superseded links;
// UserID and DeviceName are copied strings of at most 128 bytes each. No socket
// or request content is stored in a registry entry; InstanceID permits routing.
type Connection struct {
	Target             Target
	UserID, DeviceName string
	Ready              bool // false until reconnect synchronization is enqueued
}

// Registry locates daemon links. Methods are concurrency-safe and replacements
// are atomic. It owns no sockets; the gateway owns their shutdown and draining.
type Registry interface {
	// Register replaces the device's route and returns the previous generation.
	// Capacity refusal leaves the previous route intact.
	Register(context.Context, Connection) (previous *Connection, err error)
	// Activate makes the exact generation ready for fresh dispatch. Register
	// always starts unready; only OnConnected activates after sync admission.
	Activate(context.Context, Connection) (activated bool, err error)
	// Unregister removes only an exact generation; a stale close is a no-op.
	Unregister(context.Context, Connection) (removed bool, err error)
	// Lookup returns an immutable route, if the device has a current connection.
	Lookup(ctx context.Context, deviceID string) (Connection, bool, error)
	// ListOnline returns only this user's routes, sorted by device name.
	// The slice has at most the registry's configured connection limit.
	ListOnline(ctx context.Context, userID string) ([]Connection, error)
	// Stats counts real entries, including during replacement and shutdown.
	Stats() RegistryStats
}

// RegistryStats is a constant-size snapshot of the bounded registry.
type RegistryStats struct{ Connections int }

// NewRegistry constructs the process-local registry with 1-256 entries. Each
// entry retains at most six 128-byte strings plus fixed map/record overhead.
func NewRegistry(maxConnections int) (Registry, error) {
	if maxConnections < 1 || maxConnections > 256 {
		return nil, fmt.Errorf("gateway: invalid registry limit")
	}
	return &registry{limit: maxConnections, entries: make(map[string]Connection)}, nil
}

// Hooks is the gateway's consumer seam, implemented by the call router. Hooks
// borrow inbound messages until return and must bound any retained ownership.
// Every hook carries the connection generation. Call replies (ack, result,
// status) are authoritative for the user, device and dispatch boot they name,
// even on a superseded generation; job events and sync state follow the
// current generation only. Inbound hooks remain runnable while OnConnected
// synchronizes. A new generation's OnConnected starts only after the
// superseded generation of the same device has drained and disconnected.
type Hooks interface {
	// OnConnected runs after Welcome and route registration, before new dispatch.
	// The router owns SetConnected, boot comparison and durable reconciliation.
	OnConnected(context.Context, Connection) error
	// OnDisconnected runs after draining Receive, for every generation. For the
	// current one it withdraws the route and owns SetDisconnected and the
	// durable grace timer; for a superseded one it only releases its state.
	OnDisconnected(context.Context, Connection) error
	// OnCallAck accepts status or rejection, including result-before-ack races.
	OnCallAck(context.Context, Connection, *circusv1.CallAck) error
	// OnDeviceResult delivers the authoritative result, possibly before its ack.
	OnDeviceResult(context.Context, Connection, *circusv1.DeviceResult) error
	// OnCallStatus delivers a query response fenced by device and dispatch boot.
	OnCallStatus(context.Context, Connection, *circusv1.CallStatus) error
	// OnJobEvent applies an ordered boot-wide event, independently of call results.
	OnJobEvent(context.Context, Connection, *circusv1.JobEvent) error
	// OnJobSnapshot reconciles a bounded snapshot without regressing newer events.
	OnJobSnapshot(context.Context, Connection, *circusv1.JobSnapshot) error
}

// Handler serves /v1/link/daemon and owns local sockets.
type Handler interface {
	http.Handler
	// Deliver sends m to the exact connection generation t. It returns
	// ErrNoRoute when t is not the device's current, live generation, and
	// preserves wire.Link.Send errors such as wire.ErrBusy. Success means
	// queued, not written, acknowledged or executed. It never blocks on I/O.
	Deliver(ctx context.Context, t Target, m *circusv1.DaemonDownlink) error
	// Shutdown stops admission, sends GoAway, calls Link.Shutdown(ctx), drains
	// received messages and joins owned work. A deadline bounds graceful close.
	// The join-before-return guarantee can outlast ctx after forced abort; this
	// preserves the requirement that Router.Close runs only after disconnects.
	Shutdown(context.Context) error
	// Stats counts actual ownership; shutdown must release it rather than reset it.
	Stats() Stats
}

// Config supplies dependencies and hard bounds. Zero selects 100 connections,
// the auth.RecheckPolicy defaults and pools twice wire.MinBudget. Negative
// values are refused. Auth is the host's single authenticator: only a daemon
// (device) credential is admitted, and Recheck revalidates it on Clock.
// MaxConnections (1-256) includes handshakes, current and superseded sockets,
// so replacement cannot accumulate closing links. At capacity, refuse admission.
// One Receive value per socket may be borrowed by a hook; no extra inbox exists.
// LinkOptions.Clock is set from Clock; LinkOptions.Budget must be nil: New creates
// ONE wire.NewBudget(SendPoolBytes, ReceivePoolBytes) for every owned link.
// Each pool must be >= twice its wire.MinBudget(LinkOptions) requirement.
type Config struct {
	InstanceID                      string
	Clock                           clock.Clock
	Auth                            auth.LinkAuthenticator
	Registry                        Registry
	Hooks                           Hooks
	Logger                          *slog.Logger
	MaxConnections                  int
	Recheck                         auth.RecheckPolicy
	LinkOptions                     wire.Options
	SendPoolBytes, ReceivePoolBytes int
}

// Stats is a constant-size snapshot. Connections includes closing sockets;
// Registered counts current routes. Wire aggregates live link gauges, Budget
// includes pool reservations still held while graceful Receive drains.
type Stats struct {
	Connections, Handshakes, Registered int
	Wire                                wire.Stats
	Budget                              wire.BudgetStats
}

// New validates bounds. ServeHTTP authenticates (auth.Admit) before
// wire.AcceptWebSocket, answers the raw Hello with wire.AcceptHello, then
// constructs wire.New(DaemonControlSide). Normal close uses Link.Shutdown.
// ErrBudgetExhausted/ErrConsumerStalled close the link and trigger reconnect sync.
func New(config Config) (Handler, error) { return newHandler(config) }
