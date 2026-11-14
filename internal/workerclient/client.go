// Package workerclient implements a single authenticated worker connection.
// It never reconnects: transport loss returns an error; open a new connection
// and Query the call ID. Credentials must have the worker role. TLS roots,
// loopback plaintext restrictions and redirect refusal come from loopback.Link,
// shared with daemon/link.
//
// Every Submit, Query and Cancel is one operation with its own request ID
// (a per-connection counter from 1); updates are routed by that ID, so several
// operations may name the same call (a worker can Query or Cancel a call it
// is watching). At most 64 operations are open at once (the server's
// per-connection cap), eight updates each, and 256 MiB decoded queued updates
// are retained. Overflow aborts the link. One reader borrows one wire message;
// callers own returned updates. Every stream must be closed by its caller.
// Closing an operation that is still open sends Unwatch and frees its slot at
// once; it never cancels the call. Updates that arrive for an operation no
// longer open are dropped.
package workerclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/loopback"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
)

const maxOperations = 64

// ErrSubmitSent marks a transport failure after submission was attempted. The
// call may exist and run; the caller must re-query the same ID, never assume it
// is safe to retry under a new ID.
var ErrSubmitSent = errors.New("workerclient: submit was sent; outcome unknown")

// Options configures one dial. CAFile optionally overrides the credential file.
type Options struct {
	Credentials           credfile.File
	CAFile, ClientVersion string
	Wire                  wire.Options
}

// Client owns one link and a joined reader. Close is required even after errors.
type Client struct {
	link *wire.Link
	http *http.Client
	// sendMu orders slot changes with their uplink messages: a slot freed by
	// Close is reusable only after its Unwatch is queued, so the server always
	// sees the Unwatch before the operation that reuses the slot.
	sendMu     sync.Mutex
	mu         sync.Mutex
	ops        map[uint64]*Updates // open operations, by request ID
	nextID     uint64
	bytes      int64
	err        error
	done       chan struct{}
	maxMessage int
}
type queued struct {
	update *circusv1.CallUpdate
	bytes  int64
}

// Updates is the bounded stream of one operation. Closing it detaches locally
// and, if the operation is still open, sends Unwatch; it never cancels a call.
type Updates struct {
	client     *Client
	id         uint64
	queue      chan queued
	closed     chan struct{}
	oneShot    bool // Query or Cancel: exactly one update
	finished   bool // the last update was consumed by Next
	closedOnce bool
}

// Dial connects, authenticates and exchanges Hello/Welcome within ten seconds in
// total, then starts the reader. Every failure, including HTTP 401/403 and
// rejections, is returned to the caller.
func Dial(ctx context.Context, o Options) (*Client, error) {
	f := o.Credentials
	if f.Role != credfile.RoleWorker || f.Token == "" || f.PrincipalID == "" {
		return nil, errors.New("workerclient: worker credentials required")
	}
	if o.CAFile != "" {
		f.CAFile = o.CAFile
	}
	endpoint, h, err := loopback.Link(f.ControlPlaneURL, f.CAFile, "/v1/link/worker")
	if err != nil {
		return nil, fmt.Errorf("worker link: %w", err)
	}
	handshake, cancel := context.WithTimeout(ctx, wire.HelloTimeout)
	defer cancel()
	conn, resp, err := wire.DialWebSocket(handshake, endpoint, &websocket.DialOptions{HTTPClient: h, HTTPHeader: http.Header{"Authorization": []string{"Bearer " + f.Token}}})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		h.CloseIdleConnections()
		if resp != nil {
			if resp.StatusCode == http.StatusServiceUnavailable {
				return nil, fmt.Errorf("workerclient: HTTP 503: server capacity is full or authentication is temporarily unavailable; retry later: %w", err)
			}
			return nil, fmt.Errorf("workerclient: upgrade HTTP %d: %w", resp.StatusCode, err)
		}
		return nil, err
	}
	fail := func(err error) (*Client, error) { _ = conn.Close(); h.CloseIdleConnections(); return nil, err }
	hello := &circusv1.Hello{ProtocolVersion: wire.ProtocolVersion, ClientVersion: o.ClientVersion}
	_, opts, err := wire.DialHello(handshake, conn, wire.WorkerUplink, hello, f.PrincipalID, o.Wire)
	if err != nil {
		return fail(fmt.Errorf("workerclient: %w", err))
	}
	l, err := wire.New(conn, wire.WorkerSide, opts)
	if err != nil {
		return fail(err)
	}
	c := &Client{link: l, http: h, ops: make(map[uint64]*Updates), done: make(chan struct{}), maxMessage: opts.MaxMessageBytes}
	go c.read()
	return c, nil
}
func (c *Client) read() {
	defer close(c.done)
	for {
		m, err := c.link.Receive(context.Background())
		if err != nil {
			c.fail(err)
			return
		}
		d := m.(*circusv1.WorkerDownlink)
		u := d.GetCallUpdate()
		if u == nil {
			if d.GetHeartbeat() != nil {
				continue
			}
			c.fail(errors.New("workerclient: unexpected message"))
			return
		}
		cost, err := wire.Charge(u, c.maxMessage)
		if err == nil {
			err = c.enqueue(d.GetRequestId(), u, cost)
		}
		if err != nil {
			c.fail(err)
			return
		}
	}
}

