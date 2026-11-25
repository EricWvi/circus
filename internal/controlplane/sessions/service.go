package sessions

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/providers"
	"github.com/EricWvi/circus/internal/store"
	"github.com/google/uuid"
)

// Timing and bounds. See the package comment for how they combine.
const (
	// LeaseLifetime is how long a lease lives after its last acquisition,
	// resumption or extension.
	LeaseLifetime = 60 * time.Second
	// ExtendInterval is how often every lease bound to a live connection is
	// extended to now + LeaseLifetime.
	ExtendInterval = 20 * time.Second
	// SweepInterval is how often expired leases are released.
	SweepInterval = 5 * time.Second
	// ScheduleInterval is the session_schedule fallback: how often queued
	// sessions of attached users are offered again, and how long an Offer
	// stays reserved for the connection it went to.
	ScheduleInterval = 30 * time.Second
	// StoreTimeout bounds every store transaction of a session operation;
	// past it the operation answers STORE_UNAVAILABLE.
	StoreTimeout = 5 * time.Second
	// ReadConcurrency is the number of read operations (state, transcript
	// pages, lease replies, device lists) built at once across all
	// connections.
	ReadConcurrency = 2
	// MaxTracked bounds the in-memory session table: sessions with a lease
	// bound to a connection this process knows, or an outstanding Offer.
	MaxTracked = 8192
	// ReasonRecheck is how long a queue reason no event re-derived stays
	// before the fallback evaluates its session again; MaxReasonRechecks
	// bounds those evaluations per fallback pass.
	ReasonRecheck     = 5 * time.Minute
	MaxReasonRechecks = 256
	// goneRecheck is how long after its holder disconnected a lease still
	// marked disconnected is evaluated again by the fallback (also at most
	// MaxReasonRechecks per pass): by then the sweep has released it unless
	// it ended unseen.
	goneRecheck = LeaseLifetime + ScheduleInterval
	// MaxStaleRetries bounds how many times in a row an evaluation whose
	// store read turned out stale is repeated at once; past it the session
	// is evaluated at the next fallback pass.
	MaxStaleRetries = 3
	// MaxWakeQueue bounds the sessions waiting for the scheduler; past it a
	// Wake of an untracked session is dropped and the next fallback scan
	// finds the session queued in the store.
	MaxWakeQueue = 4096
	// ApprovalLifetime is how long a pending approval and an unused grant
	// live.
	ApprovalLifetime = 10 * time.Minute
	// maxAttachString bounds every Attach string field.
	maxAttachString = 256
	// MaxSessionsPerConnection is the largest Attach.max_sessions.
	MaxSessionsPerConnection = 64
	// replyMessageBytes bounds Rejected.message.
	replyMessageBytes = 1024
)

// Events is the part of the event bus the worker link publishes to: *Bus
// implements it. Both methods return at once and never fail.
type Events interface {
	Durable(sessionID string, seq uint64)
	Ephemeral(sessionID string, ev *circusv1.SessionEvent)
}

// Config supplies the service's dependencies.
type Config struct {
	Store store.Store
	// Bus receives committed durable sequence numbers and live events.
	Bus Events
	// InstanceID prefixes connection ids, so an id never repeats across
	// control-plane starts (together with a per-start random nonce).
	InstanceID string
	// Clock (nil: real time) supplies every time the service stores and
	// times its upkeep with.
	Clock  clock.Clock
	Logger *slog.Logger
	// StopJobs stops a session's device jobs after the transcript quota
	// stopped the session (calls.Service.StopSessionJobs). Optional.
	StopJobs func(ctx context.Context, userID, sessionID string) error
	// Online reports which of the user's devices have a live daemon link,
	// for ListDevices. Optional: nil reports every device offline.
	Online func(ctx context.Context, userID string) (map[string]bool, error)
	// MaxMessageBytes is the link's message limit M; zero means 64 MiB. A
	// single transcript entry may be at most M - 64 KiB, so it fits a page
	// reply with its envelope.
	MaxMessageBytes int
	// MaxSessionTranscriptBytes and MaxUserTranscriptBytes are the
	// transcript quotas; zero means the store defaults.
	MaxSessionTranscriptBytes, MaxUserTranscriptBytes int64
	// MaxViewBytes bounds a session's stored event payloads; zero means
	// store.DefaultMaxViewBytes.
	MaxViewBytes int64
	// Secrets opens providers' sealed secrets, for GetLaunchEnv and to
	// tell whether a provider's secrets are readable. Nil: no provider's
	// secrets are.
	Secrets *providers.Box
	// LedgerDue is told of a session whose ledger snapshot a turn's end
	// made due for a collection, on the report's path: it may read and
	// write the store but must not wait for other work. Optional.
	LedgerDue func(ctx context.Context, sessionID string)
}

