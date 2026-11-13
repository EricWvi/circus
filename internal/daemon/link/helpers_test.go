package link_test

import (
	"context"
	"sync"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
)

// writeGate holds one selected physical frame while the real wire scheduler
// accepts later messages. It stores no frame copy and is cancellable by wire.
type writeGate struct {
	wire.Conn
	predicate       func(*circusv1.DaemonUplink) bool
	entered, permit chan struct{}
	once, openOnce  sync.Once
}

func (g *writeGate) open() { g.openOnce.Do(func() { close(g.permit) }) }
func (g *writeGate) WriteMessage(ctx context.Context, data []byte) error {
	m, err := wire.Decode(wire.DaemonUplink, data, 1<<20)
	if err != nil {
		return err
	}
	blocked := false
	if g.predicate(m.(*circusv1.DaemonUplink)) {
		g.once.Do(func() { blocked = true; close(g.entered) })
	}
	if blocked {
		select {
		case <-g.permit:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.Conn.WriteMessage(ctx, data)
}
