package wire

import (
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"time"
)

type partial struct {
	blocks            [][]byte
	size, total, next int
	touched           time.Time
	transferred       bool
}
type reassembler struct {
	reserve          func(int) error
	releaseBytes     func(int)
	opts             Options
	high             uint64
	bytes, allocated int
	messages         map[uint64]*partial
}

func newReassembler(o Options) *reassembler {
	return &reassembler{opts: o, messages: make(map[uint64]*partial)}
}
func (r *reassembler) release(id uint64, p *partial) {
	r.bytes -= p.total
	for _, block := range p.blocks {
		r.allocated -= cap(block)
		if !p.transferred && r.releaseBytes != nil {
			r.releaseBytes(cap(block))
		}
	}
	delete(r.messages, id)
}
func (r *reassembler) clear() {
	for id, p := range r.messages {
		r.release(id, p)
	}
}
func (r *reassembler) add(f *circusv1.Fragment, now time.Time) ([]byte, error) {
	if f.MessageId == 0 || len(f.Data) == 0 || len(f.Data) > r.opts.MaxFragmentBytes {
		return nil, protocol("invalid fragment ID or data size")
	}
	p := r.messages[f.MessageId]
	if p == nil {
		if f.Index != 0 || f.MessageId <= r.high || f.TotalSize == 0 || f.TotalSize > uint64(r.opts.MaxMessageBytes) {
			return nil, protocol("invalid first fragment")
		}
		if len(r.messages) >= r.opts.MaxInFlightMessages || f.TotalSize > uint64(r.opts.MaxInFlightBytes-r.bytes) {
			return nil, protocol("reassembly budget exceeded")
		}
		p = &partial{total: int(f.TotalSize)}
	} else if f.TotalSize != 0 {
		return nil, protocol("total_size on continuation")
	}
	if int(f.Index) != p.next || len(f.Data) > p.total-p.size {
		return nil, protocol("out-of-order or oversized fragment")
	}
	if f.Last != (p.size+len(f.Data) == p.total) {
		return nil, protocol("missing or premature last fragment")
	}
	if p.next == 0 {
		r.messages[f.MessageId] = p
		r.bytes += p.total
		r.high = f.MessageId
	}
	// Fixed blocks bound each allocation and avoid quadratic copying. Tiny first
	// fragments reserve the declaration but allocate at most one 64 KiB block.
	p.touched = now
	data := f.Data
	blockSize := 64 << 10
	for len(data) > 0 {
		if len(p.blocks) == 0 || len(p.blocks[len(p.blocks)-1]) == cap(p.blocks[len(p.blocks)-1]) {
			capacity := min(blockSize, p.total-p.size)
			if r.reserve != nil {
				if err := r.reserve(capacity); err != nil {
					return nil, err
				}
			}
			p.blocks = append(p.blocks, make([]byte, 0, capacity))
			r.allocated += capacity
		}
		i := len(p.blocks) - 1
		n := min(len(data), cap(p.blocks[i])-len(p.blocks[i]))
		p.blocks[i] = append(p.blocks[i], data[:n]...)
		p.size += n
		data = data[n:]
	}
	p.next++
	if !f.Last {
		return nil, nil
	}
	var complete []byte
	if len(p.blocks) == 1 {
		complete = p.blocks[0]
		p.transferred = true
	} else {
		if r.reserve != nil {
			if err := r.reserve(p.total); err != nil {
				return nil, err
			}
		}
		complete = make([]byte, 0, p.total)
		for _, block := range p.blocks {
			complete = append(complete, block...)
		}
	}
	r.release(f.MessageId, p)
	return complete, nil
}
func (r *reassembler) deadline() time.Time {
	var next time.Time
	for _, p := range r.messages {
		at := p.touched.Add(r.opts.FragmentTimeout)
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	return next
}
func (r *reassembler) pause(d time.Duration) {
	for _, p := range r.messages {
		p.touched = p.touched.Add(d)
	}
}

// Block count bounds metadata as well as data when fragments are tiny.
func (r *reassembler) blocks() int {
	count := 0
	for _, p := range r.messages {
		count += len(p.blocks)
	}
	return count
}
