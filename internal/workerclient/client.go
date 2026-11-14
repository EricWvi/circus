// Package workerclient implements a single authenticated worker connection.
// It never reconnects: transport loss returns an error; open a new connection
// and Query the call ID. Credentials must have the worker role. TLS roots,
// loopback plaintext restrictions and redirect refusal come from loopback.Link,
// shared with daemon/link.
//
// At most four streams (the server's per-connection operation cap), eight
// updates each, and 256 MiB decoded queued updates are retained. Overflow
// aborts the link. One reader borrows one wire message; callers own returned
// updates. Every stream must be closed by its caller. Closing a Submit stream
// before terminal leaves a bounded ghost that still counts against the cap,
// discards updates, and is removed by the terminal update or link teardown.
// Closing never cancels the call or frees its server slot early.
// TODO(M3): send an unsubscribe message when the protocol adds one.
// A call ID may have only one operation on a connection at a time (the
// protocol has no request ID); query an actively watched call on another link.
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

const maxStreams = 4

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
	link       *wire.Link
	http       *http.Client
	mu         sync.Mutex
	streams    map[string]*Updates
	bytes      int64
	err        error
	done       chan struct{}
	maxMessage int
}
type queued struct {
	update *circusv1.CallUpdate
	bytes  int64
}

// Updates is a bounded stream. Closing it detaches locally, never cancels a call.
type Updates struct {
	client     *Client
	id         string
	queue      chan queued
	closed     chan struct{}
	terminal   bool
	answered   bool
	ghost      bool
	oneShot    bool
	closedOnce bool
}

// Dial authenticates, exchanges Hello/Welcome within ten seconds, then starts
// the reader. HTTP 401/403 and version rejections are returned to the caller.
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
	c := &Client{link: l, http: h, streams: make(map[string]*Updates), done: make(chan struct{}), maxMessage: opts.MaxMessageBytes}
	go c.read()
	return c, nil
}
func (c *Client) read() {
	defer close(c.done)
	defer c.removeGhosts()
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
			err = c.enqueue(u, cost)
		}
		if err != nil {
			c.fail(err)
			return
		}
	}
}

// enqueue retains one update or reports the bounded-queue failure that read
// must use to abort the whole link. Unmatched updates are harmless and dropped.
func (c *Client) enqueue(u *circusv1.CallUpdate, cost int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.streams[u.CallId]
	if s == nil {
		return nil
	}
	if s.ghost {
		if s.oneShot || wire.Terminal(u) {
			delete(c.streams, u.CallId)
		}
		return nil
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
func (c *Client) open(id string, oneShot bool) (*Updates, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return nil, c.err
	}
	if len(c.streams) >= maxStreams || c.streams[id] != nil {
		return nil, errors.New("workerclient: stream capacity (4) or duplicate call ID")
	}
	s := &Updates{client: c, id: id, queue: make(chan queued, 8), closed: make(chan struct{}), oneShot: oneShot}
	c.streams[id] = s
	return s, nil
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
	s, err := c.open(call.GetCallId(), false)
	if err != nil {
		return nil, err
	}
	err = c.send(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_SubmitCall{SubmitCall: &circusv1.SubmitCall{Device: device, Call: call}}})
	if err != nil {
		_ = s.Close()
		return nil, fmt.Errorf("%w: %w", ErrSubmitSent, err)
	}
	return s, nil
}

// Cancel sends cancellation intent without opening a stream or waiting for a
// reply. A watched Submit stream carries the real terminal update; callers that
// need the outcome immediately can issue Query afterward.
func (c *Client) Cancel(ctx context.Context, id string) error {
	if !identifier.Valid(id, false) {
		return errors.New("workerclient: invalid call ID")
	}
	return c.send(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CancelCall{CancelCall: &circusv1.CancelCall{CallId: id}}})
}

// Query asks for the current durable/reconciled outcome of an unwatched call.
// NOT_FOUND answers only this query and is not a call outcome.
func (c *Client) Query(ctx context.Context, id string) (*circusv1.CallUpdate, error) {
	s, err := c.open(id, true)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err = c.send(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: id}}}); err != nil {
		return nil, err
	}
	return s.Next(ctx)
}

// Next transfers an immutable update to the caller; ctx only bounds this read.
func (s *Updates) Next(ctx context.Context) (*circusv1.CallUpdate, error) {
	s.client.mu.Lock()
	terminal := s.terminal
	s.client.mu.Unlock()
	if terminal {
		return nil, io.EOF
	}
	consume := func(q queued) (*circusv1.CallUpdate, error) {
		terminal := wire.Terminal(q.update)
		s.client.mu.Lock()
		s.client.bytes -= q.bytes
		s.answered = true
		if terminal {
			s.terminal = true
			if s.client.streams[s.id] == s {
				delete(s.client.streams, s.id)
			}
			closeUpdatesChannel(s)
			for len(s.queue) > 0 {
				s.client.bytes -= (<-s.queue).bytes
			}
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

// closeUpdatesChannel closes the stream channel exactly once while client.mu is held.
func closeUpdatesChannel(s *Updates) {
	if !s.closedOnce {
		close(s.closed)
		s.closedOnce = true
	}
}

// Close drops unread updates and releases their byte charges. A nonterminal
// Submit close leaves a ghost that keeps the client and server caps aligned.
func (s *Updates) Close() error {
	c := s.client
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streams[s.id] != s || s.ghost {
		return nil
	}
	terminal := s.terminal
	answered := s.answered
	for {
		select {
		case q := <-s.queue:
			c.bytes -= q.bytes
			answered = true
			terminal = terminal || wire.Terminal(q.update)
		default:
			if terminal || s.oneShot && answered {
				delete(c.streams, s.id)
			} else {
				s.ghost = true
			}
			closeUpdatesChannel(s)
			return nil
		}
	}
}

// removeGhosts releases only already-closed streams on automatic reader
// teardown. Active streams and their queued outcomes remain available to Next;
// explicit Client.Close is what drains those queues.
func (c *Client) removeGhosts() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, s := range c.streams {
		if s.ghost {
			delete(c.streams, id)
		}
	}
}

func (c *Client) clearStreams() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, s := range c.streams {
		if !s.ghost {
			closeUpdatesChannel(s)
		}
		delete(c.streams, id)
		for len(s.queue) > 0 {
			c.bytes -= (<-s.queue).bytes
		}
	}
	c.bytes = 0
}

// Close aborts the link and joins the reader, releasing all unread streams.
func (c *Client) Close() error {
	c.fail(wire.ErrClosed)
	<-c.done
	c.http.CloseIdleConnections()
	c.clearStreams()
	return nil
}

// Done closes after the reader has ended, including for an idle revoked link.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err reports why the connection ended, or nil while it is live.
func (c *Client) Err() error { c.mu.Lock(); defer c.mu.Unlock(); return c.err }
