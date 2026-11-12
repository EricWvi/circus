// Package gateway defines authenticated daemon links and their registry.
// The HTTP host supplies TLS (or explicitly allowed loopback development HTTP).
package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/wire"
)

var (
	// ErrUnauthenticated maps to HTTP 401 before WebSocket upgrade.
	ErrUnauthenticated = errors.New("gateway: unauthenticated")
	// ErrRevoked maps to HTTP 403 and closes an existing authenticated link.
	ErrRevoked = errors.New("gateway: credential revoked")
)

// Principal is the gateway-owned daemon identity, containing no bearer token.
// All strings are nonempty, copied on retention and at most 128 bytes. T6 adapts
// the auth module to this type; a worker credential must never produce it.
type Principal struct {
	UserID, DeviceID, DeviceName, CredentialID string
}

// Authenticator is the consumer seam implemented by T6's auth adapter.
type Authenticator interface {
	// Authenticate checks the upgrade request, including its daemon role.
	// Both methods must promptly honor context cancellation.
	Authenticate(context.Context, *http.Request) (Principal, error)
	// Recheck validates that this exact credential is still usable. Polling
	// closes live links within RecheckInterval plus RecheckTimeout (F17).
	// Any error closes the connection; no plaintext credential is retained.
	Recheck(context.Context, Principal) error
}

// Connection identifies a registry generation. Target fences superseded links;
// UserID and DeviceName are copied strings of at most 128 bytes each. No socket
// or request content is stored in a registry entry; InstanceID permits routing.
type Connection struct {
	Target             bus.Target
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

// Handler serves /v1/link/daemon and owns local sockets and one bus binding.
type Handler interface {
	http.Handler
	// Shutdown stops admission, sends GoAway, calls Link.Shutdown(ctx), drains
	// received messages and joins owned work. A deadline bounds graceful close.
	Shutdown(context.Context) error
	// Stats counts actual ownership; shutdown must release it rather than reset it.
	Stats() Stats
}

// Config supplies dependencies and hard bounds. Zero selects 100 connections,
// a 10s Hello timeout, 1s credential recheck interval, 5s recheck timeout,
// version 1 and pools twice wire.MinBudget. Negative values are refused;
// MinVersion/MaxVersion form an inclusive supported range (currently 1 only).
// MaxConnections (1-256) includes handshakes, current and superseded sockets,
// so replacement cannot accumulate closing links. At capacity, refuse admission.
// One Receive value per socket may be borrowed by a hook; no extra inbox exists.
// LinkOptions.Clock is set from Clock; LinkOptions.Budget must be nil: New creates
// ONE wire.NewBudget(SendPoolBytes, ReceivePoolBytes) for every owned link.
// Each pool must be >= twice its wire.MinBudget(LinkOptions) requirement.
type Config struct {
	InstanceID                      string
	Clock                           clock.Clock
	Auth                            Authenticator
	Registry                        Registry
	Bus                             bus.Bus
	Hooks                           Hooks
	MinVersion, MaxVersion          uint32
	MaxConnections                  int
	HelloTimeout                    time.Duration
	RecheckInterval, RecheckTimeout time.Duration
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

// New validates bounds and binds Config.InstanceID on Bus. ServeHTTP authenticates
// before wire.AcceptWebSocket, exchanges raw Hello and Welcome/Reject, fills
// Welcome via wire.FillWelcome, then constructs wire.New(DaemonControlSide).
// Every Link.Send uses wirepolicy.Classify; normal close uses Link.Shutdown.
// ErrBudgetExhausted/ErrConsumerStalled close the link and trigger reconnect sync.
func New(config Config) (Handler, error) { return newHandler(config) }
