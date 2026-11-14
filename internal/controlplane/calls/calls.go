// Package calls defines metadata-only routing with boot-fenced at-most-once
// dispatch. Request content and results exist only in bounded volatile memory.
//
// Composition: mount the gateway with this router as its Hooks, and on
// shutdown call gateway.Handler.Shutdown BEFORE Router.Close, so every link's
// final results drain and every disconnect is recorded while the router still
// runs. A router started after a crash (or after a reversed shutdown) treats
// devices whose rows still say connected as offline since its own start.
package calls

import (
	"context"
	"errors"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
)

var (
	// ErrOverflow closes only the slow subscription; Query can recover its state.
	ErrOverflow = errors.New("calls: subscription buffer exhausted")
	// ErrResultUnavailable means completed metadata exists but the daemon lost its result.
	ErrResultUnavailable = errors.New("calls: result unavailable")
	// ErrDeviceOffline is transient: the authoritative daemon cannot be queried now.
	ErrDeviceOffline = errors.New("calls: device offline")
	// ErrClosed means the router or subscription has closed.
	ErrClosed = errors.New("calls: closed")
	// ErrOverloaded refuses work before retaining it when an admission limit is full.
	ErrOverloaded = errors.New("calls: overloaded")
	// ErrStoreUnavailable identifies a persistence failure. Its worker protocol
	// representation is CODE_STORE_UNAVAILABLE only when no new attempt persisted.
	ErrStoreUnavailable = errors.New("calls: store unavailable")
)

// Service is the worker seam; userID is supplied by an authenticated caller.
// All methods enforce user ownership before revealing any call or device state;
// another user's call ID reports store.ErrNotFound. Operations are serialized
// by one router-wide admission lock (DESIGN.md, deviation 1), never held while
// waiting for a daemon or subscriber. No operation starts an unbounded goroutine.
type Service interface {
	// Submit validates IDs and wire-shaped size limits, then deduplicates by
	// global call ID, owner, device and SHA-256 of deterministic DeviceCall bytes.
	// It copies content only after budget admission. Equal retries subscribe to
	// existing state; mismatches produce Rejected{CALL_MISMATCH}. Only never-
	// dispatched lost calls can start a fresh accepted attempt. Rejections and
	// offline outcomes are CallUpdates; infrastructure failures return errors.
	// ctx owns the subscription lifetime; cancelling it never cancels execution.
	Submit(ctx context.Context, userID, deviceName string, call *circusv1.DeviceCall) (*Subscription, error)
	// Cancel cancels accepted calls durably, or records intent for dispatched,
	// running and unknown calls before ordered delivery. Completion wins races;
	// cancelling an existing terminal call is a no-op. No implicit job stop occurs.
	Cancel(ctx context.Context, userID, callID string) error
	// Query returns an immutable update; dispatched, running, unknown and
	// completed calls ask the dispatch boot's daemon with CallQuery (one per
	// call outstanding; concurrent queries coalesce). It observes ctx. Running
	// maps to Dispatched because CallUpdate has no Running variant. While the
	// device's route synchronizes, the query waits for the sync to answer it.
	// When the daemon cannot be asked, a nonterminal call reports its durable
	// state and a completed one ErrDeviceOffline (transient); completed
	// metadata whose result expired returns ErrResultUnavailable (permanent).
	Query(ctx context.Context, userID, callID string) (*circusv1.CallUpdate, error)
	// StopSessionJobs sends barriers to the user's devices with matching running
	// jobs. It stops background jobs only, never foreground calls or future jobs.
	// Return means command admission, not daemon completion; offline routes error.
	StopSessionJobs(ctx context.Context, userID, sessionID string) error
	// Healthy reports the in-memory store health bit without locking, store I/O
	// or Stats' durable timer walk, so readiness always stays within its context.
	// While false, the existing sweep probes one bounded row outside the router
	// lock and restores it on success, without requiring call traffic. Read
	// success does not prove writes work: readiness can flap (for example disk
	// full), and the next failed write clears the bit again.
	Healthy() bool
	// Stats reports retained resources and store health for readiness/invariants.
	Stats() Stats
	// Close stops admission, releases pending content and subscriptions, and
	// joins owned work within ctx. Durable nonterminal calls/timers survive close.
	Close(context.Context) error
}

