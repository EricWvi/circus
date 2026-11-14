//go:build e2efault

package controlplane

import (
	"context"
	"os"
	"syscall"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
)

type faultSender struct{ calls.Sender }

func dispatchSender(s calls.Sender) calls.Sender { return faultSender{s} }
func (s faultSender) Deliver(ctx context.Context, target gateway.Target, m *circusv1.DaemonDownlink) error {
	// Deliver is reached only after the dispatch transaction committed, before
	// the gateway can admit a DispatchCall to wire.
	if m.GetDispatchCall() != nil && os.Getenv("CIRCUS_E2E_CRASH_DISPATCH") == "1" {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	return s.Sender.Deliver(ctx, target, m)
}
