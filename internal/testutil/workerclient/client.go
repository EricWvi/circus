// Package workerclient is a worker-link client for tests: it speaks the
// worker protocol of docs/wire.md over a real link, one operation at a
// time or pipelined, and exposes every reply and hint as plain protobuf
// messages, so a test can drive the control plane exactly as a worker does
// and also break the protocol on purpose.
//
// A Client is one connection: Dial authenticates with a worker credential
// and exchanges Hello (with the given boot id) and Welcome. Request ids
// come from a per-connection counter starting at 1. Each operation's
// replies are routed by request id to its own buffered queue; a reply for
// an id the client does not have open is dropped, as the protocol says.
//
//	c, err := workerclient.Dial(ctx, workerclient.Options{URL: srv.URL, Token: tok, PrincipalID: id, BootID: "boot-1"})
//	events, err := c.Attach(ctx, &circusv1.Attach{MaxSessions: 4})
//	ev, err := events.Next(ctx)                   // an Offer
//	r, err := c.Session(ctx, workerclient.Acquire(ev.GetOffer().GetSessionId(), true))
//	call, err := c.Submit(ctx, &circusv1.SubmitCall{...})
//	u, err := call.Next(ctx)
//
// The client never reconnects; open a new Client (same BootID for a
// reconnect of the same worker process). It bounds what it buffers: at most
// MaxQueued messages per operation and per hint stream; overflow fails the
// client. It is meant for tests and keeps no other limits of a production
// worker (the 48-call reservation, for example).
package workerclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/loopback"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
)

// MaxQueued bounds the unread messages of one operation or hint stream.
const MaxQueued = 1024

// ErrOverflow fails a client whose test stopped reading a stream.
var ErrOverflow = errors.New("workerclient: unread messages overflowed")

// Options configures Dial.
type Options struct {
	// URL is the control plane's base URL (http://host:port); the link path
	// is appended.
	URL string
	// Token is the worker credential's bearer token and PrincipalID its
	// worker id, which Welcome must name.
	Token, PrincipalID string
	// CAFile supplies the TLS roots for a privately signed server.
	CAFile string
	// BootID is sent in Hello; keep it across reconnects of one worker
	// process. Empty sends none (as the call CLI does).
	BootID string
	// Wire sets the local link limits; zero values are the defaults.
	Wire wire.Options
	// ClientVersion is sent in Hello; empty sends "test".
	ClientVersion string
	// Features are sent in Hello.features.
	Features []string
}

// Client is one worker connection.
type Client struct {
	link    *wire.Link
	welcome *circusv1.Welcome
	done    chan struct{}

	mu     sync.Mutex
	nextID uint64
	ops    map[uint64]*Stream
	err    error
}

// Stream is the queue of one operation's downlinks: CallUpdates of a
// Submit, the one SessionReply of a session operation, or the WorkerEvents
// of Attach.
type Stream struct {
	c  *Client
	ID uint64
	ch chan *circusv1.WorkerDownlink
}

// Dial connects and completes the handshake.
func Dial(ctx context.Context, o Options) (*Client, error) {
	u, httpClient, err := loopback.Link(o.URL, o.CAFile, "/v1/link/worker")
	if err != nil {
		return nil, err
	}
	defer httpClient.CloseIdleConnections()
	hctx, cancel := context.WithTimeout(ctx, wire.HelloTimeout)
	defer cancel()
	conn, resp, err := wire.DialWebSocket(hctx, u, &websocket.DialOptions{
		HTTPClient: httpClient,
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + o.Token}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("workerclient: HTTP %d: %w", resp.StatusCode, err)
		}
		return nil, err
	}
	version := o.ClientVersion
	if version == "" {
		version = "test"
	}
	hello := &circusv1.Hello{ProtocolVersion: wire.ProtocolVersion, BootId: o.BootID, ClientVersion: version, Features: o.Features}
	welcome, opts, err := wire.DialHello(hctx, conn, wire.WorkerUplink, hello, o.PrincipalID, o.Wire)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	l, err := wire.New(conn, wire.WorkerSide, opts)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	c := &Client{link: l, welcome: welcome, done: make(chan struct{}), ops: make(map[uint64]*Stream)}
	go c.read()
	return c, nil
}