// Subscription is a bounded per-call update stream. It retains at most
// MaxUpdatesPerSubscription entries and MaxSubscriptionBytes, also charged to
// MaxBufferedUpdateBytes globally. It stores no result after delivery to Next.
// Overflow or SubscriberStall drops queued updates, releases ownership and ends
// with ErrOverflow; it never blocks the daemon reader or cancels the call.
// The caller must Close or cancel Submit's ctx. Exactly one Next may be active.
type Subscription struct {
	owner       *router
	callID      string
	queue       []queuedUpdate // bounded by count, per-subscription bytes and global bytes
	bytes       int64
	changed     chan struct{}
	terminal    bool
	err         error
	lastRead    time.Time
	stopContext func() bool
	contextDone bool
}

// Next transfers one immutable update, releasing its buffer charge. Updates
// may be shared between subscribers: callers must not modify them.
// After a terminal update it returns io.EOF. Context cancellation only interrupts
// this read; subscription lifetime remains tied to Submit's ctx or Close.
func (s *Subscription) Next(ctx context.Context) (*circusv1.CallUpdate, error) {
	return s.next(ctx)
}

// Close idempotently detaches the subscriber and releases all queued updates.
func (s *Subscription) Close() error { return s.close() }

// Router combines the worker seam and gateway hooks without coupling gateway to
// this package. At most one current-generation sync and job sequence cursor per
// connected device are retained, bounded by Config.MaxDevices.
type Router interface {
	Service
	gateway.Hooks
	// HandleTimer implements timers.Handler for device_grace and call_watchdog.
	// It first CompleteTimer's the claim inside its own Tx; false abandons it.
	// The enrollment cleanup handler is supplied by server composition.
	HandleTimer(context.Context, store.Timer) error
}

// PendingBudget bounds unacknowledged request ownership, including accepted calls.
// New calls charge decoded memory and serialization scratch before marshal.
// Existing calls, including undispatched-Lost resubmits, first perform one
// transient deterministic marshal under the router lock (at most 2x message
// size, outside this budget) to prove equality. Equal duplicates attach without
// new pending ownership; Lost resubmits then pass new-attempt scratch admission.
// MaxCalls also bounds map entries and ordering records.
// Ack, result, rejection, offline, lost or cancellation releases content. Retry
// admission on wire.ErrBusy during Submit yields Rejected{OVERLOADED}. A known
// refusal of the first send can reject dispatched while preserving its boot;
// an uncertain write or failed resend preserves a durable recovery watchdog.
type PendingBudget struct {
	MaxBytes int64
	MaxCalls int
}

// Config supplies dependencies. Zero selects defaults: pending 256 MiB/4096
// calls, 256 devices, 128 operations, 1024 subscription/callback/query slots,
// 16 updates and 144 MiB per subscription, 256 MiB total buffered updates,
// 64 MiB messages, 30s grace, 15s subscriber stall, 30s watchdog slack, 2m
// delivery allowance, and 10m resend horizon. Negative values are refused.
// ResendHorizon is a replay policy and request-retention bound, not a safety
// bound: the daemon's dispatch-age check enforces at-most-once execution.
// No request/result cache exists outside pending and subscription buffers.
// Sender delivers a command to one daemon connection generation; the gateway
// implements it. Every call runs under the router's admission lock, so at most
// one delivery is in flight. Delivery success means queued, not written,
// acknowledged or executed. At-most-once dispatch does not depend on it: the
// dispatch transaction commits before the DispatchCall is handed to Deliver.
type Sender interface {
	Deliver(context.Context, gateway.Target, *circusv1.DaemonDownlink) error
}

// SenderFunc adapts a function to Sender. Assembly uses it to bind the
// gateway, which is built after the router because it calls the router's
// hooks.
type SenderFunc func(context.Context, gateway.Target, *circusv1.DaemonDownlink) error