// Service is the worker-link side of sessions: it serves session operations
// for every worker connection, keeps leases bound to live connections
// extended, releases expired ones, and schedules sessions with pending work
// onto attached workers. It implements Waker. Create it with New, call Start
// once before serving connections, and Close at shutdown.
type Service struct {
	cfg   Config
	clk   clock.Clock
	log   *slog.Logger
	quota store.TranscriptQuota
	nonce string
	seq   atomic.Uint64
	// rounds counts upkeep rounds, each ending with the next alarm armed.
	rounds atomic.Uint64
	reads  chan struct{}
	// jobStops bounds the concurrent StopJobs calls after a transcript
	// quota stop (maxJobStopCalls); jobs joins them at Close.
	jobStops chan struct{}
	jobs     sync.WaitGroup

	mu sync.Mutex
	// pend holds the log records of the current s.mu critical section,
	// written by unlock once s.mu is released (log.go).
	pend []logRec
	// dbg is whether the debug records collected under s.mu are wanted:
	// the logger's level as of New (the server's level is fixed at start),
	// so no handler method runs under s.mu.
	dbg     bool
	conns   map[string]*Conn
	entries map[string]*entry
	wakes   map[string]reasonSet
	rescan  map[string]struct{} // users whose queued sessions to offer
	gen     uint64              // Wake generation, see entry.gen
	kick    chan struct{}
	// provScans are providers (→ their user) whose queued sessions to
	// evaluate (ProviderChanged), at most MaxWakeQueue.
	provScans map[string]string
	// provEpoch counts provider leases that ended and provider changes, so
	// an evaluation can tell that its provider read may be stale.
	provEpoch uint64

	// boots is the current boot of each worker that has connected since
	// start (one entry per worker identity, kept after its connections
	// close): the boot whose boot transaction committed last, by bootSeq,
	// which each connection takes inside that transaction. Connections of
	// any other boot are stale and get no Offers.
	boots   map[string]bootRec
	bootSeq uint64
	// busy counts Wakes and rescans taken off the queues and not yet
	// processed, so Idle-style checks see in-progress work.
	busy int
	// noFallback disables the periodic rescan, noUpkeep every upkeep round
	// (tests).
	noFallback, noUpkeep bool
	// startedAt is when Start ran (Unix ms): every boot counts as alive for
	// LeaseLifetime after it, since a crash records no disconnect.
	startedAt int64
	// tokens numbers acquisitions (entry.token).
	tokens uint64
	// leaseGen counts bindings of leases to live connections and unbinds
	// (entry.changedAt); losses counts applied lease losses (lost). Both
	// let an evaluation tell that its store read is older than them.
	leaseGen, losses uint64
	// staleReads counts each session's stale reads in a row (retryStale);
	// parked holds sessions that reached MaxStaleRetries, with their
	// reasons, until the next fallback pass.
	staleReads map[string]int
	parked     map[string]reasonSet
	// maxTracked is MaxTracked (lowered by tests).
	maxTracked int

	started, closed bool
	stop            context.CancelFunc
	loops           sync.WaitGroup
}

var _ Waker = (*Service)(nil)

