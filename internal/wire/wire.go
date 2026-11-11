package wire

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"google.golang.org/protobuf/proto"
)

// Conn carries binary messages. Close aborts and unblocks concurrent I/O.
// Implementations bound allocation before returning a frame. Decorators embedding
// Conn retain read-limit negotiation and graceful close semantics.
type Conn interface {
	ReadMessage(context.Context) ([]byte, error)
	WriteMessage(context.Context, []byte) error
	Close() error
	SetReadLimit(int64)
	Shutdown(context.Context) error
}

const (
	MaxOrderingKeyBytes     = 1024
	DefaultMaxFragmentBytes = 256 << 10
	DefaultMaxMessageBytes  = 64 << 20
	FrameOverhead           = 128
	// LinkReserveBytes is each budgeted link's own receive reserve. A frame
	// whose decode admission (conservative decoded cost plus its encoded length,
	// see the package memory model) fits it, such as a heartbeat, ack, GoAway or
	// other small control/business frame of a few KiB, never waits for the shared
	// receive pool. Small accepted messages stay charged to it until Receive.
	LinkReserveBytes = 64 << 10
)

var (
	// ErrBusy rejects a Send while the shared send pool is exhausted; the link
	// stays usable and the caller may retry.
	ErrBusy = errors.New("wire shared send budget exhausted")
	// ErrBudgetExhausted closes a link whose reader waited longer than
	// MaxConsumerStall for the shared receive pool held by other owners.
	ErrBudgetExhausted = errors.New("wire shared receive budget exhausted")
	// ErrConsumerStalled closes a link whose own consumer did not Receive for
	// longer than MaxConsumerStall while the reader needed local space.
	ErrConsumerStalled = errors.New("wire local consumer stalled")
	ErrShutdownTimeout = errors.New("wire shutdown timed out")
	ErrClosed          = errors.New("wire link closed")
	ErrClosing         = errors.New("wire link is shutting down")
	ErrQueueFull       = errors.New("wire queue budget exceeded")
	ErrTooLarge        = errors.New("wire local message exceeds resource limits")
)

// Side fixes the two directions together; callers cannot swap them accidentally.
type Side uint8

const (
	DaemonSide Side = iota + 1
	DaemonControlSide
	WorkerSide
	WorkerControlSide
)

func (s Side) kinds() (Kind, Kind) {
	switch s {
	case DaemonSide:
		return DaemonDownlink, DaemonUplink
	case DaemonControlSide:
		return DaemonUplink, DaemonDownlink
	case WorkerSide:
		return WorkerDownlink, WorkerUplink
	case WorkerControlSide:
		return WorkerUplink, WorkerDownlink
	default:
		return 0, 0
	}
}

// Class declares scheduling, not business semantics. Data is FIFO per nonempty
// ordering key; unrelated small messages may bypass large ones. Control can jump
// the queue (use only for heartbeat/GoAway/ack). Barrier waits for all earlier
// Data/Barrier messages; use it for ListJobs and StopSessionJobs. Callers use the
// same key for every Dispatch/Submit/Query/Cancel of a call and one key for job
// events of a boot. Different keys may complete out of enqueue order.
type Class uint8

const (
	Data Class = iota
	Control
	Barrier
)

// Options are negotiated BEFORE New, on the raw Conn using Decode. Zero fields
// select defaults. In-flight limits must be agreed in both directions. Send
// starts at most min(2, MaxInFlightMessages) fragmented messages with declarations
// totaling at most MaxInFlightBytes. The receiver enforces the same count cap.
type Options struct {
	Clock                                 clock.Clock
	Budget                                *Budget
	MaxConsumerStall                      time.Duration
	MaxFragmentBytes, MaxMessageBytes     int
	MaxInFlightBytes, MaxInFlightMessages int
	MaxQueuedBytes, MaxQueuedMessages     int
	// MaxReceiveBytes charges decoded allocation estimates, not proto.Size.
	// Default: twice MaxMessageBytes plus 8 MiB. An explicit value must cover
	// 2*MaxMessageBytes + 6 MiB + 4 KiB of conservative decoded cost.
	MaxReceiveBytes                                      int
	HeartbeatInterval, HeartbeatTimeout, FragmentTimeout time.Duration
	DisableHeartbeat                                     bool
}

