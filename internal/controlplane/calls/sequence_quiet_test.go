package calls_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
)

// The sequence harness's exact idleness check. settle's gauges (hooks,
// router operations, wire queues, tool states) do not see work in three
// places, and a goroutine descheduled there for a few milliseconds (a loaded
// machine) made settle end early, so the next operation saw a different
// state and a seed did not replay:
//
//   - a frame or a close between the two ends of a socket: the kernel holds
//     it, and the reader that will handle it is parked in IO wait;
//   - a message a reader took off its link and is still handling (a daemon
//     between Receive and its ack, the gateway between Receive and a hook);
//   - a tool goroutine sleeping before it records its start (the replay
//     test's shuffle).
//
// seqNet accounts every byte and close on the harness's loopback
// connections, busyGoroutine finds handlers in progress, and seqStarting
// counts sleeping tools. Together they report work in flight until the
// system has nothing left to do without a new operation or a clock advance.

// seqNet pairs the two ends of each TCP connection between the daemons and
// the test server. A direction is in flight while the receiving end is open
// and has not read every byte the sending end wrote, or the sending end
// closed and the receiving end has not yet read the end of the stream.
type seqNet struct {
	mu        sync.Mutex
	pairs     map[string]*seqPair     // by "client address>server address"
	listeners map[string]*seqListener // by listening address
	client    *http.Client
}

type seqPair struct {
	client, server *seqEnd
	listener       *seqListener
}

// seqEnd is one side of a connection; guarded by seqNet.mu.
type seqEnd struct {
	written, read int64
	closed        bool // this side closed its socket
	ended         bool // a Read on this side failed: it reads no more
}

func newSeqNet() *seqNet {
	n := &seqNet{pairs: make(map[string]*seqPair), listeners: make(map[string]*seqListener)}
	n.client = &http.Client{Transport: &http.Transport{DialContext: n.dial, DisableKeepAlives: true}}
	return n
}

// listen wraps a test server's listener; set it before the server starts.
func (n *seqNet) listen(l net.Listener) net.Listener {
	sl := &seqListener{Listener: l, n: n}
	n.mu.Lock()
	n.listeners[l.Addr().String()] = sl
	n.mu.Unlock()
	return sl
}

func (n *seqNet) dial(ctx context.Context, network, address string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	p := n.pair(c.LocalAddr().String() + ">" + c.RemoteAddr().String())
	p.client = &seqEnd{}
	p.listener = n.listeners[c.RemoteAddr().String()]
	return &seqNetConn{Conn: c, n: n, end: p.client}, nil
}

// pair returns the pair of a connection, creating it; n.mu must be held.
func (n *seqNet) pair(key string) *seqPair {
	p := n.pairs[key]
	if p == nil {
		p = &seqPair{}
		n.pairs[key] = p
	}
	return p
}

// inFlight describes data or a close still travelling on a connection, or
// returns "".
func (n *seqNet) inFlight() string {
	n.mu.Lock()
	defer n.mu.Unlock()
	for key, p := range n.pairs {
		switch {
		case p.client == nil || p.server == nil:
			if p.client != nil && p.client.closed && (p.listener == nil || p.listener.closed) {
				delete(n.pairs, key) // never accepted, and never will be
				continue
			}
			if p.listener == nil || !p.listener.closed {
				return "connection " + key + " still being set up"
			}
		case p.client.done() && p.server.done():
			delete(n.pairs, key)
		case p.client.sending(p.server):
			return "uplink bytes or close in flight on " + key
		case p.server.sending(p.client):
			return "downlink bytes or close in flight on " + key
		}
	}
	return ""
}

func (e *seqEnd) done() bool { return e.closed || e.ended }

// sending reports whether something e sent has not yet reached peer.
func (e *seqEnd) sending(peer *seqEnd) bool {
	if peer.done() {
		return false // the peer's socket discards it
	}
	return e.written > peer.read || e.closed
}

type seqListener struct {
	net.Listener
	n      *seqNet
	closed bool // guarded by n.mu
}

func (l *seqListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.n.mu.Lock()
	defer l.n.mu.Unlock()
	p := l.n.pair(c.RemoteAddr().String() + ">" + c.LocalAddr().String())
	p.server = &seqEnd{}
	p.listener = l
	return &seqNetConn{Conn: c, n: l.n, end: p.server}, nil
}

func (l *seqListener) Close() error {
	l.n.mu.Lock()
	l.closed = true
	l.n.mu.Unlock()
	return l.Listener.Close()
}

// seqNetConn counts one end's traffic. It deliberately has no NetConn
// method, so wire's abort handle closes through it.
type seqNetConn struct {
	net.Conn
	n   *seqNet
	end *seqEnd
}

func (c *seqNetConn) Read(p []byte) (int, error) {
	k, err := c.Conn.Read(p)
	c.n.mu.Lock()
	c.end.read += int64(k)
	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		c.end.ended = true
	}
	c.n.mu.Unlock()
	return k, err
}

func (c *seqNetConn) Write(p []byte) (int, error) {
	k, err := c.Conn.Write(p)
	c.n.mu.Lock()
	c.end.written += int64(k)
	c.n.mu.Unlock()
	return k, err
}

func (c *seqNetConn) Close() error {
	c.n.mu.Lock()
	c.end.closed = true
	c.n.mu.Unlock()
	return c.Conn.Close()
}

// busyGoroutine returns the header and top frame of a goroutine other than
// the caller that is running, runnable or in a system call, or "". A parked
// goroutine waits for a channel, lock, timer or socket; the socket case is
// covered by seqNet, so all parked means nothing is being handled.
func busyGoroutine() string {
	buf := make([]byte, 256<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	// The caller's own trace comes first.
	_, others, _ := strings.Cut(string(buf), "\n\n")
	for g := range strings.SplitSeq(others, "\n\n") {
		header, frames, _ := strings.Cut(g, "\n")
		_, state, ok := strings.Cut(header, " [")
		if !ok {
			continue
		}
		state, _, _ = strings.Cut(state, "]")
		state, _, _ = strings.Cut(state, ",")
		if strings.Contains(frames, "/internal/testutil/leakcheck.") {
			continue // the process leak checker's sampler, not the system under test
		}
		switch state {
		case "running", "runnable", "syscall", "preempted", "copystack", "GC assist wait":
			top, _, _ := strings.Cut(frames, "\n")
			return header + " " + top
		}
	}
	return ""
}

// seqStarting counts tool goroutines in their start shuffle.
var seqStarting atomic.Int64

// inFlight reports work settle's gauges cannot see, or "". It stops the
// world to read goroutine states, so settle asks only once its gauges are
// idle.
func (w *seqWorld) inFlight() string {
	if n := seqStarting.Load(); n > 0 {
		return fmt.Sprintf("%d tools starting", n)
	}
	if reason := w.net.inFlight(); reason != "" {
		return reason
	}
	if g := busyGoroutine(); g != "" {
		return "goroutine busy: " + g
	}
	return ""
}