// New validates cfg. It starts nothing.
func New(cfg Config) (*Service, error) {
	if cfg.Store == nil || cfg.Bus == nil || cfg.InstanceID == "" || len(cfg.InstanceID) > 64 || cfg.MaxMessageBytes < 0 ||
		cfg.MaxSessionTranscriptBytes < 0 || cfg.MaxUserTranscriptBytes < 0 || cfg.MaxViewBytes < 0 {
		return nil, errors.New("sessions: invalid configuration")
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.Real{}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.MaxMessageBytes == 0 {
		cfg.MaxMessageBytes = 64 << 20
	}
	if cfg.MaxViewBytes == 0 {
		cfg.MaxViewBytes = store.DefaultMaxViewBytes
	}
	q := store.TranscriptQuota{
		MaxEntryBytes:   int64(min(cfg.MaxMessageBytes-64<<10, store.MaxTranscriptEntryBytes)),
		MaxSessionBytes: store.DefaultMaxTranscriptBytes,
		MaxUserBytes:    store.DefaultMaxUserTranscriptBytes,
	}
	if q.MaxEntryBytes < 1 {
		return nil, errors.New("sessions: message limit too small")
	}
	if cfg.MaxSessionTranscriptBytes > 0 {
		q.MaxSessionBytes = cfg.MaxSessionTranscriptBytes
	}
	if cfg.MaxUserTranscriptBytes > 0 {
		q.MaxUserBytes = cfg.MaxUserTranscriptBytes
	}
	return &Service{
		cfg: cfg, clk: cfg.Clock, log: cfg.Logger, quota: q,
		dbg:        cfg.Logger.Enabled(context.Background(), slog.LevelDebug),
		nonce:      uuid.NewString()[:8],
		reads:      make(chan struct{}, ReadConcurrency),
		jobStops:   make(chan struct{}, maxJobStopCalls),
		conns:      make(map[string]*Conn),
		entries:    make(map[string]*entry),
		wakes:      make(map[string]reasonSet),
		rescan:     make(map[string]struct{}),
		provScans:  make(map[string]string),
		staleReads: make(map[string]int),
		parked:     make(map[string]reasonSet),
		boots:      make(map[string]bootRec),
		maxTracked: MaxTracked,
		kick:       make(chan struct{}, 1),
	}, nil
}

// Start extends every held lease by one lifetime (so no lease expires
// sooner than LeaseLifetime after a control-plane start, whatever the
// outage) and starts the scheduler and lease upkeep. Call it once, before
// any connection is served.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started || s.closed {
		s.unlock()
		return errors.New("sessions: already started")
	}
	s.started = true
	s.startedAt = s.nowMs()
	s.unlock()
	at := s.nowMs()
	n := 0
	var cursor *store.RestartCursor
	for {
		tctx, cancel := context.WithTimeout(ctx, StoreTimeout)
		page, next, err := s.cfg.Store.ExtendLeasesAfterRestart(tctx, at, at+LeaseLifetime.Milliseconds(), cursor, store.MaxPageSize)
		cancel()
		if err != nil {
			return fmt.Errorf("sessions: extend leases after restart: %w", err)
		}
		n += page
		if next == nil {
			break
		}
		cursor = next
	}
	if n > 0 {
		s.log.InfoContext(ctx, "extended session leases after start", "count", n)
	}
	bg, stop := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	s.stop = stop
	s.unlock()
	now := s.clk.Now()
	alarm := clock.Until(s.clk, now.Add(SweepInterval))
	s.loops.Add(2)
	go s.schedule(bg)
	go s.upkeep(bg, now, alarm)
	return nil
}

// Close stops the scheduler and upkeep and waits for them until ctx ends.
// Leases stay in the store; a later start extends them.
func (s *Service) Close(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	stop := s.stop
	s.unlock()
	if stop == nil {
		return nil
	}
	stop()
	joined := make(chan struct{})
	go func() { s.loops.Wait(); s.jobs.Wait(); close(joined) }()
	select {
	case <-joined:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) nowMs() int64 { return s.clk.Now().UnixMilli() }

// storeCtx bounds one store transaction.
func storeCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, StoreTimeout)
}

// Read runs fn while holding one of the ReadConcurrency read permits,
// waiting for one until ctx ends. The worker link builds and sends every
// read reply inside fn, so at most ReadConcurrency read replies exist at
// once across all connections.
func (s *Service) Read(ctx context.Context, fn func()) bool {
	select {
	case s.reads <- struct{}{}:
	case <-ctx.Done():
		return false
	}
	defer func() { <-s.reads }()
	fn()
	return true
}