func (o Options) defaults() (Options, error) {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.MaxFragmentBytes == 0 {
		o.MaxFragmentBytes = DefaultMaxFragmentBytes
	}
	if o.MaxMessageBytes == 0 {
		o.MaxMessageBytes = DefaultMaxMessageBytes
	}
	if o.MaxMessageBytes <= 0 || o.MaxMessageBytes > int(^uint(0)>>1)/4 {
		return o, fmt.Errorf("wire: invalid message limit")
	}
	if o.MaxInFlightBytes == 0 {
		o.MaxInFlightBytes = 2 * o.MaxMessageBytes
	}
	if o.MaxInFlightMessages == 0 {
		o.MaxInFlightMessages = 2
	}
	if o.MaxQueuedBytes == 0 {
		o.MaxQueuedBytes = 2 * o.MaxMessageBytes
	}
	if o.MaxQueuedMessages == 0 {
		o.MaxQueuedMessages = 256
	}
	if o.HeartbeatInterval == 0 {
		o.HeartbeatInterval = 5 * time.Second
	}
	if o.HeartbeatTimeout == 0 {
		o.HeartbeatTimeout = 15 * time.Second
	}
	if o.MaxConsumerStall == 0 {
		o.MaxConsumerStall = o.HeartbeatTimeout
	}
	if o.MaxReceiveBytes == 0 {
		o.MaxReceiveBytes = 2*o.MaxMessageBytes + (8 << 20)
	}
	if o.FragmentTimeout == 0 {
		o.FragmentTimeout = o.HeartbeatTimeout
	}
	if o.MaxFragmentBytes <= 0 || o.MaxMessageBytes < o.MaxFragmentBytes || o.MaxInFlightBytes <= 0 || o.MaxInFlightMessages <= 0 || o.MaxInFlightMessages > 2 || o.MaxQueuedBytes <= 0 || o.MaxQueuedMessages <= 0 || o.MaxReceiveBytes < maxMessageCost(o.MaxMessageBytes) || o.MaxConsumerStall <= 0 || o.HeartbeatInterval <= 0 || o.HeartbeatTimeout <= o.HeartbeatInterval || o.FragmentTimeout <= 0 {
		return o, fmt.Errorf("wire: invalid options")
	}
	if o.Budget != nil {
		send, receive := minBudget(o)
		if o.Budget.send.limit < send || o.Budget.receive.limit < receive {
			return o, fmt.Errorf("wire: shared budget cannot hold one maximum-size message (need send >= %d, receive >= %d bytes)", send, receive)
		}
	}
	return o, nil
}

// Stats reports real ownership. Done means I/O stopped. Without a Budget, received
// messages remain drainable after EOF until Receive or Close releases them; with
// one, an error drops those holding shared budget at once and a graceful end
// after MaxConsumerStall (see the package memory model). ReassemblyBytes is reservation;
// ReassemblyAllocatedBytes is actual block capacity. SharedSendBytes and
// SharedReceiveBytes are this link's charges in the two shared pools;
// LinkReserveBytes is the used part of its own LinkReserveBytes reserve.
type Stats struct {
	ReassemblyBlocks                                              int
	QueuedBytes, QueuedMessages                                   int
	ReassemblyBytes, ReassemblyAllocatedBytes, ReassemblyMessages int
	ReceivedMessages, ReceivedBytes                               int
	ActiveMessages                                                int
	ReadBlocked                                                   bool
	SharedSendBytes, SharedReceiveBytes                           int64
	LinkReserveBytes                                              int
	FramesRead, FramesWritten, HeartbeatsRead, HeartbeatsWritten  uint64
}

// outbound holds an unfragmented frame in data, or a fragmented message as
// per-fragment chunks so every written fragment frees its memory. charged is
// the part still reserved from the shared send pool.
type outbound struct {
	data          []byte
	chunks        [][]byte
	size, charged int
	index         uint32
	id, order     uint64
	class         Class
	key           string
	heartbeat     bool
}
type inbound struct {
	message proto.Message
	charge  charge
}

// A charge records where admission came from; n is also the decoded cost that
// ReceivedBytes accounts for an accepted message.
type charge struct {
	n    int
	from source
}
type source uint8

const (
	unbudgeted source = iota
	sharedPool
	linkReserve
)

