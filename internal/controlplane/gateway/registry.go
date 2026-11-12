package gateway

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// registry owns at most limit immutable connection records, each six bounded
// strings; it has no socket, callback queue or background work.
type registry struct {
	mu      sync.Mutex
	limit   int
	entries map[string]Connection
}

func validConnection(c Connection) bool {
	for _, s := range []string{c.Target.InstanceID, c.Target.ConnectionID, c.Target.DeviceID, c.Target.BootID, c.UserID, c.DeviceName} {
		if s == "" || len(s) > 128 {
			return false
		}
	}
	return true
}
func cloneConnection(c Connection) Connection {
	c.Target.InstanceID = strings.Clone(c.Target.InstanceID)
	c.Target.ConnectionID = strings.Clone(c.Target.ConnectionID)
	c.Target.DeviceID = strings.Clone(c.Target.DeviceID)
	c.Target.BootID = strings.Clone(c.Target.BootID)
	c.UserID = strings.Clone(c.UserID)
	c.DeviceName = strings.Clone(c.DeviceName)
	return c
}
func (r *registry) Register(ctx context.Context, c Connection) (*Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validConnection(c) {
		return nil, fmt.Errorf("gateway: invalid connection identity")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.entries[c.Target.DeviceID]
	if !ok && len(r.entries) >= r.limit {
		return nil, fmt.Errorf("gateway: registry full")
	}
	c = cloneConnection(c)
	c.Ready = false
	r.entries[c.Target.DeviceID] = c
	if ok {
		return &old, nil
	}
	return nil, nil
}
func (r *registry) Activate(ctx context.Context, c Connection) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.entries[c.Target.DeviceID]
	if !ok || old.Target != c.Target {
		return false, nil
	}
	old.Ready = true
	r.entries[c.Target.DeviceID] = old
	return true, nil
}
func (r *registry) Unregister(ctx context.Context, c Connection) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old, ok := r.entries[c.Target.DeviceID]
	if !ok || old.Target != c.Target {
		return false, nil
	}
	delete(r.entries, c.Target.DeviceID)
	return true, nil
}
func (r *registry) Lookup(ctx context.Context, id string) (Connection, bool, error) {
	if err := ctx.Err(); err != nil {
		return Connection{}, false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.entries[id]
	return v, ok, nil
}
func (r *registry) ListOnline(ctx context.Context, user string) ([]Connection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Connection, 0, len(r.entries))
	for _, v := range r.entries {
		if v.UserID == user {
			out = append(out, v)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DeviceName < out[j].DeviceName })
	return out, nil
}
func (r *registry) Stats() RegistryStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RegistryStats{Connections: len(r.entries)}
}