// ConnInfo identifies one authenticated worker connection.
type ConnInfo struct {
	WorkerID, UserID string
	// BootID is the worker process's Hello.boot_id; empty for a client
	// without one (the call CLI), which may submit calls without a session
	// but cannot attach or drive sessions.
	BootID string
	// Fail closes the link; the service calls it when the link broke a
	// rule it detects asynchronously or a hint cannot be sent.
	Fail func(reason string)
}

// bootRec is a worker's current boot and the sequence of the boot
// transaction that made it current.
type bootRec struct {
	boot string
	seq  uint64
}

// ErrBoot refuses a worker Hello: a boot id that is not a UUIDv7, or one
// older than the worker's current boot while that boot is alive (a replaced
// process still reconnecting).
var ErrBoot = errors.New("sessions: boot id refused")

// parseBoot parses a worker boot id: a UUIDv7 in canonical lowercase form.
func parseBoot(id string) (uuid.UUID, bool) {
	u, err := uuid.Parse(id)
	return u, err == nil && u.Version() == 7 && u.Variant() == uuid.RFC4122 && u.String() == strings.ToLower(id)
}

// older reports whether boot is older than current: by the UUIDv7
// timestamp, then by the remaining bytes (a UUID's byte order is exactly
// that). A current boot that is not a UUIDv7 (recorded before boot ids were
// checked) is older than any.
func older(boot uuid.UUID, current string) bool {
	cur, ok := parseBoot(current)
	return ok && bytes.Compare(boot[:], cur[:]) < 0
}

// CheckBoot tells whether a worker's Hello may be welcomed: an empty boot
// id (a client that only submits calls), or a UUIDv7 not older than the
// worker's current boot. It only reads; Connect checks again atomically.
// A refusal matches ErrBoot.
func (s *Service) CheckBoot(ctx context.Context, workerID, bootID string) error {
	if bootID == "" {
		return nil
	}
	b, ok := parseBoot(bootID)
	if !ok {
		return fmt.Errorf("%w: not a UUIDv7", ErrBoot)
	}
	tctx, cancel := storeCtx(ctx)
	defer cancel()
	return s.checkOrder(tctx, s.cfg.Store, workerID, b)
}

// checkOrder refuses boot b with ErrBoot when it is older than the
// worker's current boot and that boot is alive: it has a connection here,
// a lease that has not expired, or connected or disconnected within
// LeaseLifetime (a worker reconnects with backoff), and every boot for
// LeaseLifetime after this process started (a crash records no
// disconnect, and the worker may not have reconnected yet). Boots
// are ordered by their UUIDv7 timestamps, which come from the workers'
// clocks; once the current boot is gone an older-looking boot is accepted,
// so a skewed clock cannot lock a worker out.
func (s *Service) checkOrder(ctx context.Context, ops store.Ops, workerID string, b uuid.UUID) error {
	ident, err := ops.GetIdentity(ctx, store.KindWorker, workerID)
	if err != nil {
		return err
	}
	cur := ident.CurrentBootID
	if !older(b, cur) {
		return nil
	}
	now := s.nowMs()
	s.mu.Lock()
	startedAt := s.startedAt
	connected := false
	for _, c := range s.conns {
		if c.workerID == workerID && c.bootID == cur {
			connected = true
		}
	}
	s.unlock()
	if connected || now < max(ident.LastConnectedAt, ident.LastDisconnectedAt, startedAt)+LeaseLifetime.Milliseconds() {
		return fmt.Errorf("%w: older than the worker's current, live boot", ErrBoot)
	}
	live, err := ops.HasLiveLease(ctx, workerID, cur, now)
	if err != nil {
		return err
	}
	if live {
		return fmt.Errorf("%w: older than the worker's current boot, which holds leases", ErrBoot)
	}
	return nil
}

