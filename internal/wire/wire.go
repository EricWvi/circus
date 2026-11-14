package wire

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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
// the queue (heartbeat, GoAway, ack). Barrier waits for all earlier
// Data/Barrier messages (ListJobs, StopSessionJobs). Send classifies every
// message itself (see classify): every Dispatch/Submit/Query/Cancel of a call
// shares one key, and so do the job events of a boot. Different keys may
// complete out of enqueue order.
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

func (l *Link) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.stats
	s.SharedSendBytes = l.sharedSend.Load()
	s.SharedReceiveBytes = l.sharedReceive.Load()
	return s
}