// Welcome returns the server's Welcome.
func (c *Client) Welcome() *circusv1.Welcome { return c.welcome }

func (c *Client) read() {
	defer close(c.done)
	for {
		m, err := c.link.Receive(context.Background())
		if err != nil {
			c.fail(err)
			return
		}
		d := m.(*circusv1.WorkerDownlink)
		if d.GetHeartbeat() != nil {
			continue
		}
		c.mu.Lock()
		s := c.ops[d.GetRequestId()]
		c.mu.Unlock()
		if s == nil {
			continue // not open: dropped
		}
		select {
		case s.ch <- d:
		default:
			c.fail(ErrOverflow)
			return
		}
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

// Err returns why the connection ended, nil while it is open.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Done is closed when the connection has ended.
func (c *Client) Done() <-chan struct{} { return c.done }

// Close aborts the connection (the server sees it drop) and waits for the
// reader.
func (c *Client) Close() error {
	c.fail(errors.New("workerclient: closed"))
	<-c.done
	return nil
}

// Shutdown flushes what was sent and closes the connection gracefully.
func (c *Client) Shutdown(ctx context.Context) error {
	err := c.link.Shutdown(ctx)
	<-c.done
	return err
}

// Open sends m as a new operation under the next request id and returns
// its stream. m's request_id is overwritten.
func (c *Client) Open(ctx context.Context, m *circusv1.WorkerUplink) (*Stream, error) {
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, err
	}
	c.nextID++
	s := &Stream{c: c, ID: c.nextID, ch: make(chan *circusv1.WorkerDownlink, MaxQueued)}
	c.ops[s.ID] = s
	c.mu.Unlock()
	m.RequestId = s.ID
	if err := c.link.Send(ctx, m); err != nil {
		c.forget(s.ID)
		return nil, err
	}
	return s, nil
}

// SendRaw sends m as it is, request id included, without opening an
// operation: for tests that break the protocol.
func (c *Client) SendRaw(ctx context.Context, m *circusv1.WorkerUplink) error {
	return c.link.Send(ctx, m)
}

func (c *Client) forget(id uint64) {
	c.mu.Lock()
	delete(c.ops, id)
	c.mu.Unlock()
}

// Next returns the stream's next downlink, waiting until one arrives, the
// connection ends or ctx ends.
func (s *Stream) Next(ctx context.Context) (*circusv1.WorkerDownlink, error) {
	select {
	case d := <-s.ch:
		return d, nil
	default:
	}
	select {
	case d := <-s.ch:
		return d, nil
	case <-s.c.done:
		select {
		case d := <-s.ch:
			return d, nil
		default:
		}
		return nil, fmt.Errorf("workerclient: connection ended: %w", s.c.Err())
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Pending reports how many downlinks are queued unread.
func (s *Stream) Pending() int { return len(s.ch) }

// Unwatch sends Unwatch for the stream's operation and stops routing its
// downlinks.
func (s *Stream) Unwatch(ctx context.Context) error {
	s.c.forget(s.ID)
	return s.c.link.Send(ctx, &circusv1.WorkerUplink{RequestId: s.ID, Payload: &circusv1.WorkerUplink_Unwatch{Unwatch: &circusv1.Unwatch{}}})
}

// Abandon queues Unwatch before the caller reuses the operation slot.
// If the link cannot accept Unwatch, close it so the server retires the slot.
func (s *Stream) Abandon() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for {
		if err := s.Unwatch(ctx); err == nil {
			return
		} else if !errors.Is(err, wire.ErrBusy) {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
			continue
		case <-s.c.Done():
			return
		case <-ctx.Done():
		}
		break
	}
	_ = s.c.Close()
}

// Events is an Attach stream.
type Events struct{ *Stream }

// Next returns the next session hint (Offer, Nudge, LeaseRevoked,
// ReleaseIdle), skipping the AgentsWanted events the control plane sends
// after Attach and on every change of the worker's wishes; NextEvent
// returns those too.
func (e Events) Next(ctx context.Context) (*circusv1.WorkerEvent, error) {
	for {
		ev, err := e.NextEvent(ctx)
		if err != nil || ev.GetAgentsWanted() == nil {
			return ev, err
		}
	}
}

// NextEvent returns the next WorkerEvent of any kind.
func (e Events) NextEvent(ctx context.Context) (*circusv1.WorkerEvent, error) {
	d, err := e.Stream.Next(ctx)
	if err != nil {
		return nil, err
	}
	if d.GetWorkerEvent() == nil {
		return nil, fmt.Errorf("workerclient: Attach stream got %T", d.GetPayload())
	}
	return d.GetWorkerEvent(), nil
}

// ClaudeCode is an Attach.agents (or ReportAgents) entry for Claude Code in
// state, installed at version when it is on disk, with target as the
// worker's target version.
func ClaudeCode(state circusv1.AgentStatus_State, version, target string) *circusv1.AgentStatus {
	return &circusv1.AgentStatus{Agent: circusv1.AgentKind_AGENT_KIND_CLAUDE_CODE, State: state, InstalledVersion: version, TargetVersion: target}
}

// ClaudeInstalled reports Claude Code installed at the worker's target
// version, the state in which a worker may be offered sessions (the
// control plane records the wish for an agent first reported on disk).
func ClaudeInstalled() []*circusv1.AgentStatus {
	return []*circusv1.AgentStatus{ClaudeCode(circusv1.AgentStatus_STATE_INSTALLED, "cli", "cli")}
}

// ReportAgents sends ReportAgents without waiting for its reply.
func (c *Client) ReportAgents(ctx context.Context, agents ...*circusv1.AgentStatus) (Reply, error) {
	s, err := c.Open(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_ReportAgents{ReportAgents: &circusv1.ReportAgents{Agents: agents}}})
	return Reply{s}, err
}

// UpdateFailed sends UpdateFailed without waiting for its reply.
func (c *Client) UpdateFailed(ctx context.Context, updateID, errText string) (Reply, error) {
	s, err := c.Open(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_UpdateFailed{UpdateFailed: &circusv1.UpdateFailed{UpdateId: updateID, Error: errText}}})
	return Reply{s}, err
}