// Deliver calls f.
func (f SenderFunc) Deliver(ctx context.Context, t gateway.Target, m *circusv1.DaemonDownlink) error {
	return f(ctx, t, m)
}

// MaxOperations bounds active public calls, hooks and timer work; each may
// borrow one wire-bounded message. Waiting queries release their operation slot
// and are bounded independently by MaxSubscriptions;
// MaxDevices bounds per-device sync state (cursor and outstanding requests).
// Retained IDs/names are copied and <=128 bytes. Messages obey wire byte, node,
// repeated-field and metadata bounds even when submitted without a wire link.
// Store walks use keyset pages of at most 64 rows; no full table is loaded.
type Config struct {
	Store                          store.Store
	Registry                       gateway.Registry
	Sender                         Sender
	Clock                          clock.Clock
	Pending                        PendingBudget
	MaxDevices, MaxOperations      int
	MaxSubscriptions               int
	MaxUpdatesPerSubscription      int
	MaxSubscriptionBytes           int64
	MaxBufferedUpdateBytes         int64
	MaxMessageBytes                int
	GracePeriod, SubscriberStall   time.Duration
	WatchdogSlack, DeliveryTimeout time.Duration
	// ResendHorizon bounds how long same-boot unacknowledged replay is preferred
	// over a query and how long its request content is retained. It is a policy
	// knob, not an at-most-once safety bound; zero means ten minutes.
	ResendHorizon time.Duration
	// MaxSyncRequests (1-256, zero means 16) bounds the reconnect sync's
	// unanswered CallQuery, resent DispatchCall and ListJobs requests per
	// device. The daemon closes its link when its send queue (256 messages or
	// 2M bytes by default) cannot take a reply, so an unbounded resync batch
	// would disconnect on every reconnect. A daemon that answers none within
	// DeliveryTimeout ends the sync and the link.
	MaxSyncRequests int
	// MaxSyncReplyBytes (zero means 2*MaxMessageBytes, at least
	// MaxMessageBytes) further bounds the same requests by their worst-case
	// replies: any reply can be a CallStatus or CallAck carrying a result of
	// up to MaxMessageBytes, and the daemon's send queue holds 2M bytes by
	// default (M = negotiated message limit). The effective bound is
	// min(MaxSyncRequests, MaxSyncReplyBytes/MaxMessageBytes), so with default
	// limits two sync replies are outstanding at once. Results pushed by the
	// daemon outside the sync share that queue; see DESIGN.md.
	MaxSyncReplyBytes int64
	// lockObserver, when set (by tests, through export_test.go), receives how
	// long each locked segment of an operation held the router-wide
	// admission lock. It is nil in production: the cost is one nil check per
	// lock and unlock.
	lockObserver func(kind string, held time.Duration)
}

// Stats is a constant-size snapshot of actual ownership. SyncRequests counts
// outstanding sync requests; UnrecordedDisconnects counts disconnects whose
// store write failed and awaits retry. PendingBytes includes
// pending scratch; BufferedBytes includes queued updates; QueryWaiters counts
// coalesced query consumers. DurableTimers counts router-owned durable timers,
// including claimed ones; StoreHealthy is false after persistence failure and
// may also be false solely because Stats' own timer walk failed. Healthy instead
// tracks writes and the sweep's bounded recovery probes, so the two can differ.
// All volatile gauges reach zero on Close. Durable timers disappear on outcome,
// not merely on process shutdown, and never point at terminal calls.
type Stats struct {
	PendingCalls, Subscribers, QueryWaiters int
	// Callbacks includes registered/running subscription context callbacks.
	Callbacks                           int
	Devices, Operations, DurableTimers  int
	SyncRequests, UnrecordedDisconnects int
	PendingBytes, BufferedBytes         int64
	StoreHealthy                        bool
}

// New validates configuration and returns an idle router. Startup reconciliation
// marks abandoned accepted calls lost before accepting traffic; dispatched calls
// retain their boot and recover by query. Lifecycle starts no hidden real clock.
// One bounded subscription-stall sweep is started; Close joins it.
func New(config Config) (Router, error) { return newRouter(config) }
