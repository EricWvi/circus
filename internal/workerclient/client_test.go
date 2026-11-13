package workerclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/EricWvi/circus/internal/wirepolicy"
	"google.golang.org/protobuf/proto"
)

func TestSubmitValidatesIDsBeforeOpeningStream(t *testing.T) {
	c := &Client{}
	valid := func() *circusv1.DeviceCall {
		return &circusv1.DeviceCall{CallId: "valid", SessionId: "session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "true"}}}
	}
	for _, call := range []*circusv1.DeviceCall{nil, {CallId: "bad id", SessionId: "session"}, {CallId: "valid", SessionId: "bad session"}, {CallId: "valid", SessionId: ""}} {
		if _, err := c.Submit(t.Context(), "desk", call); err == nil {
			t.Fatalf("accepted invalid call: %v", call)
		}
	}
	call := valid()
	call.CallId = strings.Repeat("x", 129)
	if _, err := c.Submit(t.Context(), "desk", call); err == nil {
		t.Fatal("accepted overlong call ID")
	}
}

func TestCancelValidatesID(t *testing.T) {
	c := &Client{}
	if err := c.Cancel(t.Context(), ""); err == nil {
		t.Fatal("empty call ID was sent instead of rejected locally")
	}
}

func TestClientStreamCapMatchesServer(t *testing.T) {
	c := &Client{streams: make(map[string]*Updates)}
	for i := range 4 {
		id := string(rune('a' + i))
		if _, err := c.open(id, false); err != nil {
			t.Fatalf("stream %d: %v", i+1, err)
		}
	}
	if _, err := c.open("fifth", false); err == nil {
		t.Fatal("fifth stream exceeded the server operation cap")
	}
}

func TestGhostAccounting(t *testing.T) {
	c := &Client{streams: make(map[string]*Updates)}
	s, err := c.open("call", false)
	if err != nil {
		t.Fatal(err)
	}
	mustClose := s.Close()
	if mustClose != nil || !s.ghost || len(c.streams) != 1 {
		t.Fatalf("nonterminal close did not leave a ghost: %v %+v", mustClose, s)
	}
	if _, err = c.open("call", false); err == nil {
		t.Fatal("ghost allowed a duplicate call ID")
	}
	accepted := &circusv1.CallUpdate{CallId: "call", Update: &circusv1.CallUpdate_Accepted_{Accepted: &circusv1.CallUpdate_Accepted{}}}
	if err = c.enqueue(accepted, 1); err != nil || !s.ghost {
		t.Fatalf("nonterminal update changed ghost: %v", err)
	}
	result := &circusv1.CallUpdate{CallId: "call", Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{}}}
	if err = c.enqueue(result, 1); err != nil || len(c.streams) != 0 {
		t.Fatalf("terminal update did not release ghost: %v", err)
	}
	query, err := c.open("query", true)
	if err != nil {
		t.Fatal(err)
	}
	if err = query.Close(); err != nil || !query.ghost || len(c.streams) != 1 {
		t.Fatalf("unanswered one-shot Query did not leave a ghost: %v", err)
	}
	accepted.CallId = "query"
	if err = c.enqueue(accepted, 1); err != nil || len(c.streams) != 0 {
		t.Fatalf("first one-shot answer did not release ghost: %v", err)
	}
}

func TestReaderTeardownPreservesQueuedOutcome(t *testing.T) {
	transport := errors.New("transport failure")
	c := &Client{streams: make(map[string]*Updates), done: make(chan struct{})}
	resultStream, err := c.open("result", false)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := c.open("pending", false)
	if err != nil {
		t.Fatal(err)
	}
	result := &circusv1.CallUpdate{CallId: "result", Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{}}}
	if err = c.enqueue(result, 1); err != nil {
		t.Fatal(err)
	}
	c.removeGhosts()
	c.err = transport
	close(c.done)
	if update, err := resultStream.Next(t.Context()); err != nil || update.GetResult() == nil {
		t.Fatalf("queued outcome was discarded: %v, %v", update, err)
	}
	if _, err = pending.Next(t.Context()); !errors.Is(err, transport) {
		t.Fatalf("blocked Next did not observe transport error: %v", err)
	}
}