// Link owns and joins its reader, writer, timer and join goroutines.
type Link struct {
	sharedSend, sharedReceive          atomic.Int64
	heartbeatQueued                    bool
	controlUsed, smallUsed             bool
	largeUsed                          int
	sendMu                             sync.Mutex
	conn                               Conn
	incoming, outgoing                 Kind
	opts                               Options
	ctx                                context.Context
	cancel                             context.CancelFunc
	done, wake, notify, space, changed chan struct{}
	// stopDrain (closed once by Close) ends expireDrain; drainDone is non-nil
	// while a graceful drain deadline runs. Guarded by mu.
	stopDrain, drainDone         chan struct{}
	closeOnce                    sync.Once
	testAdmitted                 func() // test hook between admission and acceptance
	mu                           sync.Mutex
	err                          error
	closing                      bool
	queue                        []*outbound
	received                     []inbound
	stats                        Stats
	lastRead, reassemblyDeadline time.Time
	heartbeat                    bool
	order                        uint64
	wg                           sync.WaitGroup
}

// New takes Conn ownership only on success. Complete Hello/Welcome/Reject on
// raw Conn first, then create Link with negotiated limits. WebSocket allocation
// limits are applied here; authentication, handshake and reconnect policy stay
// outside wire.
func New(conn Conn, side Side, opts Options) (*Link, error) {
	o, err := opts.defaults()
	if err != nil {
		return nil, err
	}
	in, out := side.kinds()
	if conn == nil || in == 0 {
		return nil, fmt.Errorf("wire: invalid side or connection")
	}
	conn.SetReadLimit(int64(o.MaxFragmentBytes) + FrameOverhead)
	ctx, cancel := context.WithCancel(context.Background())
	l := &Link{conn: conn, incoming: in, outgoing: out, opts: o, ctx: ctx, cancel: cancel, done: make(chan struct{}), wake: make(chan struct{}, 1), notify: make(chan struct{}, 1), space: make(chan struct{}, 1), changed: make(chan struct{}, 1), stopDrain: make(chan struct{}), lastRead: o.Clock.Now(), heartbeat: !o.DisableHeartbeat}
	started := l.lastRead
	tick := o.Clock.After(min(o.HeartbeatInterval, o.FragmentTimeout))
	l.wg.Add(3)
	go func() { defer l.wg.Done(); l.read() }()
	go func() { defer l.wg.Done(); l.write() }()
	go func() { defer l.wg.Done(); l.timer(tick, started) }()
	go func() { l.wg.Wait(); close(l.done) }()
	return l, nil
}
func (l *Link) StartHeartbeat() {
	l.mu.Lock()
	if !l.heartbeat {
		l.heartbeat = true
		l.lastRead = l.opts.Clock.Now()
	}
	l.mu.Unlock()
}