// Attach opens the hint stream.
func (c *Client) Attach(ctx context.Context, a *circusv1.Attach) (Events, error) {
	s, err := c.Open(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_Attach{Attach: a}})
	return Events{s}, err
}

// Reply is a pending SessionReply.
type Reply struct{ *Stream }

// Wait returns the operation's SessionReply.
func (r Reply) Wait(ctx context.Context) (*circusv1.SessionReply, error) {
	defer r.c.forget(r.ID)
	d, err := r.Stream.Next(ctx)
	if err != nil {
		if ctx.Err() != nil {
			r.Abandon()
		}
		return nil, err
	}
	if d.GetSessionReply() == nil {
		return nil, fmt.Errorf("workerclient: session operation got %T", d.GetPayload())
	}
	return d.GetSessionReply(), nil
}

// Send sends a SessionRequest without waiting for its reply.
func (c *Client) Send(ctx context.Context, req *circusv1.SessionRequest) (Reply, error) {
	s, err := c.Open(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_Session{Session: req}})
	return Reply{s}, err
}

// Session sends a SessionRequest and waits for its reply.
func (c *Client) Session(ctx context.Context, req *circusv1.SessionRequest) (*circusv1.SessionReply, error) {
	r, err := c.Send(ctx, req)
	if err != nil {
		return nil, err
	}
	return r.Wait(ctx)
}