// Connect registers a connection after its handshake. Its boot becomes the
// worker's current boot, and every lease of other boots is released (their
// processes and CLIs are gone); the worker's connections of other boots
// become stale (no Offers). An older boot while the current one is alive is
// refused with ErrBoot and changes nothing (see checkOrder). An error means
// the connection must be closed; the worker reconnects.
func (s *Service) Connect(ctx context.Context, info ConnInfo) (*Conn, error) {
	c := &Conn{s: s, workerID: info.WorkerID, userID: info.UserID, bootID: info.BootID, fail: info.Fail,
		queued: make(map[string]*hint), kick: make(chan struct{}, 1)}
	c.seq = s.seq.Add(1)
	c.id = s.cfg.InstanceID + "." + s.nonce + "." + strconv.FormatUint(c.seq, 10)
	var recs []logRec
	if info.BootID != "" {
		var err error
		if recs, err = s.changeBoot(ctx, c); err != nil {
			s.write(ctx, recs)
			return nil, err
		}
	}
	// The release's records are written once the connection and its boot
	// are registered and stale Offers withdrawn (deferred before the
	// unlock, so it runs after it): until then an Offer could still go to
	// the old boot. A handler that panics, here or in the unlock's writes,
	// would leave a registered connection nobody holds: it is closed, as
	// its link would close it, and the panic goes on with its value.
	registered := false
	defer func() {
		p := recover()
		func() {
			defer func() {
				if q := recover(); q != nil && p == nil {
					p = q
				}
			}()
			s.write(ctx, recs)
		}()
		if p != nil {
			if registered {
				c.Close()
			}
			panic(p)
		}
	}()
	s.mu.Lock()
	defer s.unlock()
	if s.closed {
		return nil, errors.New("sessions: closed")
	}
	s.conns[c.id] = c
	registered = true
	if c.bootID != "" {
		// The current boot follows the store's order of boot transactions,
		// not the order of registration: a connection whose transaction
		// committed before another's never takes over in memory.
		if rec, ok := s.boots[c.workerID]; !ok || c.bootSeq > rec.seq {
			s.boots[c.workerID] = bootRec{boot: c.bootID, seq: c.bootSeq}
		}
		// Offers held by connections now stale go elsewhere.
		for id, e := range s.entries {
			if e.offered != nil && s.stale(e.offered) {
				s.setOffer(id, e, nil, "stale_boot")
				s.queueWake(id, 0)
			}
		}
	}
	return c, nil
}

// stale reports whether c belongs to a boot other than its worker's
// current one; s.mu held. A connection without a boot id never attaches
// and counts as stale too. Staleness is derived from the current boot, so
// a connection of the current boot is never stale.
func (s *Service) stale(c *Conn) bool {
	return c.bootID == "" || s.boots[c.workerID].boot != c.bootID
}

// changeBoot records the connection's boot as the worker's current one,
// unless checkOrder refuses it (ErrBoot, nothing written), and releases
// every lease the worker holds under another boot. It returns the
// release's log records, for Connect to write once the connection is
// registered; every loss is applied before.
func (s *Service) changeBoot(ctx context.Context, c *Conn) ([]logRec, error) {
	b, ok := parseBoot(c.bootID)
	if !ok {
		return nil, fmt.Errorf("%w: not a UUIDv7", ErrBoot)
	}
	now := s.nowMs()
	tctx, cancel := storeCtx(ctx)
	err := s.cfg.Store.Tx(tctx, func(ctx context.Context, tx store.Ops) error {
		if err := s.checkOrder(ctx, tx, c.workerID, b); err != nil {
			return err
		}
		if _, err := tx.SetConnected(ctx, store.KindWorker, c.workerID, c.bootID, now); err != nil {
			return err
		}
		// Boot transactions run one at a time, so this sequence is their
		// commit order.
		s.mu.Lock()
		s.bootSeq++
		c.bootSeq = s.bootSeq
		s.unlock()
		return nil
	})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("sessions: record worker boot: %w", err)
	}
	// Every connect of a boot releases what other boots still hold, not
	// only a boot change: a release that failed on an earlier connect (the
	// connection was closed then) is completed by the next one. It is one
	// cheap query when there is nothing to release.
	// Each page is released only while this boot is still the store's
	// current one: a connection whose boot was superseded meanwhile (an old
	// process registering after the new one) must not release the new
	// boot's leases.
	released := 0
	var recs []logRec
	done := func() []logRec {
		if released > 0 {
			recs = append(recs, logRec{level: slog.LevelInfo, msg: "old boot leases released", attrs: []slog.Attr{
				slog.String("worker_id", c.workerID), slog.String("boot_id", c.bootID), slog.Int("count", released)}})
		}
		return recs
	}
	for {
		var losses []store.LeaseLoss
		tctx, cancel := storeCtx(ctx)
		err := s.cfg.Store.Tx(tctx, func(ctx context.Context, tx store.Ops) error {
			ident, err := tx.GetIdentity(ctx, store.KindWorker, c.workerID)
			if err != nil || ident.CurrentBootID != c.bootID {
				return err
			}
			losses, err = tx.ReleaseWorkerLeases(ctx, c.workerID, c.bootID, s.nowMs(), store.MaxPageSize)
			return err
		})
		cancel()
		if err != nil {
			return done(), fmt.Errorf("sessions: release previous boot's leases: %w", err)
		}
		for _, l := range losses {
			if !l.Repeated {
				released++
			}
			recs = append(recs, s.lost(l, nil, true, endBootChanged)...)
		}
		if len(losses) < store.MaxPageSize {
			return done(), nil
		}
	}
}