// Send serializes one bounded message. Accepted sends survive caller context
// cancellation. Queue overflow aborts; local size rejection leaves Link usable.
func (l *Link) Send(ctx context.Context, m proto.Message, class Class, orderingKey string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.mu.Lock()
	err := l.err
	closing := l.closing
	l.mu.Unlock()
	if err != nil {
		return err
	}
	if closing {
		return ErrClosing
	}
	if len(orderingKey) > MaxOrderingKeyBytes {
		return ErrTooLarge
	}
	if class > Barrier || payload(m) == nil || m.ProtoReflect().Descriptor() != l.outgoing.newMessage().ProtoReflect().Descriptor() {
		return fmt.Errorf("wire: wrong class, direction or payload")
	}
	switch body(m).(type) {
	case *circusv1.Fragment, *circusv1.Heartbeat:
		return fmt.Errorf("wire: reserved transport payload")
	}
	size := proto.Size(m)
	fragmented := size > l.opts.MaxFragmentBytes
	if size > min(l.opts.MaxMessageBytes, payloadLimit(payload(m).Name(), l.opts.MaxFragmentBytes, l.opts.MaxMessageBytes)) || fragmented && size > l.opts.MaxInFlightBytes {
		return ErrTooLarge
	}
	// A fragmented message is copied into fragment-sized chunks so each written
	// fragment really frees its share; reserve that brief overlap before Marshal.
	charged := size
	if fragmented {
		charged = 2 * size
	}
	if !l.reserveSend(charged) {
		return ErrBusy
	}
	accepted := false
	defer func() {
		if !accepted {
			l.releaseSend(charged)
		}
	}()
	l.sendMu.Lock()
	defer l.sendMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	if len(data) != size {
		return fmt.Errorf("wire: message changed during Send")
	}
	if _, err := scan(l.outgoing, data, l.opts.MaxMessageBytes, l.opts.MaxFragmentBytes); err != nil {
		return fmt.Errorf("%w: %v", ErrTooLarge, err)
	}
	out := &outbound{data: data, size: size, class: class, key: strings.Clone(orderingKey)}
	if fragmented {
		out.data = nil
		for off := 0; off < size; off += l.opts.MaxFragmentBytes {
			out.chunks = append(out.chunks, bytes.Clone(data[off:min(off+l.opts.MaxFragmentBytes, size)]))
		}
		data = nil
		l.releaseSend(size) // the marshaled buffer is garbage now
		charged = size
	}
	out.charged = charged
	err = l.enqueue(out)
	accepted = err == nil
	return err
}
func (l *Link) enqueue(m *outbound) error {
	l.mu.Lock()
	if l.err != nil {
		err := l.err
		l.mu.Unlock()
		return err
	}
	if l.closing {
		l.mu.Unlock()
		return ErrClosing
	}
	if !m.heartbeat && (m.size > l.opts.MaxQueuedBytes-l.stats.QueuedBytes || l.stats.QueuedMessages >= l.opts.MaxQueuedMessages) {
		l.mu.Unlock()
		l.fail(ErrQueueFull)
		return ErrQueueFull
	}
	if m.heartbeat {
		if l.heartbeatQueued {
			l.mu.Unlock()
			return nil
		}
		l.heartbeatQueued = true
	}
	l.order++
	m.order = l.order
	l.queue = append(l.queue, m)
	if !m.heartbeat {
		l.stats.QueuedBytes += m.size
		l.stats.QueuedMessages++
	}
	l.mu.Unlock()
	signal(l.wake)
	return nil
}
func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// Receive drains complete messages before terminal errors, including after Done.
// With a Budget, a link that failed (or ended gracefully and was not drained
// within MaxConsumerStall) has dropped messages holding shared receive budget, so
// Receive returns the remaining in-order prefix and then the error.
func (l *Link) Receive(ctx context.Context) (proto.Message, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		l.mu.Lock()
		if len(l.received) > 0 {
			entry := l.received[0]
			l.received[0] = inbound{}
			l.received = l.received[1:]
			l.stats.ReceivedBytes -= entry.charge.n
			l.releaseLocked(entry.charge)
			l.stats.ReceivedMessages--
			if len(l.received) > 0 {
				signal(l.notify)
			}
			l.mu.Unlock()
			signal(l.space)
			return entry.message, nil
		}
		if l.err != nil {
			err := l.err
			l.mu.Unlock()
			return nil, err
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-l.ctx.Done():
		case <-l.notify:
		}
	}
}

// fail ends the link on an error or local abort: shared receive budget is
// returned at once (see dropSharedLocked).
func (l *Link) fail(err error) { l.terminate(err, false) }

// terminate records the first terminal error. After a graceful end (the peer's
// clean EOF or normal close, or a successful local Shutdown) every received
// message stays deliverable; messages holding shared budget are dropped only if
// the consumer has not drained them within MaxConsumerStall. Release never
// depends on Receive or Close.
func (l *Link) terminate(err error, graceful bool) {
	l.mu.Lock()
	if l.err != nil {
		l.mu.Unlock()
		return
	}
	l.err = err
	if !graceful {
		l.dropSharedLocked()
	} else if l.holdsSharedLocked() {
		l.drainDone = make(chan struct{})
		go l.expireDrain(l.drainDone)
	}
	l.mu.Unlock()
	l.cancel()
	signal(l.changed)
	_ = l.conn.Close()
}

// expireDrain bounds how long undelivered messages keep shared budget after a
// graceful end. Close stops it early (and drops everything itself).
func (l *Link) expireDrain(done chan struct{}) {
	defer close(done)
	select {
	case <-l.opts.Clock.After(l.opts.MaxConsumerStall):
		l.mu.Lock()
		l.dropSharedLocked()
		l.mu.Unlock()
	case <-l.stopDrain:
	}
}

