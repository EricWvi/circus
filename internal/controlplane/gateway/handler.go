package gateway

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wirepolicy"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

// handler retains at most MaxConnections peers, including raw handshakes and
// closing generations. Each peer has one reader and one credential-check loop.
type handler struct {
	mu              sync.Mutex
	config          Config
	peers           map[string]*peer
	budget          *wire.Budget
	binding         bus.Registration
	closed          bool
	shutdownContext context.Context
	changed         chan struct{}
	registrations   uint64 // orders registry generations of local peers
}
type peer struct {
	id        string
	conn      wire.Conn
	link      *wire.Link
	info      Connection
	cancel    context.CancelFunc
	closeOnce sync.Once
	closing   chan struct{}
	done      chan struct{} // closed once ServeHTTP released the peer
	// registered is this peer's registration order (0 until registered).
	registered uint64
}

func newHandler(c Config) (Handler, error) {
	if c.InstanceID == "" || len(c.InstanceID) > 128 || c.Clock == nil || c.Auth == nil || c.Registry == nil || c.Bus == nil || c.Hooks == nil {
		return nil, fmt.Errorf("gateway: missing dependency")
	}
	c.InstanceID = strings.Clone(c.InstanceID)
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.MaxConnections == 0 {
		c.MaxConnections = 100
	}
	if c.MaxConnections < 1 || c.MaxConnections > 256 {
		return nil, fmt.Errorf("gateway: invalid connection limit")
	}
	if c.HelloTimeout == 0 {
		c.HelloTimeout = 10 * time.Second
	}
	if c.RecheckInterval == 0 {
		c.RecheckInterval = time.Second
	}
	if c.RecheckTimeout == 0 {
		c.RecheckTimeout = 5 * time.Second
	}
	if c.RecheckMaxStale == 0 {
		c.RecheckMaxStale = 30 * time.Second
	}
	if c.HelloTimeout <= 0 || c.RecheckInterval <= 0 || c.RecheckTimeout <= 0 || c.RecheckMaxStale <= 0 {
		return nil, fmt.Errorf("gateway: invalid duration")
	}
	if c.MinVersion == 0 {
		c.MinVersion = 1
	}
	if c.MaxVersion == 0 {
		c.MaxVersion = 1
	}
	if c.MinVersion != 1 || c.MaxVersion != 1 {
		return nil, fmt.Errorf("gateway: unsupported configured version")
	}
	if c.LinkOptions.Budget != nil {
		return nil, fmt.Errorf("gateway: budget must be gateway-owned")
	}
	c.LinkOptions.Clock = c.Clock
	opts, err := wire.FillWelcome(&circusv1.Welcome{}, c.LinkOptions)
	if err != nil {
		return nil, err
	}
	c.LinkOptions = opts
	send, receive, err := wire.MinBudget(opts)
	if err != nil {
		return nil, err
	}
	if c.SendPoolBytes == 0 {
		c.SendPoolBytes = 2 * send
	}
	if c.ReceivePoolBytes == 0 {
		c.ReceivePoolBytes = 2 * receive
	}
	if c.SendPoolBytes < 2*send || c.ReceivePoolBytes < 2*receive {
		return nil, fmt.Errorf("gateway: pools must each be at least twice MinBudget")
	}
	budget, err := wire.NewBudget(c.SendPoolBytes, c.ReceivePoolBytes)
	if err != nil {
		return nil, err
	}
	c.LinkOptions.Budget = budget
	h := &handler{config: c, peers: make(map[string]*peer), budget: budget, changed: make(chan struct{})}
	h.binding, err = c.Bus.Register(c.InstanceID, h)
	if err != nil {
		return nil, err
	}
	return h, nil
}
func (h *handler) signal() { close(h.changed); h.changed = make(chan struct{}) }

