package wire

import (
	"fmt"
	"sync"
)

// Budget bounds live, charged wire ownership across links in one process. It
// has two independent pools: sending never consumes receive capacity or the
// reverse. It is optional, must be shared by pointer, and must not be copied.
// The send pool covers unsent serialized bytes; the receive pool covers actual
// assembly blocks, flat buffers and decoded costs of frames too large for the
// per-link reserve. Declarations alone never consume either pool. See the
// package memory model.
type Budget struct {
	send, receive pool
}

// PoolStats reports one pool's limit, current charge and peak charge.
type PoolStats struct{ Limit, Used, Peak int }

// BudgetStats reports both pools.
type BudgetStats struct{ Send, Receive PoolStats }

// NewBudget creates a shared budget. Each limit must also cover one
// maximum-size message for every Options that uses it; New and
// OptionsFromWelcome reject smaller pools (see MinBudget).
func NewBudget(sendLimit, receiveLimit int) (*Budget, error) {
	if sendLimit <= 0 || receiveLimit <= 0 {
		return nil, fmt.Errorf("wire: budget limits must be positive")
	}
	b := &Budget{}
	b.send.init(sendLimit)
	b.receive.init(receiveLimit)
	return b, nil
}
func (b *Budget) Stats() BudgetStats {
	return BudgetStats{Send: b.send.stats(), Receive: b.receive.stats()}
}

// MinBudget returns the smallest send and receive pool limits that can carry
// one maximum-size message under o (after defaults). A smaller pool would make
// such a message impossible to send or receive, so New rejects it. Size real
// pools well above this: two in-flight messages on one link already need more.
func MinBudget(o Options) (send, receive int, err error) {
	o.Budget = nil
	if o, err = o.defaults(); err != nil {
		return 0, 0, err
	}
	send, receive = minBudget(o)
	return send, receive, nil
}

// A fragmented send briefly holds the marshaled buffer and its fragment copies.
// The receive peak is the flat input plus decoded cost plus transient decoder
// credit; the flattening phase (blocks plus flat buffer plus one fragment frame
// cost) never exceeds it because a fragment frame is at most F+FrameOverhead.
func minBudget(o Options) (send, receive int) {
	return 2 * o.MaxMessageBytes, 2*o.MaxMessageBytes + maxMessageCost(max(o.MaxMessageBytes, o.MaxFragmentBytes+FrameOverhead))
}

type pool struct {
	mu                sync.Mutex
	limit, used, peak int
	changed           chan struct{}
}

func (p *pool) init(limit int) { p.limit, p.changed = limit, make(chan struct{}) }
func (p *pool) stats() PoolStats {
	p.mu.Lock()
	defer p.mu.Unlock()
	return PoolStats{p.limit, p.used, p.peak}
}
func (p *pool) reserve(n int) (bool, <-chan struct{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n > p.limit-p.used {
		return false, p.changed
	}
	p.used += n
	p.peak = max(p.peak, p.used)
	return true, nil
}
func (p *pool) release(n int) {
	if n == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if n < 0 || n > p.used {
		panic("wire: invalid budget release")
	}
	p.used -= n
	close(p.changed)
	p.changed = make(chan struct{})
}
