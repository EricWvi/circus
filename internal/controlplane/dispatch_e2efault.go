//go:build e2efault

package controlplane

import (
	"context"
	"os"
	"syscall"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/bus"
)

type faultBus struct{ bus.Bus }

func dispatchBus(b bus.Bus) bus.Bus { return faultBus{b} }
func (b faultBus) Send(ctx context.Context, target bus.Target, m *circusv1.DaemonDownlink) error {
	// Bus.Send is reached only after the dispatch transaction committed, before
	// gateway delivery can admit a DispatchCall to wire.
	if m.GetDispatchCall() != nil && os.Getenv("CIRCUS_E2E_CRASH_DISPATCH") == "1" {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	return b.Bus.Send(ctx, target, m)
}