// enqueue retains one update or reports the bounded-queue failure that read
// must use to abort the whole link. Updates for an operation that is not open
// (unwatched, answered or never opened) are harmless and dropped. The last
// update of an operation (a terminal update, or any one-shot reply) ends it
// here, because the server freed its slot before sending it.
func (c *Client) enqueue(id uint64, u *circusv1.CallUpdate, cost int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.ops[id]
	if s == nil {
		return nil
	}
	if s.oneShot || wire.Terminal(u) {
		delete(c.ops, id)
	}
	if cost > 256<<20-c.bytes {
		return errors.New("workerclient: update budget exhausted")
	}
	select {
	case s.queue <- queued{u, cost}:
		c.bytes += cost
		return nil
	default:
		return errors.New("workerclient: update queue full")
	}
}
func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
	_ = c.link.Close()
}
func (c *Client) send(ctx context.Context, m *circusv1.WorkerUplink) error {
	err := c.link.Send(ctx, m)
	if err != nil {
		c.fail(err)
	}
	return err
}

// open reserves the next request ID and a local slot. The local cap mirrors
// the server's, so a live link never exceeds it.
func (c *Client) open(oneShot bool) (*Updates, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if len(c.ops) >= maxOperations {
		return nil, fmt.Errorf("workerclient: %d operations already open on this connection", maxOperations)
	}
	c.nextID++
	s := &Updates{client: c, id: c.nextID, queue: make(chan queued, 8), closed: make(chan struct{}), oneShot: oneShot}
	c.ops[s.id] = s
	return s, nil
}

// start opens one operation and sends m for it. sent reports whether the
// send was attempted, i.e. whether a failure leaves the outcome ambiguous.
func (c *Client) start(ctx context.Context, oneShot bool, m *circusv1.WorkerUplink) (s *Updates, sent bool, err error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if s, err = c.open(oneShot); err != nil {
		return nil, false, err
	}
	m.RequestId = s.id
	if err = c.send(ctx, m); err != nil {
		c.mu.Lock()
		if c.ops[s.id] == s {
			delete(c.ops, s.id)
		}
		closeUpdatesChannel(s)
		c.mu.Unlock()
		return nil, true, err
	}
	return s, true, nil
}

// Submit validates IDs before sending, then watches all updates, including
// recovery after Unknown, until terminal. A send error wraps ErrSubmitSent.
func (c *Client) Submit(ctx context.Context, device string, call *circusv1.DeviceCall) (*Updates, error) {
	if call == nil || !identifier.Valid(call.GetCallId(), false) {
		return nil, errors.New("workerclient: invalid call ID")
	}
	if !identifier.Valid(call.GetSessionId(), false) {
		return nil, errors.New("workerclient: invalid session ID")
	}
	s, sent, err := c.start(ctx, false, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_SubmitCall{SubmitCall: &circusv1.SubmitCall{Device: device, Call: call}}})
	if err != nil && sent {
		return nil, fmt.Errorf("%w: %w", ErrSubmitSent, err)
	}
	return s, err
}