// deadline uses only the injected clock. Its helper is joined by cancel.
func (h *handler) deadline(ctx context.Context, d time.Duration) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	alarm := h.config.Clock.After(d)
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
		case <-alarm:
			cancel()
		}
	}()
	return ctx, func() { cancel(); <-done }
}
func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/link/daemon" {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	id := uuid.NewString()
	p := &peer{id: id, cancel: cancel, closing: make(chan struct{}), done: make(chan struct{})}
	h.mu.Lock()
	if h.closed || len(h.peers) >= h.config.MaxConnections {
		h.mu.Unlock()
		cancel()
		http.Error(w, "gateway overloaded", http.StatusServiceUnavailable)
		return
	}
	h.peers[id] = p
	h.signal()
	h.mu.Unlock()
	defer func() {
		cancel()
		h.mu.Lock()
		conn, link, info := p.conn, p.link, p.info
		h.mu.Unlock()
		if link != nil {
			_ = link.Close()
		} else if conn != nil {
			_ = conn.Close()
		}
		p.closeOnce.Do(func() { close(p.closing) })
		<-p.closing
		if info.Target.DeviceID != "" {
			// Router fences against a replacement that registered after this close.
			h.mu.Lock()
			hookContext := h.shutdownContext
			h.mu.Unlock()
			if hookContext == nil {
				hookContext = context.WithoutCancel(r.Context())
			}
			_ = h.config.Hooks.OnDisconnected(hookContext, info)
			_, _ = h.config.Registry.Unregister(context.WithoutCancel(r.Context()), info)
		}
		h.mu.Lock()
		delete(h.peers, id)
		h.signal()
		h.mu.Unlock()
		close(p.done)
	}()
	principal, err := h.config.Auth.Authenticate(ctx, r)
	if errors.Is(err, ErrWrongRole) {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if err != nil {
		status := http.StatusServiceUnavailable
		if errors.Is(err, ErrUnauthenticated) {
			status = http.StatusUnauthorized
		}
		if errors.Is(err, ErrRevoked) {
			status = http.StatusForbidden
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	if !validConnection(Connection{Target: bus.Target{InstanceID: h.config.InstanceID, ConnectionID: id, DeviceID: principal.DeviceID, BootID: "pending"}, UserID: principal.UserID, DeviceName: principal.DeviceName}) || principal.CredentialID == "" || len(principal.CredentialID) > 128 {
		http.Error(w, "invalid principal", http.StatusUnauthorized)
		return
	}
	principal.UserID = strings.Clone(principal.UserID)
	principal.DeviceID = strings.Clone(principal.DeviceID)
	principal.DeviceName = strings.Clone(principal.DeviceName)
	principal.CredentialID = strings.Clone(principal.CredentialID)
	conn, err := wire.AcceptWebSocket(w, r, nil)
	if err != nil {
		return
	}
	h.mu.Lock()
	p.conn = conn
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return
	}
	handshake, finish := h.deadline(ctx, h.config.HelloTimeout)
	data, err := conn.ReadMessage(handshake)
	if err != nil {
		finish()
		return
	}
	msg, err := wire.Decode(wire.DaemonUplink, data, wire.MaxMetadataBytes)
	reject := func(code circusv1.Reject_Code) {
		out := &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Reject{Reject: &circusv1.Reject{Code: code, MinVersion: h.config.MinVersion, MaxVersion: h.config.MaxVersion}}}
		encoded, _ := proto.Marshal(out)
		_ = conn.WriteMessage(handshake, encoded)
		_ = conn.Shutdown(handshake)
	}
	if err != nil {
		reject(circusv1.Reject_CODE_BAD_HELLO)
		finish()
		return
	}
	hello := msg.(*circusv1.DaemonUplink).GetHello()
	if hello == nil || hello.BootId == "" || len(hello.BootId) > 128 {
		reject(circusv1.Reject_CODE_BAD_HELLO)
		finish()
		return
	}
	if hello.ProtocolVersion < h.config.MinVersion || hello.ProtocolVersion > h.config.MaxVersion {
		reject(circusv1.Reject_CODE_VERSION_UNSUPPORTED)
		finish()
		return
	}
	welcome := &circusv1.Welcome{PrincipalId: principal.DeviceID, InstanceId: h.config.InstanceID, NegotiatedVersion: hello.ProtocolVersion, ServerTimeUnixMs: h.config.Clock.Now().UnixMilli()}
	opts, err := wire.FillWelcome(welcome, h.config.LinkOptions)
	if err != nil {
		finish()
		return
	}
	encoded, _ := proto.Marshal(&circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_Welcome{Welcome: welcome}})
	if err = conn.WriteMessage(handshake, encoded); err != nil {
		finish()
		return
	}
	finish()
	link, err := wire.New(conn, wire.DaemonControlSide, opts)
	if err != nil {
		return
	}
	info := Connection{Target: bus.Target{InstanceID: h.config.InstanceID, ConnectionID: id, DeviceID: principal.DeviceID, BootID: hello.BootId}, UserID: principal.UserID, DeviceName: principal.DeviceName}
	// One locked section: shutdown check, then registration under h.mu, so
	// registration order and the set of older local peers of this device are
	// consistent with the registry.
	h.mu.Lock()
	p.link = link
	p.info = info
	if h.closed {
		h.mu.Unlock()
		return
	}
	previous, err := h.config.Registry.Register(ctx, info)
	if err != nil {
		h.mu.Unlock()
		return
	}
	h.registrations++
	p.registered = h.registrations
	// Every older, not yet released peer of this device: the sync waits for
	// all of them, not only the direct predecessor, because a predecessor
	// that is itself superseded stops waiting for its own (review 2, M2).
	var older []*peer // at most MaxConnections
	for _, q := range h.peers {
		if q != p && q.registered != 0 && q.registered < p.registered && q.info.Target.DeviceID == info.Target.DeviceID {
			older = append(older, q)
		}
	}
	// Wait in registration order (deterministic; any order is correct).
	slices.SortFunc(older, func(a, b *peer) int { return cmp.Compare(a.registered, b.registered) })
	var predecessor *peer
	if previous != nil {
		predecessor = h.peers[previous.Target.ConnectionID]
	}
	h.mu.Unlock()
	if previous != nil {
		slog.Warn("daemon connection superseded", "instance_id", h.config.InstanceID, "device_id", principal.DeviceID, "boot_changed", previous.Target.BootID != hello.BootId)
		if predecessor != nil {
			h.requestClose(predecessor, circusv1.GoAway_REASON_SUPERSEDED)
		}
	}
	checksDone := make(chan struct{})
	go func() { defer close(checksDone); h.recheck(ctx, p, principal) }()
	defer func() { cancel(); <-checksDone }()
	syncDone := make(chan struct{})
	go func() {
		defer close(syncDone)
		// Superseded links drain their final results (D43 item 12) before
		// this generation's sync may lose them to a boot change. Each drain
		// is bounded by its shutdown deadline and MaxConsumerStall, and the
		// chain by MaxConnections.
		for _, q := range older {
			select {
			case <-q.done:
			case <-ctx.Done():
				return
			}
		}
		if err := h.config.Hooks.OnConnected(ctx, info); err != nil {
			_ = link.Close()
		}
	}()
	defer func() { cancel(); _ = link.Close(); <-syncDone }()
	for {
		m, err := link.Receive(ctx)
		if err != nil {
			return
		}
		u := m.(*circusv1.DaemonUplink)
		switch v := u.Payload.(type) {
		case *circusv1.DaemonUplink_CallAck:
			err = h.config.Hooks.OnCallAck(ctx, info, v.CallAck)
		case *circusv1.DaemonUplink_Result:
			err = h.config.Hooks.OnDeviceResult(ctx, info, v.Result)
		case *circusv1.DaemonUplink_CallStatus:
			err = h.config.Hooks.OnCallStatus(ctx, info, v.CallStatus)
		case *circusv1.DaemonUplink_JobEvent:
			err = h.config.Hooks.OnJobEvent(ctx, info, v.JobEvent)
		case *circusv1.DaemonUplink_JobSnapshot:
			err = h.config.Hooks.OnJobSnapshot(ctx, info, v.JobSnapshot)
		case *circusv1.DaemonUplink_Heartbeat:
		default:
			err = fmt.Errorf("gateway: unexpected business message")
		}
		// Persistence failures preserve the reader so results can still be delivered.
		var protocol *wire.ProtocolError
		if errors.As(err, &protocol) {
			return
		}
	}
}
func (h *handler) recheck(ctx context.Context, p *peer, principal Principal) {
	lastValid := h.config.Clock.Now()
	// Randomize only the phase, preserving a worst-case definitive-revoke
	// observation of one interval plus one timeout while desynchronizing links.
	delay := time.Duration(rand.Float64() * float64(h.config.RecheckInterval))
	for {
		alarm := h.config.Clock.After(delay)
		select {
		case <-ctx.Done():
			return
		case <-alarm:
		}
		check, finish := h.deadline(ctx, h.config.RecheckTimeout)
		err := h.config.Auth.Recheck(check, principal)
		timedOut := check.Err() != nil
		finish()
		if ctx.Err() != nil {
			return
		}
		if err == nil && !timedOut {
			lastValid = h.config.Clock.Now()
		} else {
			definitive := !timedOut && errors.Is(err, ErrRevoked)
			if definitive || h.config.Clock.Now().Sub(lastValid) >= h.config.RecheckMaxStale {
				h.config.Logger.WarnContext(ctx, "daemon credential revalidation closed link", "device_id", principal.DeviceID, "definitive_revoke", definitive, "last_valid", lastValid, "error", err)
				h.mu.Lock()
				link := p.link
				h.mu.Unlock()
				if link != nil {
					_ = link.Close()
				}
				return
			}
		}
		delay = h.config.RecheckInterval
	}
}
func (h *handler) requestClose(p *peer, reason circusv1.GoAway_Reason) {
	p.closeOnce.Do(func() {
		go func() {
			defer close(p.closing)
			ctx, finish := h.deadline(context.Background(), h.config.LinkOptions.HeartbeatTimeout)
			defer finish()
			h.mu.Lock()
			link, conn := p.link, p.conn
			h.mu.Unlock()
			if link != nil {
				m := &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_GoAway{GoAway: &circusv1.GoAway{Reason: reason}}}
				class, key, _ := wirepolicy.Classify(m)
				_ = link.Send(ctx, m, class, key)
				_ = link.Shutdown(ctx)
			} else {
				p.cancel()
				if conn != nil {
					_ = conn.Close()
				}
			}
		}()
	})
}
func (h *handler) Deliver(ctx context.Context, t bus.Target, m *circusv1.DaemonDownlink) error {
	route, ok, err := h.config.Registry.Lookup(ctx, t.DeviceID)
	if err != nil {
		return err
	}
	if !ok || route.Target != t {
		return bus.ErrNoRoute
	}
	h.mu.Lock()
	p := h.peers[t.ConnectionID]
	var link *wire.Link
	if p != nil {
		link = p.link
	}
	h.mu.Unlock()
	if link == nil {
		return bus.ErrNoRoute
	}
	class, key, err := wirepolicy.Classify(m)
	if err != nil {
		return err
	}
	return link.Send(ctx, m, class, key)
}
func (h *handler) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.closed = true
	h.shutdownContext = ctx
	peers := make([]*peer, 0, len(h.peers))
	for _, p := range h.peers {
		peers = append(peers, p)
	}
	h.mu.Unlock()
	for _, p := range peers {
		h.requestClose(p, circusv1.GoAway_REASON_SHUTTING_DOWN)
	}
	var failure error
	for {
		h.mu.Lock()
		n, changed := len(h.peers), h.changed
		h.mu.Unlock()
		if n == 0 {
			break
		}
		if failure != nil {
			<-changed
			continue
		}
		select {
		case <-changed:
		case <-ctx.Done():
			failure = ctx.Err()
			for _, p := range peers {
				p.cancel()
				h.mu.Lock()
				conn := p.conn
				h.mu.Unlock()
				if conn != nil {
					_ = conn.Close()
				}
			}
		}
	}
	for _, p := range peers {
		<-p.closing
	}
	err := h.binding.Close(context.WithoutCancel(ctx))
	if failure != nil {
		return failure
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
func (h *handler) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := Stats{Connections: len(h.peers), Registered: h.config.Registry.Stats().Connections, Budget: h.budget.Stats()}
	for _, p := range h.peers {
		if p.link == nil {
			s.Handshakes++
			continue
		}
		v := p.link.Stats()
		s.Wire.QueuedBytes += v.QueuedBytes
		s.Wire.QueuedMessages += v.QueuedMessages
		s.Wire.ReceivedBytes += v.ReceivedBytes
		s.Wire.ReceivedMessages += v.ReceivedMessages
		s.Wire.ReassemblyBytes += v.ReassemblyBytes
		s.Wire.ReassemblyAllocatedBytes += v.ReassemblyAllocatedBytes
		s.Wire.ReassemblyMessages += v.ReassemblyMessages
		s.Wire.ReassemblyBlocks += v.ReassemblyBlocks
		s.Wire.ActiveMessages += v.ActiveMessages
		s.Wire.SharedSendBytes += v.SharedSendBytes
		s.Wire.SharedReceiveBytes += v.SharedReceiveBytes
		s.Wire.LinkReserveBytes += v.LinkReserveBytes
		s.Wire.FramesRead += v.FramesRead
		s.Wire.FramesWritten += v.FramesWritten
		s.Wire.HeartbeatsRead += v.HeartbeatsRead
		s.Wire.HeartbeatsWritten += v.HeartbeatsWritten
		s.Wire.ReadBlocked = s.Wire.ReadBlocked || v.ReadBlocked
	}
	return s
}
