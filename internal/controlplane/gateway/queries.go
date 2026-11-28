package gateway

import (
	"context"
	"errors"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/google/uuid"
)

var (
	ErrDeviceOffline      = errors.New("device is offline")
	ErrFeatureUnsupported = errors.New("daemon did not advertise the required feature")
	ErrQueryUnsupported   = errors.New("the device's circus is too old to answer this query, please update it")
	ErrQueryLinkClosed    = errors.New("device link closed before answering")
	ErrQueryBusy          = errors.New("too many queries in flight for the device")
	errQueryResult        = errors.New("device query result has no outcome")
	errQueryResultKind    = errors.New("device query result has the wrong query kind")
)

const (
	maxLinkQueries = 16
	queryTimeout   = 10 * time.Second
)

type pendingQuery struct {
	ctx      context.Context
	deadline time.Time
	feature  string
	done     chan queryReply
}

type queryReply struct {
	result *circusv1.DeviceQueryResult
	err    error
}

// closeQueries is called under h.mu at replacement or as the peer closes.
func (p *peer) closeQueries() {
	p.queriesClosed = true
	for _, query := range p.queries {
		query.done <- queryReply{err: ErrQueryLinkClosed}
	}
	clear(p.queries)
}

func (h *handler) UpdateMachine(ctx context.Context, deviceID string, u *circusv1.UpdateMachine) error {
	h.mu.Lock()
	route, ok, err := h.config.Registry.Lookup(ctx, deviceID)
	if err != nil || !ok || !route.Ready {
		h.mu.Unlock()
		return ErrDeviceOffline
	}
	p := h.peers[route.Target.ConnectionID]
	if h.closed || p == nil || p.queriesClosed || p.link.Err() != nil {
		h.mu.Unlock()
		return ErrDeviceOffline
	}
	if _, ok := p.features[wire.FeatureMachineUpdate]; !ok {
		h.mu.Unlock()
		return ErrFeatureUnsupported
	}
	link := p.link
	h.mu.Unlock()
	return link.Send(ctx, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_UpdateMachine{UpdateMachine: u}})
}

func (h *handler) Supports(deviceID, feature string) (online, supported bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	route, ok, err := h.config.Registry.Lookup(context.Background(), deviceID)
	if err != nil || !ok || !route.Ready {
		return false, false
	}
	p := h.peers[route.Target.ConnectionID]
	if h.closed || p == nil || p.queriesClosed || p.link.Err() != nil {
		return false, false
	}
	_, supported = p.features[feature]
	return true, supported
}

func (h *handler) Query(ctx context.Context, deviceID string, q *circusv1.DeviceQuery) (*circusv1.DeviceQueryResult, error) {
	feature := wire.DeviceQueryFeature(q)
	if feature == "" {
		return nil, ErrQueryUnsupported
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	requestID := id.String()
	h.mu.Lock()
	route, ok, err := h.config.Registry.Lookup(ctx, deviceID)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	p := h.peers[route.Target.ConnectionID]
	if !ok || p == nil || p.queriesClosed || p.link.Err() != nil {
		h.mu.Unlock()
		return nil, ErrDeviceOffline
	}
	if _, supported := p.features[feature]; !supported {
		h.mu.Unlock()
		return nil, ErrQueryUnsupported
	}
	if len(p.queries) >= maxLinkQueries {
		h.mu.Unlock()
		return nil, ErrQueryBusy
	}
	deadline := h.config.Clock.Now().Add(queryTimeout)
	queryCtx, cancel := clock.WithTimeout(ctx, h.config.Clock, queryTimeout)
	pending := pendingQuery{ctx: queryCtx, deadline: deadline, feature: feature, done: make(chan queryReply, 1)}
	p.queries[requestID] = pending
	h.mu.Unlock()
	defer cancel()
	defer func() {
		h.mu.Lock()
		delete(p.queries, requestID)
		h.mu.Unlock()
	}()

	q = &circusv1.DeviceQuery{RequestId: requestID, Query: q.Query}
	m := &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_DeviceQuery{DeviceQuery: q}}
	contextError := func() error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return context.DeadlineExceeded
	}
	if err := p.link.Send(queryCtx, m); err != nil {
		switch {
		case queryCtx.Err() != nil:
			return nil, contextError()
		case errors.Is(err, wire.ErrBusy):
			return nil, ErrQueryBusy
		case errors.Is(err, wire.ErrTooLarge):
			return nil, err
		default:
			return nil, ErrQueryLinkClosed
		}
	}

	select {
	case reply := <-pending.done:
		return reply.result, reply.err
	case <-p.link.Done():
		select {
		case reply := <-pending.done:
			return reply.result, reply.err
		default:
			return nil, ErrQueryLinkClosed
		}
	case <-queryCtx.Done():
		return nil, contextError()
	}
}

func (h *handler) resolveQuery(p *peer, result *circusv1.DeviceQueryResult) {
	id := result.GetRequestId()
	h.mu.Lock()
	pending, ok := p.queries[id]
	if ok {
		delete(p.queries, id)
	}
	if ok && pending.ctx.Err() == nil && h.config.Clock.Now().Before(pending.deadline) && p.link.Err() == nil {
		reply := queryReply{result: result}
		switch {
		case result.GetError() != nil:
		case result.GetListDirectory() == nil && result.GetGitRemote() == nil:
			reply = queryReply{err: errQueryResult}
		case pending.feature == wire.FeatureListDirectory && result.GetGitRemote() != nil,
			pending.feature == wire.FeatureGitRemote && result.GetListDirectory() != nil:
			reply = queryReply{err: errQueryResultKind}
		}
		pending.done <- reply
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	h.config.Logger.Debug("ignoring unmatched device query result", "connection_id", p.id, "request_id", id)
}
