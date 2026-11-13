package wire

import (
	"context"
	"errors"
	"strings"
	"testing"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"google.golang.org/protobuf/proto"
)

// chanConn is a minimal Conn; wiretest cannot be imported here (import cycle).
type chanConn struct {
	frames chan []byte
	closed chan struct{}
}

func (c *chanConn) ReadMessage(ctx context.Context) ([]byte, error) {
	select {
	case f := <-c.frames:
		return f, nil
	case <-c.closed:
		return nil, errors.New("closed")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
func (c *chanConn) WriteMessage(context.Context, []byte) error { return nil }
func (c *chanConn) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}
func (c *chanConn) SetReadLimit(int64)             {}
func (c *chanConn) Shutdown(context.Context) error { return c.Close() }

// A failure between admission and acceptance must not let the read loop append
// a message after fail dropped the shared charges.
func TestNoAcceptanceAfterFailure(t *testing.T) {
	o := Options{MaxFragmentBytes: 64 << 10, MaxMessageBytes: 64 << 10, DisableHeartbeat: true}
	o.Budget, _ = NewBudget(minBudget(func() Options { d, _ := o.defaults(); return d }()))
	conn := &chanConn{frames: make(chan []byte, 1), closed: make(chan struct{})}
	l, err := New(conn, DaemonControlSide, o)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.testAdmitted = func() { l.fail(errors.New("injected")) }
	m := &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_Result{Result: &circusv1.DeviceResult{CallId: "c", Output: &circusv1.ToolOutput{Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: strings.Repeat("x", 30<<10)}}}}}}}
	data, _ := proto.Marshal(m)
	conn.frames <- data // the channel send orders the hook write before the reader's use
	wait.Recv(t, l.Done())
	l.mu.Lock()
	n := len(l.received)
	l.mu.Unlock()
	if s := o.Budget.Stats(); n != 0 || s.Receive.Used != 0 || s.Receive.Peak == 0 {
		t.Fatalf("accepted after failure: received=%d %+v", n, s)
	}
}