func TestTerminalNextRacingCloseReleasesStream(t *testing.T) {
	for range 100 {
		c := &Client{streams: make(map[string]*Updates), done: make(chan struct{})}
		s, err := c.open("call", false)
		if err != nil {
			t.Fatal(err)
		}
		u := &circusv1.CallUpdate{CallId: "call", Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{}}}
		if err = c.enqueue(u, 1); err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, _ = s.Next(t.Context())
		}()
		go func() {
			defer wg.Done()
			<-start
			_ = s.Close()
		}()
		close(start)
		wg.Wait()
		if len(c.streams) != 0 {
			t.Fatal("terminal Next/Close race stranded a ghost")
		}
	}
}

func TestTerminalNextDrainsLaterQueuedUpdates(t *testing.T) {
	c := &Client{streams: make(map[string]*Updates), done: make(chan struct{})}
	s, err := c.open("call", false)
	if err != nil {
		t.Fatal(err)
	}
	result := &circusv1.CallUpdate{CallId: "call", Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{}}}
	accepted := &circusv1.CallUpdate{CallId: "call", Update: &circusv1.CallUpdate_Accepted_{Accepted: &circusv1.CallUpdate_Accepted{}}}
	if err = c.enqueue(result, 1); err != nil {
		t.Fatal(err)
	}
	if err = c.enqueue(accepted, 1); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Next(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.bytes != 0 || len(s.queue) != 0 {
		t.Fatalf("terminal Next retained charges=%d queued=%d", c.bytes, len(s.queue))
	}
}

func TestUpdateQueueOverflowFailsLink(t *testing.T) {
	served := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(served)
		conn, err := wire.AcceptWebSocket(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		data, err := conn.ReadMessage(ctx)
		if err != nil {
			return
		}
		m, err := wire.Decode(wire.WorkerUplink, data, wire.MaxMetadataBytes)
		if err != nil || m.(*circusv1.WorkerUplink).GetHello() == nil {
			return
		}
		welcome := &circusv1.Welcome{PrincipalId: "worker", InstanceId: "instance", NegotiatedVersion: 1}
		opts, err := wire.FillWelcome(welcome, wire.Options{MaxMessageBytes: 1 << 20})
		if err != nil {
			return
		}
		data, _ = proto.Marshal(&circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_Welcome{Welcome: welcome}})
		if err = conn.WriteMessage(ctx, data); err != nil {
			return
		}
		link, err := wire.New(conn, wire.WorkerControlSide, opts)
		if err != nil {
			return
		}
		defer link.Close()
		if _, err = link.Receive(ctx); err != nil {
			return
		}
		for range 9 {
			u := &circusv1.CallUpdate{CallId: "overflow", Update: &circusv1.CallUpdate_Accepted_{Accepted: &circusv1.CallUpdate_Accepted{}}}
			msg := &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_CallUpdate{CallUpdate: u}}
			class, key, err := wirepolicy.Classify(msg)
			if err != nil || link.Send(ctx, msg, class, key) != nil {
				return
			}
		}
		// Keep the socket open until the client's overflow policy closes it;
		// otherwise EOF would race and hide the queue-full reason.
		_, _ = link.Receive(ctx)
	}))
	defer server.Close()
	c, err := Dial(t.Context(), Options{Credentials: credfile.File{ControlPlaneURL: server.URL, Role: credfile.RoleWorker, PrincipalID: "worker", Token: "token"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, err = c.Submit(t.Context(), "desk", &circusv1.DeviceCall{CallId: "overflow", SessionId: "session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "true"}}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
		if c.Err() == nil || !strings.Contains(c.Err().Error(), "update queue full") {
			t.Fatalf("link ended for wrong reason: %v", c.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reader dropped an overflowing update without failing the link")
	}
	// Exercise the real Submit send path on its now-closed wire link. Clear
	// only the recorded reader error so open reaches Send rather than failing early.
	c.mu.Lock()
	c.err = nil
	c.mu.Unlock()
	_, err = c.Submit(t.Context(), "desk", &circusv1.DeviceCall{CallId: "send-error", SessionId: "session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "true"}}})
	if !errors.Is(err, ErrSubmitSent) {
		t.Fatalf("Submit send error was not wrapped in ErrSubmitSent: %v", err)
	}
}
