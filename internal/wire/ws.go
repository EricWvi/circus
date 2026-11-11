package wire

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"sync"

	"github.com/coder/websocket"
)

// WebSocket owns a coder connection and the underlying transport's abort handle.
// Retaining the transport is necessary: coder CloseNow cannot interrupt a Close
// already in progress. AcceptWebSocket/DialWebSocket capture it without exposing
// HTTP authentication, TLS policy or routing decisions in wire.
type WebSocket struct {
	conn  *websocket.Conn
	abort net.Conn
}

func newWebSocket(conn *websocket.Conn, transport net.Conn) *WebSocket {
	// TLS Close can itself write close_notify; force abort closes the raw socket.
	for {
		unwrapped, ok := transport.(interface{ NetConn() net.Conn })
		if !ok {
			break
		}
		next := unwrapped.NetConn()
		if next == transport {
			break
		}
		transport = next
	}
	conn.SetReadLimit(MaxMetadataBytes)
	return &WebSocket{conn: conn, abort: transport}
}

type hijackCapture struct {
	http.ResponseWriter
	transport net.Conn
}

func (w *hijackCapture) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, b, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.transport = c
	}
	return c, b, err
}

// AcceptWebSocket upgrades an already authenticated HTTP request. All policy
// stays with the caller; options are passed to coder unchanged.
func AcceptWebSocket(w http.ResponseWriter, r *http.Request, opts *websocket.AcceptOptions) (*WebSocket, error) {
	capture := &hijackCapture{ResponseWriter: w}
	c, err := websocket.Accept(capture, r, opts)
	if err != nil {
		return nil, err
	}
	return newWebSocket(c, capture.transport), nil
}

// DialWebSocket retains the successful HTTP transport via standard httptrace.
// A custom RoundTripper must report GotConn; otherwise it cannot supply a safe
// deadline abort handle and is rejected. Options retain caller TLS/auth policy.
func DialWebSocket(ctx context.Context, url string, opts *websocket.DialOptions) (*WebSocket, *http.Response, error) {
	var mu sync.Mutex
	var transport net.Conn
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) { mu.Lock(); transport = info.Conn; mu.Unlock() }}
	c, response, err := websocket.Dial(httptrace.WithClientTrace(ctx, trace), url, opts)
	if err != nil {
		return nil, response, err
	}
	mu.Lock()
	raw := transport
	mu.Unlock()
	if raw == nil {
		_ = c.CloseNow()
		return nil, response, fmt.Errorf("wire: websocket transport unavailable from HTTP trace")
	}
	return newWebSocket(c, raw), response, nil
}
func (w *WebSocket) SetReadLimit(n int64) { w.conn.SetReadLimit(n) }
func (w *WebSocket) ReadMessage(ctx context.Context) ([]byte, error) {
	typ, data, err := w.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if typ != websocket.MessageBinary {
		_ = w.Close()
		return nil, protocol("expected binary websocket message")
	}
	return data, nil
}
func (w *WebSocket) WriteMessage(ctx context.Context, data []byte) error {
	return w.conn.Write(ctx, websocket.MessageBinary, data)
}
func (w *WebSocket) Close() error {
	_ = w.abort.Close()
	return w.conn.CloseNow()
}

// Shutdown performs a normal close handshake, aborts the actual socket at ctx,
// and joins the library close call. It never detaches a waiting close helper.
func (w *WebSocket) Shutdown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		_ = w.Close()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- w.conn.Close(websocket.StatusNormalClosure, "") }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = w.abort.Close()
		<-done
		return ctx.Err()
	}
}

var _ Conn = (*WebSocket)(nil)

// gracefulEnd reports a clean end of stream: EOF (wiretest and other Conns) or
// a WebSocket normal close handshake.
func gracefulEnd(err error) bool {
	return errors.Is(err, io.EOF) || websocket.CloseStatus(err) == websocket.StatusNormalClosure
}