// dropSharedLocked returns shared receive budget now: it drops accepted messages
// from the first shared-charged one onward, keeping an in-order prefix of
// reserve-only messages. l.mu must be held.
func (l *Link) dropSharedLocked() {
	for i, entry := range l.received {
		if entry.charge.from == sharedPool {
			l.dropReceivedLocked(i)
			break
		}
	}
}
func (l *Link) holdsSharedLocked() bool {
	for _, entry := range l.received {
		if entry.charge.from == sharedPool {
			return true
		}
	}
	return false
}

// Close aborts I/O, joins it, and discards unread received messages. Every gauge
// is decremented from owned entries, never assigned zero to mask bad accounting.
func (l *Link) Close() error {
	l.fail(ErrClosed)
	<-l.done
	l.closeOnce.Do(func() { close(l.stopDrain) })
	l.mu.Lock()
	drain := l.drainDone
	l.mu.Unlock()
	if drain != nil {
		<-drain
	}
	l.mu.Lock()
	l.dropReceivedLocked(0)
	l.received = nil
	l.mu.Unlock()
	return nil
}

// dropReceivedLocked discards accepted messages from index i onward, decrementing
// every gauge from the owned entries. l.mu must be held.
func (l *Link) dropReceivedLocked(i int) {
	for j := i; j < len(l.received); j++ {
		entry := l.received[j]
		l.stats.ReceivedBytes -= entry.charge.n
		l.releaseLocked(entry.charge)
		l.stats.ReceivedMessages--
		l.received[j] = inbound{}
	}
	l.received = l.received[:i]
}

// Shutdown stops admission, drains accepted writes, performs a transport close
// handshake, and joins I/O. A deadline forces abort. Without a Budget it
// preserves already received messages; call Receive to drain or Close to
// discard them. With one, undrained messages holding shared budget are dropped
// MaxConsumerStall after the end (see terminate).
func (l *Link) Shutdown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		err = shutdownTimeout(err)
		l.fail(err)
		<-l.done
		return err
	}
	l.mu.Lock()
	l.closing = true
	l.mu.Unlock()
	signal(l.wake)
	for {
		l.mu.Lock()
		pending, err := len(l.queue), l.err
		l.mu.Unlock()
		if pending == 0 {
			break
		}
		if err != nil {
			<-l.done
			return err
		}
		select {
		case <-ctx.Done():
			err := shutdownTimeout(ctx.Err())
			l.fail(err)
			<-l.done
			return err
		case <-l.changed:
		case <-l.done:
		}
	}
	err := l.conn.Shutdown(ctx)
	if ctx.Err() != nil {
		err = shutdownTimeout(ctx.Err())
	}
	if err != nil {
		l.fail(err)
	} else {
		l.terminate(ErrClosed, true)
	}
	<-l.done
	return err
}
func (l *Link) Done() <-chan struct{} { return l.done }
func (l *Link) Err() error            { l.mu.Lock(); defer l.mu.Unlock(); return l.err }
func (l *Link) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.stats
	s.SharedSendBytes = l.sharedSend.Load()
	s.SharedReceiveBytes = l.sharedReceive.Load()
	return s
}

