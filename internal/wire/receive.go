package wire

import (
	"context"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
)

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
