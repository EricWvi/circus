// Package wiretest provides a bounded in-memory duplex Conn with independent
// directional faults. It starts no goroutines; readers wait for packet delivery.
package wiretest

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/wire"
)

// Faults are captured at the start of each write.
type Faults struct {
	// Close immediately after N accepted writes, including silenced writes.
	// Already accepted packets remain readable before EOF. Zero disables it.
	DisconnectAfter int
	Silence         bool
	// BlockWrites simulates a peer/socket accepting no bytes. The in-progress
	// write waits for context cancellation or Close, never succeeds silently.
	BlockWrites    bool
	BytesPerSecond int64
	// FrameDelay is serialization/processing time per frame, independent of rate.
	FrameDelay time.Duration
	// Delay is propagation latency: packets can be in flight concurrently.
	// Increase BufferMessages to model a larger bandwidth-delay product.
	Delay     time.Duration
	Truncate  bool
	KeepBytes int
}
type Options struct {
	AtoB, BtoA    Faults
	Clock         clock.Clock
	MaxFrameBytes int
	// Default 16 packets per direction; bounds simulated in-flight packets.
	BufferMessages int
}
type packet struct {
	data  []byte
	ready time.Time
}
type shared struct {
	done chan struct{}
	once sync.Once
}
type Endpoint struct {
	shared             *shared
	incoming, outgoing chan packet
	clock              clock.Clock
	maxFrame           int
	readLimit          int64
	writeMu, mu        sync.Mutex
	faults             Faults
	writes             uint64
}

func Pipe(o Options) (*Endpoint, *Endpoint) {
	if o.Clock == nil {
		o.Clock = clock.Real{}
	}
	if o.MaxFrameBytes <= 0 {
		o.MaxFrameBytes = wire.DefaultMaxFragmentBytes + wire.FrameOverhead
	}
	if o.BufferMessages <= 0 {
		o.BufferMessages = 16
	}
	s := &shared{done: make(chan struct{})}
	ab, ba := make(chan packet, o.BufferMessages), make(chan packet, o.BufferMessages)
	return &Endpoint{shared: s, incoming: ba, outgoing: ab, clock: o.Clock, maxFrame: o.MaxFrameBytes, faults: o.AtoB},
		&Endpoint{shared: s, incoming: ab, outgoing: ba, clock: o.Clock, maxFrame: o.MaxFrameBytes, faults: o.BtoA}
}
func (e *Endpoint) SetFaults(f Faults) { e.mu.Lock(); e.faults = f; e.mu.Unlock() }
func (e *Endpoint) Writes() uint64     { e.mu.Lock(); defer e.mu.Unlock(); return e.writes }
func (e *Endpoint) ReadMessage(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var p packet
	// Drain accepted data before reporting EOF, even if Close raced with delivery.
	select {
	case p = <-e.incoming:
	default:
		select {
		case p = <-e.incoming:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-e.shared.done:
			select {
			case p = <-e.incoming:
			default:
				return nil, io.EOF
			}
		}
	}
	if delay := p.ready.Sub(e.clock.Now()); delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-e.shared.done:
		case <-e.clock.After(delay):
		}
	}
	e.mu.Lock()
	limit := e.readLimit
	e.mu.Unlock()
	if limit > 0 && int64(len(p.data)) > limit {
		return nil, fmt.Errorf("wiretest: read limit exceeded")
	}
	return p.data, nil
}
func (e *Endpoint) WriteMessage(ctx context.Context, data []byte) error {
	e.writeMu.Lock()
	defer e.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-e.shared.done:
		return io.EOF
	default:
	}
	if len(data) > e.maxFrame {
		return fmt.Errorf("wiretest: frame exceeds allocation limit")
	}
	e.mu.Lock()
	f, n := e.faults, e.writes
	e.mu.Unlock()
	if f.DisconnectAfter > 0 && n >= uint64(f.DisconnectAfter) {
		_ = e.Close()
		return io.EOF
	}
	if f.BlockWrites {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.shared.done:
			return io.EOF
		}
	}
	delay := f.FrameDelay
	if f.BytesPerSecond > 0 {
		seconds, remainder := int64(len(data))/f.BytesPerSecond, int64(len(data))%f.BytesPerSecond
		delay += time.Duration(seconds)*time.Second + time.Duration(float64(remainder)/float64(f.BytesPerSecond)*float64(time.Second))
	}
	if delay > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.shared.done:
			return io.EOF
		case <-e.clock.After(delay):
		}
	}
	if !f.Silence {
		size := len(data)
		if f.Truncate {
			size = min(size, max(0, f.KeepBytes))
		}
		copyData := make([]byte, size)
		copy(copyData, data[:size])
		p := packet{copyData, e.clock.Now().Add(f.Delay)}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.shared.done:
			return io.EOF
		case e.outgoing <- p:
		}
	}
	e.mu.Lock()
	e.writes++
	written := e.writes
	e.mu.Unlock()
	if f.DisconnectAfter > 0 && written >= uint64(f.DisconnectAfter) {
		_ = e.Close()
	}
	return nil
}
func (e *Endpoint) Close() error { e.shared.once.Do(func() { close(e.shared.done) }); return nil }

var _ wire.Conn = (*Endpoint)(nil)

func (e *Endpoint) SetReadLimit(n int64) { e.mu.Lock(); e.readLimit = n; e.mu.Unlock() }
func (e *Endpoint) Shutdown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		_ = e.Close()
		return err
	}
	return e.Close()
}