func (l *Link) read() {
	r := newReassembler(l.opts)
	r.reserve = func(n int) error {
		return l.waitReceive(r, true, func() (bool, <-chan struct{}) { return l.reserveShared(n) })
	}
	r.releaseBytes = l.releaseShared
	publish := func() {
		l.stats.ReassemblyBytes = r.bytes
		l.stats.ReassemblyAllocatedBytes = r.allocated
		l.stats.ReassemblyBlocks = r.blocks()
		l.stats.ReassemblyMessages = len(r.messages)
		l.reassemblyDeadline = r.deadline()
	}
	defer func() { r.clear(); l.mu.Lock(); publish(); l.stats.ReadBlocked = false; l.mu.Unlock() }()
	for {
		data, err := l.conn.ReadMessage(l.ctx)
		if err != nil {
			if len(r.messages) > 0 {
				l.fail(protocol("missing fragment at EOF: %v", err))
				return
			}
			// A clean end of stream after a complete frame keeps every
			// delivered message; anything else is an error.
			l.terminate(err, gracefulEnd(err))
			return
		}
		now := l.opts.Clock.Now()
		if deadline := r.deadline(); !deadline.IsZero() && !now.Before(deadline) {
			l.fail(protocol("missing fragment timeout"))
			return
		}
		l.mu.Lock()
		l.lastRead = now
		l.stats.FramesRead++
		l.mu.Unlock()
		m, c, err := l.decodeReceived(r, data, l.opts.MaxFragmentBytes+FrameOverhead, false)
		if err != nil {
			l.fail(err)
			return
		}
		if f, ok := body(m).(*circusv1.Fragment); ok {
			data, err = r.add(f, l.opts.Clock.Now())
			l.releaseCharge(c)
			l.mu.Lock()
			publish()
			l.mu.Unlock()
			if err != nil {
				l.fail(err)
				return
			}
			if data == nil {
				continue
			}
			m, c, err = l.decodeReceived(r, data, l.opts.MaxMessageBytes, true)
			l.releaseShared(len(data)) // completed assembly buffer, held through decoding
			data = nil
			if err != nil {
				l.fail(err)
				return
			}
			if _, ok := body(m).(*circusv1.Fragment); ok {
				l.releaseCharge(c)
				l.fail(protocol("nested fragment"))
				return
			}
		} else if len(data) > l.opts.MaxFragmentBytes || len(data) > l.opts.MaxMessageBytes {
			l.releaseCharge(c)
			l.fail(protocol("unfragmented message too large"))
			return
		}
		if _, ok := body(m).(*circusv1.Heartbeat); ok {
			l.releaseCharge(c)
			l.mu.Lock()
			l.stats.HeartbeatsRead++
			l.mu.Unlock()
			continue
		}
		err = l.waitReceive(r, false, func() (bool, <-chan struct{}) {
			l.mu.Lock()
			defer l.mu.Unlock()
			return c.n <= l.opts.MaxReceiveBytes-l.stats.ReceivedBytes, nil
		})
		if err != nil {
			l.releaseCharge(c)
			l.fail(err)
			return
		}
		if l.testAdmitted != nil {
			l.testAdmitted()
		}
		l.mu.Lock()
		if l.err != nil { // failed meanwhile: never accept after the drop in fail
			l.releaseLocked(c)
			l.mu.Unlock()
			return
		}
		l.received = append(l.received, inbound{m, c})
		l.stats.ReceivedBytes += c.n
		l.stats.ReceivedMessages++
		l.mu.Unlock()
		signal(l.notify)
	}
}

// waitReceive pauses only this reader. shared reports whether acquire waits for
// the shared receive pool (expiry: ErrBudgetExhausted) or for this link's own
// consumer to free local space (expiry: ErrConsumerStalled). A single reader
// owns r; receivers may only release admission in parallel.
func (l *Link) waitReceive(r *reassembler, shared bool, acquire func() (bool, <-chan struct{})) error {
	stalled := ErrConsumerStalled
	if shared {
		stalled = ErrBudgetExhausted
	}
	blockedAt := time.Time{}
	var stall <-chan time.Time
	for {
		if err := l.ctx.Err(); err != nil {
			return err
		}
		if !blockedAt.IsZero() && l.opts.Clock.Now().Sub(blockedAt) >= l.opts.MaxConsumerStall {
			return stalled
		}
		ok, changed := acquire()
		if ok {
			if !blockedAt.IsZero() {
				r.pause(l.opts.Clock.Now().Sub(blockedAt))
				l.mu.Lock()
				l.lastRead = l.opts.Clock.Now()
				l.stats.ReadBlocked = false
				l.reassemblyDeadline = r.deadline()
				l.mu.Unlock()
			}
			return nil
		}
		if blockedAt.IsZero() {
			blockedAt = l.opts.Clock.Now()
			stall = l.opts.Clock.After(l.opts.MaxConsumerStall)
			l.mu.Lock()
			l.stats.ReadBlocked = true
			l.mu.Unlock()
		}
		select {
		case <-l.ctx.Done():
			return l.ctx.Err()
		case <-stall:
			return stalled
		case <-l.space:
		case <-changed:
		}
	}
}