// Conn is one worker connection as the session service sees it. Its
// methods other than Close are called from the connection's receive loop,
// one at a time, or (the read functions they return) from the link's
// operation goroutines.
type Conn struct {
	s                         *Service
	id                        string
	seq                       uint64
	bootSeq                   uint64
	workerID, bootID, userID  string
	fail                      func(string)
	sending                   context.CancelFunc
	senderDone                chan struct{}
	kick                      chan struct{}
	send                      func(context.Context, *circusv1.WorkerEvent) error
	closed, attached, wasOnce bool
	// localOnly: the worker said at Attach that this connection runs only
	// local sessions pinned to it (Attach.local_only). Until Attach it is
	// false; nothing is offered before Attach anyway.
	localOnly bool
	// acquires remembers this connection's last AcquireLeases, for a late
	// Unwatch (Unwatched); nextAcquire is the slot to overwrite next.
	acquires       [maxRecentAcquires]recentAcquire
	nextAcquire    int
	max            int
	leases, offers int
	// outbox: sessions with a hint to send, in order, each at most once.
	outbox []string
	queued map[string]*hint
}

// recentAcquire is one AcquireLease a connection made.
type recentAcquire struct {
	rid uint64
	id  string
	tok uint64
}

// hint is what is owed for one session on one connection. Nudge reasons
// and the interrupt level are read from the session's entry when sent.
type hint struct {
	offer, nudge bool
	revoke       int64 // a LeaseRevoked for this epoch, or 0
}

// ID returns the connection's opaque id (it never appears on the wire).
func (c *Conn) ID() string { return c.id }

// Close unregisters the connection. Leases bound to it stay in the store
// and expire unless the worker resumes them on a new connection; Offers
// made to it go to another connection. When it was the last connection of
// its worker's boot, every lease that boot holds without a live connection
// is marked disconnected (worker_disconnected_at, KIND_WORKER) until it is
// bound again or ends.
func (c *Conn) Close() {
	s := c.s
	s.mu.Lock()
	if c.closed {
		s.unlock()
		return
	}
	c.closed = true
	// The disconnect time is taken before the connection leaves s.conns, so
	// whoever sees it gone sees a disconnect no later than that moment,
	// although the store write lands after the sender has stopped.
	at := s.nowMs()
	delete(s.conns, c.id)
	lastOfBoot := c.bootID != ""
	for _, o := range s.conns {
		if o.workerID == c.workerID && o.bootID == c.bootID {
			lastOfBoot = false
		}
	}
	// The last connection of the current boot records the disconnect: the
	// boot counts as alive for LeaseLifetime more (checkOrder).
	lastOfCurrent := lastOfBoot && s.boots[c.workerID].boot == c.bootID
	marked := 0
	for id, e := range s.entries {
		if e.offered == c {
			s.setOffer(id, e, nil, "conn_closed")
			s.queueWake(id, 0)
		}
		if e.bound == c {
			s.setBound(e, nil)
		}
		// Leases bound here, and those left unbound by an earlier
		// connection of the boot that closed while this one was live: the
		// boot now has no connection at all. A lease bound to a live
		// connection cannot belong to this boot (none is left).
		if lastOfBoot && e.bound == nil && e.holder.worker == c.workerID && e.holder.boot == c.bootID {
			if e.gone != at {
				marked++
			}
			s.setGone(id, e, at)
		}
	}
	c.outbox, c.queued = nil, nil
	stop, done := c.sending, c.senderDone
	// The records are written once the sender has stopped and the
	// disconnect is recorded.
	recs := s.unlockKeep()
	if stop != nil {
		stop()
		<-done
	}
	if lastOfCurrent {
		ctx, cancel := storeCtx(context.Background())
		err := s.cfg.Store.SetDisconnected(ctx, store.KindWorker, c.workerID, at)
		cancel()
		if err != nil {
			recs = append(recs, logRec{level: slog.LevelWarn, msg: "recording a worker disconnect failed",
				attrs: []slog.Attr{slog.String("worker_id", c.workerID), slog.Any("error", err)}})
		}
	}
	s.write(context.Background(), recs)
	s.log.LogAttrs(context.Background(), slog.LevelInfo, "worker connection closed", slog.String("worker_id", c.workerID),
		slog.String("boot_id", c.bootID), slog.String("conn_id", c.id), slog.Bool("last_of_boot", lastOfBoot), slog.Int("marked", marked))
}

