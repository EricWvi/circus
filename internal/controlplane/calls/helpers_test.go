package calls_test

import (
	"context"
	"errors"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
)

func tryReceive(t testing.TB, ctx context.Context, l *wire.Link, timeout time.Duration) (*circusv1.DaemonDownlink, bool) {
	t.Helper()
	short, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	m, err := l.Receive(short)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, false
		}
		t.Fatal(err)
	}
	return m.(*circusv1.DaemonDownlink), true
}

func statusCompleted(id string) *circusv1.DaemonUplink {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_COMPLETED, Result: &circusv1.DeviceResult{CallId: id, Output: &circusv1.ToolOutput{}}}}}
}

func statusRunning(id string) *circusv1.DaemonUplink {
	return &circusv1.DaemonUplink{Payload: &circusv1.DaemonUplink_CallStatus{CallStatus: &circusv1.CallStatus{CallId: id, State: circusv1.CallStatus_STATE_RUNNING}}}
}

func waitAnswer[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	return wait.Recv(t, ch)
}