// Resume sends ResumeLeases and waits for its reply.
func (c *Client) Resume(ctx context.Context, leases ...*circusv1.ResumeLeases_Held) (*circusv1.SessionReply, error) {
	s, err := c.Open(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_ResumeLeases{ResumeLeases: &circusv1.ResumeLeases{Leases: leases}}})
	if err != nil {
		return nil, err
	}
	return Reply{s}.Wait(ctx)
}

// ListDevices sends ListDevices and waits for its reply.
func (c *Client) ListDevices(ctx context.Context) (*circusv1.SessionReply, error) {
	s, err := c.Open(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_ListDevices{ListDevices: &circusv1.ListDevices{}}})
	if err != nil {
		return nil, err
	}
	return Reply{s}.Wait(ctx)
}

// Call is a Submit stream.
type Call struct{ *Stream }

// Next returns the call's next CallUpdate.
func (c Call) Next(ctx context.Context) (*circusv1.CallUpdate, error) {
	d, err := c.Stream.Next(ctx)
	if err != nil {
		return nil, err
	}
	update := d.GetCallUpdate()
	if update == nil {
		return nil, fmt.Errorf("workerclient: Submit stream got %T", d.GetPayload())
	}
	if wire.Terminal(update) {
		c.c.forget(c.ID)
	}
	return update, nil
}

// Submit sends SubmitCall.
func (c *Client) Submit(ctx context.Context, sc *circusv1.SubmitCall) (Call, error) {
	s, err := c.Open(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_SubmitCall{SubmitCall: sc}})
	return Call{s}, err
}

// Query opens a one-reply operation to read the current call outcome.
func (c *Client) Query(ctx context.Context, callID string) (*circusv1.CallUpdate, error) {
	return c.callOnce(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: callID}}})
}

// Cancel opens a one-reply operation to record cancellation intent.
func (c *Client) Cancel(ctx context.Context, callID string) (*circusv1.CallUpdate, error) {
	return c.callOnce(ctx, &circusv1.WorkerUplink{Payload: &circusv1.WorkerUplink_CancelCall{CancelCall: &circusv1.CancelCall{CallId: callID}}})
}

func (c *Client) callOnce(ctx context.Context, m *circusv1.WorkerUplink) (*circusv1.CallUpdate, error) {
	s, err := c.Open(ctx, m)
	if err != nil {
		return nil, err
	}
	defer c.forget(s.ID)
	u, err := (Call{s}).Next(ctx)
	if err != nil && ctx.Err() != nil {
		s.Abandon()
	}
	return u, err
}

// Request builders.

// Acquire is AcquireLease (epoch 0).
func Acquire(session string, fresh bool) *circusv1.SessionRequest {
	return &circusv1.SessionRequest{SessionId: session, Op: &circusv1.SessionRequest_AcquireLease{AcquireLease: &circusv1.AcquireLease{Fresh: fresh}}}
}

// Release is ReleaseLease.
func Release(session string, epoch uint64, reason circusv1.ReleaseLease_Reason) *circusv1.SessionRequest {
	return &circusv1.SessionRequest{SessionId: session, Epoch: epoch, Op: &circusv1.SessionRequest_ReleaseLease{ReleaseLease: &circusv1.ReleaseLease{Reason: reason}}}
}

// LaunchEnv is GetLaunchEnv.
func LaunchEnv(session string, epoch uint64) *circusv1.SessionRequest {
	return &circusv1.SessionRequest{SessionId: session, Epoch: epoch, Op: &circusv1.SessionRequest_GetLaunchEnv{GetLaunchEnv: &circusv1.GetLaunchEnv{}}}
}

// TakeInputs is TakeInputs.
func TakeInputs(session string, epoch uint64) *circusv1.SessionRequest {
	return &circusv1.SessionRequest{SessionId: session, Epoch: epoch, Op: &circusv1.SessionRequest_TakeInputs{TakeInputs: &circusv1.TakeInputs{}}}
}

