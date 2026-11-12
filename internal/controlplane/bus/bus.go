// Package bus defines bounded, instance-addressed delivery of daemon commands.
// M2 uses synchronous in-process delivery; it has no outbox or retry queue.
package bus

import (
	"context"
	"errors"
	"fmt"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"strings"
	"sync"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

var (
	// ErrBusy means admission failed without delivery; callers may retry safely.
	ErrBusy = errors.New("bus: capacity exhausted")
	// ErrNoRoute means the instance or exact connection generation is absent.
	ErrNoRoute = errors.New("bus: route unavailable")
)

// Target fences delivery to one connection and boot on one control-plane instance.
// Each field is nonempty and at most 128 bytes; retained strings are copied.
type Target struct {
	InstanceID, ConnectionID, DeviceID, BootID string
}

// Receiver consumes a command synchronously. It must reject stale targets with
// ErrNoRoute and use wirepolicy.Classify for every subsequent wire.Link.Send.
// Delivery success means queued, not written, acknowledged or executed.
type Receiver interface {
	Deliver(context.Context, Target, *circusv1.DaemonDownlink) error
}

// Registration owns one receiver binding. Close is idempotent and removes only
// this generation, then waits for its admitted deliveries to finish within ctx.
type Registration interface {
	Close(context.Context) error
}

// Bus routes commands without persisting content. Methods are concurrency-safe.
type Bus interface {
	// Register binds an instance once; duplicate bindings are refused.
	Register(instanceID string, receiver Receiver) (Registration, error)
	// Send borrows an immutable message until return. It preserves errors from
	// Receiver, including wire.ErrBusy; it never retries or spawns delivery work.
	Send(context.Context, Target, *circusv1.DaemonDownlink) error
	// Stats reports actual retained bindings and admitted deliveries.
	Stats() Stats
}

// Limits bounds all retained state: MaxInstances receiver bindings, MaxInFlight
// borrowed messages and MaxBytes total charged message memory. IDs cost at most
// 512 bytes per target; message charges include decoded objects, not just Size.
// All limits must be positive; messages also obey the wire schema/size limits.
type Limits struct {
	MaxInstances, MaxInFlight int
	MaxBytes                  int64
}

// Stats is a constant-size snapshot; all gauges reach zero after unbinding.
type Stats struct {
	Bindings, InFlight int
	Bytes              int64
}

// New constructs an in-process Bus with no goroutines or unbounded queues.
// Exhausted admission returns ErrBusy before retaining the borrowed message.
func New(limits Limits) (Bus, error) {
	if limits.MaxInstances < 1 || limits.MaxInFlight < 1 || limits.MaxBytes < 1 {
		return nil, fmt.Errorf("bus: invalid limits")
	}
	return &local{limits: limits, bindings: make(map[string]*binding)}, nil
}

// local retains at most Limits.MaxInstances bindings and Limits.MaxInFlight
// borrowed commands. Waiters are caller-owned; no delivery goroutines exist.
type local struct {
	mu       sync.Mutex
	limits   Limits
	bindings map[string]*binding
	stats    Stats
}
type binding struct {
	bus      *local
	id       string
	receiver Receiver
	active   int
	closing  bool
	done     chan struct{}
}

func (b *local) Register(id string, r Receiver) (Registration, error) {
	if id == "" || len(id) > 128 || r == nil {
		return nil, fmt.Errorf("bus: invalid binding")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.bindings[id] != nil || len(b.bindings) >= b.limits.MaxInstances {
		return nil, ErrBusy
	}
	reg := &binding{bus: b, id: strings.Clone(id), receiver: r, done: make(chan struct{})}
	b.bindings[reg.id] = reg
	b.stats.Bindings++
	return reg, nil
}
func (b *local) Send(ctx context.Context, t Target, m *circusv1.DaemonDownlink) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, v := range []string{t.InstanceID, t.ConnectionID, t.DeviceID, t.BootID} {
		if v == "" || len(v) > 128 {
			return ErrNoRoute
		}
	}
	cost, err := Charge(m, int(min(b.limits.MaxBytes, int64(^uint(0)>>1))))
	if err != nil {
		return err
	}
	b.mu.Lock()
	reg := b.bindings[t.InstanceID]
	if reg == nil || reg.closing {
		b.mu.Unlock()
		return ErrNoRoute
	}
	if b.stats.InFlight >= b.limits.MaxInFlight || cost > b.limits.MaxBytes-b.stats.Bytes {
		b.mu.Unlock()
		return ErrBusy
	}
	reg.active++
	b.stats.InFlight++
	b.stats.Bytes += cost
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		reg.active--
		b.stats.InFlight--
		b.stats.Bytes -= cost
		if reg.closing && reg.active == 0 {
			close(reg.done)
		}
	}()
	return reg.receiver.Deliver(ctx, t, m)
}
func (b *local) Stats() Stats { b.mu.Lock(); defer b.mu.Unlock(); return b.stats }
func (r *binding) Close(ctx context.Context) error {
	b := r.bus
	b.mu.Lock()
	if !r.closing {
		r.closing = true
		delete(b.bindings, r.id)
		b.stats.Bindings--
		if r.active == 0 {
			close(r.done)
		}
	}
	b.mu.Unlock()
	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Charge validates protobuf tree bounds before Size or cloning, then estimates
// live decoded ownership as 2*encoded + 256*nodes + 64*fields + 4096 bytes.
// Its recursive walk has depth <=64 and retains no message after return.
// Wire callers still use wire's pre-Unmarshal scanner for hostile encoded data.
func Charge(m proto.Message, maxBytes int) (int64, error) {
	if m == nil || !m.ProtoReflect().IsValid() || maxBytes < 1 {
		return 0, fmt.Errorf("bus: invalid message")
	}
	nodes, fields := int64(0), int64(0)
	payload := int64(0)
	var walk func(protoreflect.Message, int) error
	walk = func(msg protoreflect.Message, depth int) error {
		nodes++
		if depth > 64 || nodes > wire.MaxDecodeNodes {
			return fmt.Errorf("bus: message tree too deep or wide")
		}
		payload += int64(len(msg.GetUnknown()))
		var failure error
		msg.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			count := 1
			if f.IsList() {
				count = v.List().Len()
			}
			fields += int64(count)
			limit := 65536
			switch f.Name() {
			case "content":
				limit = 128
			case "working_dirs", "online_devices":
				limit = 256
			case "jobs":
				limit = wire.MaxSnapshotJobs
			}
			if count > limit || fields > wire.MaxDecodeFields || f.IsMap() {
				failure = fmt.Errorf("bus: repeated field limit")
				return false
			}
			visit := func(v protoreflect.Value) error {
				switch f.Kind() {
				case protoreflect.MessageKind:
					return walk(v.Message(), depth+1)
				case protoreflect.StringKind:
					payload += int64(len(v.String()))
				case protoreflect.BytesKind:
					payload += int64(len(v.Bytes()))
				}
				return nil
			}
			if f.IsList() {
				for i := 0; i < count; i++ {
					if failure = visit(v.List().Get(i)); failure != nil {
						break
					}
				}
			} else {
				failure = visit(v)
			}
			if payload > int64(maxBytes) {
				failure = wire.ErrTooLarge
			}
			return failure == nil
		})
		return failure
	}
	if err := walk(m.ProtoReflect(), 0); err != nil {
		return 0, err
	}
	n := proto.Size(m)
	if n > maxBytes {
		return 0, wire.ErrTooLarge
	}
	return 2*int64(n) + 256*nodes + 64*fields + 4096, nil
}

// CloneOwned copies a message whose tree has already passed Charge, including
// string backing storage. proto.Clone alone can retain an arbitrarily large
// caller-owned backing string through a tiny substring. The bounded walk owns
// only the cloned protobuf tree and at most 64 stack frames.
func CloneOwned(m proto.Message) proto.Message {
	out := proto.Clone(m)
	var detach func(protoreflect.Message)
	detach = func(msg protoreflect.Message) {
		msg.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
			if f.IsList() {
				list := v.List()
				for i := 0; i < list.Len(); i++ {
					if f.Kind() == protoreflect.StringKind {
						list.Set(i, protoreflect.ValueOfString(strings.Clone(list.Get(i).String())))
					} else if f.Kind() == protoreflect.MessageKind {
						detach(list.Get(i).Message())
					}
				}
			} else if f.Kind() == protoreflect.StringKind {
				msg.Set(f, protoreflect.ValueOfString(strings.Clone(v.String())))
			} else if f.Kind() == protoreflect.MessageKind {
				detach(v.Message())
			}
			return true
		})
	}
	detach(out.ProtoReflect())
	return out
}
