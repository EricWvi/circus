package wire

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"google.golang.org/protobuf/proto"
)

// Send serializes one bounded message, scheduled by its class and ordering
// key (see Class). Accepted sends survive caller context cancellation. Queue
// overflow aborts; local size rejection leaves Link usable.
func (l *Link) Send(ctx context.Context, m proto.Message) error {
	class, key, err := classify(m)
	if err != nil {
		return err
	}
	return l.send(ctx, m, class, key)
}

func (l *Link) send(ctx context.Context, m proto.Message, class Class, orderingKey string) error {
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