// Cancel records cancellation intent and returns the server's single reply:
// the call's state read after the intent was recorded. Cancelled means it was
// cancelled before dispatch; Dispatched or Unknown mean the daemon's result
// decides (a watched Submit stream still carries it); a terminal outcome means
// the call had already finished; Rejected NOT_FOUND means no such call.
func (c *Client) Cancel(ctx context.Context, id string) (*circusv1.CallUpdate, error) {
	if !identifier.Valid(id, false) {
		return nil, errors.New("workerclient: invalid call ID")
	}
	return c.oneShot(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CancelCall{CancelCall: &circusv1.CancelCall{CallId: id}}})
}

// Query asks for the current durable/reconciled outcome of a call, watched or
// not. NOT_FOUND answers only this query and is not a call outcome.
func (c *Client) Query(ctx context.Context, id string) (*circusv1.CallUpdate, error) {
	return c.oneShot(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: id}}})
}

// oneShot sends m and waits for its single reply. Giving up early (ctx)
// unwatches the operation, which frees its slot at once.
func (c *Client) oneShot(ctx context.Context, m *circusv1.WorkerUplink) (*circusv1.CallUpdate, error) {
	s, _, err := c.start(ctx, true, m)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return s.Next(ctx)
}

// Next transfers an immutable update to the caller; ctx only bounds this read.
// After the operation's last update (terminal, or a one-shot reply) it
// returns io.EOF.
func (s *Updates) Next(ctx context.Context) (*circusv1.CallUpdate, error) {
	s.client.mu.Lock()
	finished := s.finished
	s.client.mu.Unlock()
	if finished {
		return nil, io.EOF
	}
	consume := func(q queued) (*circusv1.CallUpdate, error) {
		last := s.oneShot || wire.Terminal(q.update)
		s.client.mu.Lock()
		s.client.bytes -= q.bytes
		if last {
			s.finished = true
			closeUpdatesChannel(s)
			s.client.bytes -= s.drain()
		}
		s.client.mu.Unlock()
		return q.update, nil
	}
	select {
	case q := <-s.queue:
		return consume(q)
	default:
	}
	select {
	case q := <-s.queue:
		return consume(q)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.closed:
		return nil, io.EOF
	case <-s.client.done:
		// A final update can race the second poll. Prefer anything the reader
		// managed to queue before teardown over the transport error.
		select {
		case q := <-s.queue:
			return consume(q)
		default:
		}
		s.client.mu.Lock()
		err := s.client.err
		s.client.mu.Unlock()
		return nil, err
	}
}

// drain drops every queued update without blocking and returns their
// charges. A concurrent Next may take an update between a length check and a
// receive, so a blocking receive here could wait forever while holding
// client.mu, which that Next then needs.
func (s *Updates) drain() int64 {
	var n int64
	for {
		select {
		case q := <-s.queue:
			n += q.bytes
		default:
			return n
		}
	}
}

// closeUpdatesChannel closes the stream channel exactly once while client.mu is held.
func closeUpdatesChannel(s *Updates) {
	if !s.closedOnce {
		close(s.closed)
		s.closedOnce = true
	}
}

// Close drops unread updates and releases their byte charges. If the
// operation is still open on the server, Close frees its local slot and sends
// Unwatch before any later operation can reuse that slot; the call itself is
// never cancelled.
func (s *Updates) Close() error {
	c := s.client
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	c.mu.Lock()
	open := c.ops[s.id] == s
	if open {
		delete(c.ops, s.id)
	}
	c.bytes -= s.drain()
	closeUpdatesChannel(s)
	live := c.err == nil
	c.mu.Unlock()
	if open && live {
		// A failed send fails the whole link, which ends the operation too.
		_ = c.send(context.Background(), &circusv1.WorkerUplink{RequestId: s.id, Payload: &circusv1.WorkerUplink_Unwatch{Unwatch: &circusv1.Unwatch{}}})
	}
	return nil
}

func (c *Client) clearOperations() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, s := range c.ops {
		closeUpdatesChannel(s)
		delete(c.ops, id)
		c.bytes -= s.drain()
	}
	c.bytes = 0
}

// Close aborts the link and joins the reader, releasing all unread streams.
func (c *Client) Close() error {
	c.fail(wire.ErrClosed)
	<-c.done
	c.http.CloseIdleConnections()
	c.clearOperations()
	return nil
}

// Done closes after the reader has ended, including for an idle revoked link.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended, or nil while it is live.
func (c *Client) Err() error { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
