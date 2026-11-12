// Package timers defines the lease-based runner for durable control-plane timers.
// Domain writes and SetTimer/CancelTimer belong to the same store transaction.
package timers

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/store"
)

const (
	// DeviceGrace guards a disconnected device; its subject is the device ID.
	DeviceGrace = "device_grace"
	// CallWatchdog guards a dispatched call; its subject is the call ID.
	CallWatchdog = "call_watchdog"
	// EnrollTokenExpiry triggers metadata-only enrollment token cleanup.
	EnrollTokenExpiry = "enroll_token_expiry"
)

// Handler handles one claim synchronously. Its first operation is a store.Tx
// whose first domain call is CompleteTimer(ctx, claim). On false it does nothing.
// On true, effects and replacement timers commit in that same Tx. Errors roll
// back completion so the claim can expire and be retried. Network writes and
// notifications happen after commit; failure must leave a durable recovery path.
// It must be idempotent, honor ctx and never retain the claim after return.
type Handler func(ctx context.Context, claim store.Timer) error

// Runner has one claim loop and one synchronous handler, with no per-timer
// goroutine or in-memory deadline map. Only one Run may be active.
type Runner interface {
	// Run claims due timers until ctx ends, then joins the active handler.
	// Handler errors leave claims for lease expiry; store failures are reported
	// through Stats and retried on the next poll. Cancellation returns ctx.Err().
	Run(context.Context) error
	// Wake coalesces hints in a single slot; polling guarantees eventual work.
	Wake()
	// Stats reports real claims still owned by this runner, including retries.
	Stats() Stats
}

// Config supplies dependencies and finite admission. InstanceID is 1-128 bytes.
// BatchSize is 1-256; at most BatchSize claims (kind <= 128 bytes, subject <= 128
// bytes, token <= 128 bytes each) and one wake hint are retained. Lease and
// PollInterval must be positive; Handler must handle all three timer kinds.
// The handler supplies its own bounded transient work; store reads are keyset
// pages of at most store.MaxPageSize rows (calls/DESIGN.md).
type Config struct {
	Store               store.Store
	Clock               clock.Clock
	InstanceID          string
	Lease, PollInterval time.Duration
	BatchSize           int
	Handler             Handler
}

// Stats is a constant-size snapshot. Claims includes leased rows not yet
// completed or expired; Active is at most one. Healthy reflects the last poll.
type Stats struct {
	Running, Healthy bool
	Claims, Active   int
}

// New validates configuration without starting work; all time uses Config.Clock.
// Persistent timer count is not an in-memory queue and is not loaded at startup.
func New(config Config) (Runner, error) {
	if config.Store == nil || config.Clock == nil || config.Handler == nil || config.InstanceID == "" || len(config.InstanceID) > 128 || config.BatchSize < 1 || config.BatchSize > store.MaxPageSize || config.Lease < time.Millisecond || config.PollInterval <= 0 {
		return nil, fmt.Errorf("timers: invalid configuration")
	}
	config.InstanceID = strings.Clone(config.InstanceID)
	return &runner{config: config, wake: make(chan struct{}, 1)}, nil
}

// runner retains only a BatchSize claim slice and one coalesced wake signal.
type runner struct {
	mu     sync.Mutex
	config Config
	wake   chan struct{}
	stats  Stats
}

func (r *runner) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}
func (r *runner) Stats() Stats { r.mu.Lock(); defer r.mu.Unlock(); return r.stats }
func (r *runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.stats.Running {
		r.mu.Unlock()
		return fmt.Errorf("timers: already running")
	}
	r.stats.Running = true
	r.mu.Unlock()
	defer func() { r.mu.Lock(); r.stats.Running = false; r.stats.Active = 0; r.mu.Unlock() }()
	held := make([]store.Timer, 0, r.config.BatchSize)
	poll := r.config.Clock.After(r.config.PollInterval)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		now := r.config.Clock.Now().UnixMilli()
		live := held[:0]
		healthy := true
		drained := false
		for _, t := range held {
			if t.ClaimUntil <= now {
				continue
			}
			rows, err := r.config.Store.ListTimers(ctx, store.TimerFilter{Kind: t.Kind, SubjectID: t.SubjectID, Limit: 1})
			if err != nil {
				healthy = false
				live = append(live, t)
			} else if len(rows) > 0 && rows[0].Token == t.Token {
				live = append(live, t)
			}
		}
		held = live
		if room := r.config.BatchSize - len(held); room > 0 {
			batch, err := r.config.Store.ClaimDueTimers(ctx, now, r.config.InstanceID, r.config.Lease.Milliseconds(), room)
			if err != nil {
				healthy = false
			} else {
				r.mu.Lock()
				drained = len(batch) > 0
				r.stats.Claims = len(held) + len(batch)
				r.mu.Unlock()
				for _, t := range batch {
					r.mu.Lock()
					r.stats.Active = 1
					r.mu.Unlock()
					err := r.config.Handler(ctx, t)
					r.mu.Lock()
					r.stats.Active = 0
					r.mu.Unlock()
					if err != nil {
						healthy = false
						held = append(held, t)
					}
				}
			}
		}
		r.mu.Lock()
		r.stats.Claims = len(held)
		r.stats.Healthy = healthy
		r.mu.Unlock()
		if drained {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.wake:
		case <-poll:
			poll = r.config.Clock.After(r.config.PollInterval)
		}
	}
}