// ErrAttach is an Attach the link must answer by closing: a second Attach,
// a field cap violation, or a connection without a boot id.
var ErrAttach = errors.New("sessions: invalid attach")

// Attach opens the connection's hint stream. send delivers one WorkerEvent
// on it (the link wraps it with the Attach request id); it may wait for the
// link and returns an error when the event cannot be sent, which closes the
// link. ctx ends the stream (Unwatch or disconnect). Every pending Offer
// for this worker's user is then re-evaluated, Nudges deferred until now are
// sent, and the connection receives Offers from then on.
func (c *Conn) Attach(ctx context.Context, a *circusv1.Attach, send func(context.Context, *circusv1.WorkerEvent) error) error {
	if c.bootID == "" {
		return fmt.Errorf("%w: the connection has no boot id", ErrAttach)
	}
	if a.GetMaxSessions() < 1 || a.GetMaxSessions() > MaxSessionsPerConnection {
		return fmt.Errorf("%w: max_sessions %d outside 1..%d", ErrAttach, a.GetMaxSessions(), MaxSessionsPerConnection)
	}
	for _, v := range []string{a.GetSdkVersion(), a.GetClaudeVersion(), a.GetAccount().GetEmail(), a.GetAccount().GetOrganization(), a.GetAccount().GetSubscriptionType()} {
		if len(v) > maxAttachString {
			return fmt.Errorf("%w: a field longer than %d bytes", ErrAttach, maxAttachString)
		}
	}
	s := c.s
	sctx, stop := context.WithCancel(ctx)
	s.mu.Lock()
	if c.wasOnce || c.closed {
		s.unlock()
		stop()
		return fmt.Errorf("%w: at most one Attach per connection", ErrAttach)
	}
	c.wasOnce, c.attached, c.max, c.send, c.localOnly = true, true, int(a.GetMaxSessions()), send, a.GetLocalOnly()
	c.sending, c.senderDone = stop, make(chan struct{})
	// Nudges for leases bound here before Attach are already in the
	// outbox (queue defers them; the sender starts now), so nothing is
	// re-queued for them. Offers held by another connection of this worker
	// and boot (the one the worker reconnected from) move here: the worker
	// ignores its abandoned connection.
	for id, e := range s.entries {
		if o := e.offered; o != nil && o != c && o.workerID == c.workerID && o.bootID == c.bootID && c.leases+c.offers < c.max && c.runs(e.local, e.pin) {
			s.setOffer(id, e, c, "moved")
			c.queue(id, func(h *hint) { h.offer = true })
		}
	}
	s.rescan[c.userID] = struct{}{}
	s.signal()
	maxSessions, localOnly := c.max, c.localOnly
	recs := s.unlockKeep()
	go c.run(sctx)
	s.write(ctx, recs)
	s.log.InfoContext(ctx, "worker attached", "worker_id", c.workerID, "boot_id", c.bootID, "conn_id", c.id, "max_sessions", maxSessions,
		"local_only", localOnly, "sdk_version", a.GetSdkVersion(), "claude_version", a.GetClaudeVersion(),
		"auto_mode_check", a.GetAutoModeCheck().String())
	if s.debug(ctx) {
		s.log.LogAttrs(ctx, slog.LevelDebug, "worker account", slog.String("worker_id", c.workerID), slog.String("conn_id", c.id),
			slog.String("account_email", a.GetAccount().GetEmail()))
	}
	return nil
}