// decodeReceived admits decoded cost plus one encoded length of transient
// decoder credit before Unmarshal, then returns that credit. With a Budget, a
// frame whose admission fits LinkReserveBytes uses the link's own reserve;
// larger frames and every reassembled message use the shared receive pool.
func (l *Link) decodeReceived(r *reassembler, data []byte, limit int, assembled bool) (proto.Message, charge, error) {
	b, err := scan(l.incoming, data, limit, l.opts.MaxFragmentBytes)
	if err != nil {
		return nil, charge{}, err
	}
	cost := b.cost(len(data))
	c := charge{n: cost + len(data)}
	switch {
	case l.opts.Budget == nil:
	case !assembled && c.n <= LinkReserveBytes:
		c.from = linkReserve
		err = l.waitReceive(r, false, func() (bool, <-chan struct{}) {
			l.mu.Lock()
			defer l.mu.Unlock()
			if c.n > LinkReserveBytes-l.stats.LinkReserveBytes {
				return false, nil
			}
			l.stats.LinkReserveBytes += c.n
			return true, nil
		})
	default:
		c.from = sharedPool
		err = l.waitReceive(r, true, func() (bool, <-chan struct{}) { return l.reserveShared(c.n) })
	}
	if err != nil {
		return nil, charge{}, err
	}
	m := l.incoming.newMessage()
	if err = (proto.UnmarshalOptions{RecursionLimit: 32}).Unmarshal(data, m); err != nil {
		l.releaseCharge(c)
		return nil, charge{}, protocol("malformed protobuf: %v", err)
	}
	l.releaseCharge(charge{n: len(data), from: c.from}) // transient decoder growth credit
	c.n = cost
	return m, c, nil
}
func (l *Link) reserveSend(n int) bool {
	if l.opts.Budget == nil {
		return true
	}
	ok, _ := l.opts.Budget.send.reserve(n)
	if ok {
		l.sharedSend.Add(int64(n))
	}
	return ok
}
func (l *Link) releaseSend(n int) {
	if l.opts.Budget != nil && n != 0 {
		l.sharedSend.Add(-int64(n))
		l.opts.Budget.send.release(n)
	}
}
func (l *Link) reserveShared(n int) (bool, <-chan struct{}) {
	if l.opts.Budget == nil {
		return true, nil
	}
	ok, ch := l.opts.Budget.receive.reserve(n)
	if ok {
		l.sharedReceive.Add(int64(n))
	}
	return ok, ch
}
func (l *Link) releaseShared(n int) {
	if l.opts.Budget != nil && n != 0 {
		l.sharedReceive.Add(-int64(n))
		l.opts.Budget.receive.release(n)
	}
}

// releaseLocked returns c to where it was admitted. l.mu must be held.
func (l *Link) releaseLocked(c charge) {
	switch c.from {
	case sharedPool:
		l.releaseShared(c.n)
	case linkReserve:
		l.stats.LinkReserveBytes -= c.n
	}
}
func (l *Link) releaseCharge(c charge) {
	if c.from == unbudgeted {
		return
	}
	l.mu.Lock()
	l.releaseLocked(c)
	l.mu.Unlock()
}
func shutdownTimeout(err error) error { return fmt.Errorf("%w: %w", ErrShutdownTimeout, err) }

func (l *Link) blocked(m *outbound) bool {
	for _, p := range l.queue {
		if p.order >= m.order {
			continue
		}
		if m.key != "" && p.key == m.key {
			return true
		}
		if (m.class == Barrier || p.class == Barrier) && p.class != Control && m.class != Control {
			return true
		}
	}
	return false
}
func (l *Link) canStart(m *outbound) bool {
	if m.id != 0 || m.chunks == nil {
		return true
	}
	active, bytes := 0, 0
	for _, p := range l.queue {
		if p.id != 0 {
			active++
			bytes += p.size
		}
	}
	return active < min(2, l.opts.MaxInFlightMessages) && m.size <= l.opts.MaxInFlightBytes-bytes
}

