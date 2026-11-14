package wire

import (
	"context"
	"errors"
	"fmt"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
)

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

func (l *Link) Err() error { l.mu.Lock(); defer l.mu.Unlock(); return l.err }

func shutdownTimeout(err error) error { return fmt.Errorf("%w: %w", ErrShutdownTimeout, err) }

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