// Started is ReportTurn STARTED.
func Started(session string, epoch, localSeq uint64, origin circusv1.TurnOrigin) *circusv1.SessionRequest {
	return &circusv1.SessionRequest{SessionId: session, Epoch: epoch, Op: &circusv1.SessionRequest_ReportTurn{ReportTurn: &circusv1.ReportTurn{
		LocalSeq: localSeq, Phase: circusv1.ReportTurn_PHASE_STARTED, Origin: origin}}}
}

// Ended is ReportTurn ENDED.
func Ended(session string, epoch, localSeq uint64, outcome circusv1.TurnOutcome, p *circusv1.ReportTurn_Persisted) *circusv1.SessionRequest {
	return &circusv1.SessionRequest{SessionId: session, Epoch: epoch, Op: &circusv1.SessionRequest_ReportTurn{ReportTurn: &circusv1.ReportTurn{
		LocalSeq: localSeq, Phase: circusv1.ReportTurn_PHASE_ENDED, Outcome: outcome, Persisted: p}}}
}

// Append is AppendTranscript.
func Append(session string, epoch uint64, subpath string, batch uint64, entries ...*circusv1.TranscriptEntry) *circusv1.SessionRequest {
	return &circusv1.SessionRequest{SessionId: session, Epoch: epoch, Op: &circusv1.SessionRequest_AppendTranscript{AppendTranscript: &circusv1.AppendTranscript{
		Subpath: subpath, BatchSeq: batch, Entries: entries}}}
}

// Load is LoadTranscript.
func Load(session string, epoch uint64, subpath string, after uint64, maxBytes uint32) *circusv1.SessionRequest {
	return &circusv1.SessionRequest{SessionId: session, Epoch: epoch, Op: &circusv1.SessionRequest_LoadTranscript{LoadTranscript: &circusv1.LoadTranscript{
		Subpath: subpath, AfterSeq: after, MaxBytes: maxBytes}}}
}

// Publish is PublishEvents.
func Publish(session string, epoch, batch uint64, events ...*circusv1.PublishEvents_Event) *circusv1.SessionRequest {
	return &circusv1.SessionRequest{SessionId: session, Epoch: epoch, Op: &circusv1.SessionRequest_PublishEvents{PublishEvents: &circusv1.PublishEvents{
		BatchSeq: batch, Events: events}}}
}

// State is GetSessionState.
func State(session string, epoch uint64, sections ...circusv1.GetSessionState_Section) *circusv1.SessionRequest {
	return &circusv1.SessionRequest{SessionId: session, Epoch: epoch, Op: &circusv1.SessionRequest_GetState{GetState: &circusv1.GetSessionState{Sections: sections}}}
}

// Patch is PatchSessionState.
func Patch(session string, epoch uint64, p *circusv1.PatchSessionState) *circusv1.SessionRequest {
	return &circusv1.SessionRequest{SessionId: session, Epoch: epoch, Op: &circusv1.SessionRequest_PatchState{PatchState: p}}
}

// Op wraps any other operation.
func Op(session string, epoch uint64, op any) *circusv1.SessionRequest {
	r := &circusv1.SessionRequest{SessionId: session, Epoch: epoch}
	switch v := op.(type) {
	case *circusv1.ListTranscriptKeys:
		r.Op = &circusv1.SessionRequest_ListTranscriptKeys{ListTranscriptKeys: v}
	case *circusv1.RequestApproval:
		r.Op = &circusv1.SessionRequest_RequestApproval{RequestApproval: v}
	case *circusv1.ConsumeGrant:
		r.Op = &circusv1.SessionRequest_ConsumeGrant{ConsumeGrant: v}
	case *circusv1.TranscriptRepaired:
		r.Op = &circusv1.SessionRequest_TranscriptRepaired{TranscriptRepaired: v}
	case *circusv1.ReportTurn:
		r.Op = &circusv1.SessionRequest_ReportTurn{ReportTurn: v}
	default:
		panic(fmt.Sprintf("workerclient: unsupported operation %T", op))
	}
	return r
}
