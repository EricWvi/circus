package controlplane

import (
	"context"
	"errors"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/workerlink"
	"github.com/EricWvi/circus/internal/wire"
)

// buildCallPlane composes the call path (registry, call router, daemon
// gateway and worker call handler) with this server's single authenticator,
// mounts both links, and registers its readiness check and its one shutdown
// step. On error it closes what it built.
func (s *Server) buildCallPlane(ctx context.Context) error {
	c := s.cfg
	registry, err := gateway.NewRegistry(100)
	if err != nil {
		return err
	}
	// Dispatch commit timestamps and Welcome share one clock on the daemon link.
	clk := clock.Real{}
	// The gateway calls the router's hooks, so it is built second; the
	// router reaches it through this sender, which is only used once a
	// device is connected and so after g is set.
	var g gateway.Handler
	sender := dispatchSender(calls.SenderFunc(func(ctx context.Context, t gateway.Target, m *circusv1.DaemonDownlink) error {
		if g == nil { // fail safe if a delivery ever precedes the binding
			return gateway.ErrNoRoute
		}
		return g.Deliver(ctx, t, m)
	}))
	r, err := calls.New(calls.Config{Store: s.st, Registry: registry, Sender: sender, Clock: clk, MaxMessageBytes: c.MaxMessageBytes, GracePeriod: c.GracePeriod})
	if err != nil {
		return err
	}
	opts := wire.Options{Clock: clk, MaxMessageBytes: c.MaxMessageBytes, HeartbeatInterval: c.HeartbeatInterval, HeartbeatTimeout: c.HeartbeatTimeout}
	g, err = gateway.New(gateway.Config{InstanceID: s.instanceID, Clock: clk, Auth: s.authn, Registry: registry, Hooks: r, Logger: s.logger, LinkOptions: opts, SendPoolBytes: c.SendPoolBytes, ReceivePoolBytes: c.ReceivePoolBytes})
	if err != nil {
		_ = r.Close(ctx)
		return err
	}
	w, err := workerlink.New(workerlink.Config{Auth: s.authn, Router: r, InstanceID: s.instanceID, Clock: clk, Wire: opts, Logger: s.logger})
	if err != nil {
		_ = g.Shutdown(ctx)
		_ = r.Close(ctx)
		return err
	}
	s.router = r
	s.mux.Handle("/v1/link/daemon", g)
	s.mux.Handle("/v1/link/worker", w)
	s.checks = append(s.checks, readyCheck{"calls", func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !r.Healthy() {
			return errors.New("call store unhealthy")
		}
		return nil
	}})
	s.closeCalls = func(ctx context.Context) error {
		return closeCallPlane(ctx, w, g, r, s.stopBg, s.timersDone)
	}
	s.hooks = append(s.hooks, shutdownHook{"call plane", s.closeCalls})
	return nil
}

type shutdownWorker interface {
	Shutdown(context.Context) error
}
type shutdownGateway interface {
	Shutdown(context.Context) error
}
type shutdownRouter interface {
	Close(context.Context) error
}

func closeCallPlane(ctx context.Context, w shutdownWorker, g shutdownGateway, r shutdownRouter, stopBg context.CancelFunc, timersDone <-chan struct{}) error {
	// Fixed sub-deadlines reserve roughly 20% for workers and 60% for the
	// gateway, leaving the rest for router/timer teardown. An early step
	// donates its unused time to later steps; none gets a new overall budget.
	begin := time.Now()
	deadline, bounded := ctx.Deadline()
	sub := func(portion float64) (context.Context, context.CancelFunc) {
		if !bounded {
			return ctx, func() {}
		}
		d := begin.Add(time.Duration(float64(deadline.Sub(begin)) * portion))
		return context.WithDeadline(ctx, d)
	}
	workerCtx, workerDone := sub(0.20)
	we := w.Shutdown(workerCtx)
	workerDone()
	gatewayCtx, gatewayDone := sub(0.80)
	// gateway.Shutdown deliberately joins disconnect delivery even after its
	// context expires; Router.Close must not run before that join completes.
	ge := g.Shutdown(gatewayCtx)
	gatewayDone()
	re := r.Close(ctx)
	var te error
	if stopBg != nil {
		stopBg()
		select {
		case <-timersDone:
		case <-ctx.Done():
			te = ctx.Err()
		}
	}
	return errors.Join(we, ge, re, te)
}