// Each round gives eligible urgent traffic first service, then one small
// business message, then up to K rotating large fragments. A started large
// control joins rotation; it cannot monopolize the link until logical completion.
func (l *Link) next() *outbound {
	for attempt := 0; attempt < 2; attempt++ {
		if !l.controlUsed {
			for _, m := range l.queue {
				if m.class == Control && !l.blocked(m) && l.canStart(m) {
					if m.chunks != nil && m.id != 0 {
						continue
					}
					l.controlUsed = true
					return m
				}
			}
		}
		if !l.smallUsed {
			for _, m := range l.queue {
				if m.class != Control && m.chunks == nil && !l.blocked(m) {
					l.smallUsed = true
					return m
				}
			}
		}
		if l.largeUsed < min(2, l.opts.MaxInFlightMessages) {
			for _, m := range l.queue {
				if m.chunks != nil && !l.blocked(m) && l.canStart(m) {
					l.largeUsed++
					return m
				}
			}
		}
		l.controlUsed = false
		l.smallUsed = false
		l.largeUsed = 0
	}
	return nil
}
func (l *Link) write() {
	defer func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		for _, m := range l.queue {
			if !m.heartbeat {
				l.stats.QueuedBytes -= m.size
				l.stats.QueuedMessages--
				l.releaseSend(m.charged)
				m.charged = 0
			} else {
				l.heartbeatQueued = false
			}
			if m.id != 0 {
				l.stats.ActiveMessages--
			}
		}
		l.queue = nil
		signal(l.changed)
	}()
	var id uint64
	for {
		if l.ctx.Err() != nil {
			return
		}
		l.mu.Lock()
		m := l.next()
		if m != nil && m.chunks != nil && m.id == 0 {
			id++
			if id == 0 {
				l.mu.Unlock()
				l.fail(protocol("message ID exhausted"))
				return
			}
			m.id = id
			l.stats.ActiveMessages++
		}
		l.mu.Unlock()
		if m == nil {
			select {
			case <-l.ctx.Done():
				return
			case <-l.wake:
				continue
			}
		}
		// Only this goroutine advances m's progress fields and chunks.
		data, written := m.data, m.size
		if m.chunks != nil {
			chunk := m.chunks[m.index]
			f := &circusv1.Fragment{MessageId: m.id, Index: m.index, Last: int(m.index) == len(m.chunks)-1, Data: chunk}
			if m.index == 0 {
				f.TotalSize = uint64(m.size)
			}
			data, _ = proto.Marshal(wrap(l.outgoing, "fragment", f))
			written = len(chunk)
		}
		if err := l.conn.WriteMessage(l.ctx, data); err != nil {
			l.fail(err)
			return
		}
		l.mu.Lock()
		l.stats.FramesWritten++
		if m.heartbeat {
			l.stats.HeartbeatsWritten++
		}
		for i, q := range l.queue {
			if q == m {
				copy(l.queue[i:], l.queue[i+1:])
				l.queue[len(l.queue)-1] = nil
				l.queue = l.queue[:len(l.queue)-1]
				break
			}
		}
		// Progressive release: each written fragment returns its share at once.
		done := true
		if m.chunks != nil {
			m.chunks[m.index] = nil
			m.index++
			done = int(m.index) == len(m.chunks)
		}
		released := min(written, m.charged)
		if done {
			released = m.charged
		}
		m.charged -= released
		l.releaseSend(released)
		if done {
			m.data = nil
			if !m.heartbeat {
				l.stats.QueuedBytes -= m.size
				l.stats.QueuedMessages--
			} else {
				l.heartbeatQueued = false
			}
			if m.id != 0 {
				l.stats.ActiveMessages--
			}
		} else {
			l.queue = append(l.queue, m)
		}
		l.mu.Unlock()
		signal(l.changed)
	}
}
func (l *Link) timer(tick <-chan time.Time, lastHeartbeat time.Time) {
	var seq uint64
	for {
		select {
		case <-l.ctx.Done():
			return
		case <-tick:
			now := l.opts.Clock.Now()
			l.mu.Lock()
			alive, enabled, deadline, paused, closing := l.lastRead, l.heartbeat, l.reassemblyDeadline, l.stats.ReadBlocked, l.closing
			l.mu.Unlock()
			if !paused && !deadline.IsZero() && !now.Before(deadline) {
				l.fail(protocol("missing fragment timeout"))
				return
			}
			if !paused && enabled && now.Sub(alive) >= l.opts.HeartbeatTimeout {
				l.fail(protocol("heartbeat timeout"))
				return
			}
			if enabled && !closing && now.Sub(lastHeartbeat) >= l.opts.HeartbeatInterval {
				seq++
				data, _ := proto.Marshal(wrap(l.outgoing, "heartbeat", &circusv1.Heartbeat{Seq: seq, SentUnixMs: now.UnixMilli()}))
				if err := l.enqueue(&outbound{data: data, size: len(data), class: Control, heartbeat: true}); err != nil {
					if !errors.Is(err, ErrClosing) {
						return
					}
				}
				lastHeartbeat = now
			}
			tick = l.opts.Clock.After(min(l.opts.HeartbeatInterval, l.opts.FragmentTimeout))
		}
	}
}