// runs reports whether connection c may run a session that is local or
// not and pinned to worker pin ("" for none): a pinned session runs only on
// its worker, and a local-only connection runs only local sessions; s.mu
// held. Local and the pin never change, so the answer for a session never
// does either.
func (c *Conn) runs(local bool, pin string) bool {
	return (pin == "" || pin == c.workerID) && (!c.localOnly || local)
}

// queue adds a hint for session id; s.mu held. Before Attach the hint
// waits in the outbox, which the sender started by Attach drains: this is
// the only deferral path for hints of a connection without a stream.
func (c *Conn) queue(id string, set func(*hint)) {
	if c.closed {
		return
	}
	h := c.queued[id]
	if h == nil {
		h = &hint{}
		c.queued[id] = h
		c.outbox = append(c.outbox, id)
	}
	set(h)
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

// run sends the connection's hints until ctx ends. It never holds s.mu while
// sending.
func (c *Conn) run(ctx context.Context) {
	defer close(c.senderDone)
	s := c.s
	for {
		s.mu.Lock()
		var events []*circusv1.WorkerEvent
		for len(events) == 0 && len(c.outbox) > 0 {
			id := c.outbox[0]
			c.outbox[0] = ""
			c.outbox = c.outbox[1:]
			h := c.queued[id]
			delete(c.queued, id)
			events = s.hintEvents(c, id, h)
		}
		if len(c.outbox) == 0 {
			c.outbox = nil // let the backing array go
		}
		attached := c.attached
		s.unlock()
		if !attached {
			return
		}
		if len(events) == 0 {
			select {
			case <-c.kick:
				continue
			case <-ctx.Done():
				c.detach()
				return
			}
		}
		for _, ev := range events {
			if err := c.send(ctx, ev); err != nil {
				if ctx.Err() == nil {
					c.fail("worker event could not be sent: " + err.Error())
				}
				c.detach()
				return
			}
			if n := ev.GetNudge(); n != nil && s.debug(ctx) {
				s.log.LogAttrs(ctx, slog.LevelDebug, "nudge sent", slog.String("session_id", n.GetSessionId()), slog.String("conn_id", c.id),
					slog.String("reasons", reasonNames(n.GetReasons())))
			}
		}
	}
}

// detach ends the hint stream: the connection gets no more Offers, and
// those it has go elsewhere.
func (c *Conn) detach() {
	s := c.s
	s.mu.Lock()
	defer s.unlock()
	c.attached = false
	for id, e := range s.entries {
		if e.offered == c {
			s.setOffer(id, e, nil, "detached")
			s.queueWake(id, 0)
		}
	}
}

// hintEvents builds the events owed for session id from the current state;
// s.mu held. A revoked lease suppresses a Nudge for it.
func (s *Service) hintEvents(c *Conn, id string, h *hint) []*circusv1.WorkerEvent {
	if h == nil {
		return nil
	}
	var out []*circusv1.WorkerEvent
	if h.revoke > 0 {
		out = append(out, &circusv1.WorkerEvent{Event: &circusv1.WorkerEvent_LeaseRevoked_{LeaseRevoked: &circusv1.WorkerEvent_LeaseRevoked{SessionId: id, Epoch: uint64(h.revoke)}}})
	}
	e := s.entries[id]
	if e == nil {
		return out
	}
	if h.offer && e.offered == c {
		out = append(out, &circusv1.WorkerEvent{Event: &circusv1.WorkerEvent_Offer_{Offer: &circusv1.WorkerEvent_Offer{SessionId: id}}})
	}
	rs := e.reasons
	if e.held {
		rs &^= turnReasons
	}
	if h.nudge && e.bound == c && rs != 0 {
		out = append(out, &circusv1.WorkerEvent{Event: &circusv1.WorkerEvent_Nudge_{Nudge: &circusv1.WorkerEvent_Nudge{
			SessionId: id, Reasons: rs.list(), InterruptTurnNo: uint64(e.interrupt)}}})
	}
	return out
}
