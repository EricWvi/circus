package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/bus"
	"github.com/EricWvi/circus/internal/controlplane/calls"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/workerlink"
	"github.com/EricWvi/circus/internal/wire"
)

// LinkConfig exposes the operational transport and disconnect limits. Zero uses
// package defaults. ConfigureLinks must run once, before Start.
type LinkConfig struct {
	MaxMessageBytes                     int
	GracePeriod                         time.Duration
	HeartbeatInterval, HeartbeatTimeout time.Duration
}

// ConfigureLinks composes the M2 call path using this server's authenticator.
func (s *Server) ConfigureLinks(ctx context.Context, c LinkConfig) error {
	s.mu.Lock()
	s.mustNotHaveStarted("ConfigureLinks")
	s.mu.Unlock()
	if s.router != nil {
		return errors.New("controlplane: links already configured")
	}
	registry, err := gateway.NewRegistry(100)
	if err != nil {
		return err
	}
	b, err := bus.New(bus.Limits{MaxInstances: 1, MaxInFlight: 128, MaxBytes: 256 << 20})
	if err != nil {
		return err
	}
	b = dispatchBus(b)
	r, err := calls.New(calls.Config{Store: s.st, Registry: registry, Bus: b, Clock: clock.Real{}, MaxMessageBytes: c.MaxMessageBytes, GracePeriod: c.GracePeriod})
	if err != nil {
		return err
	}
	opts := wire.Options{MaxMessageBytes: c.MaxMessageBytes, HeartbeatInterval: c.HeartbeatInterval, HeartbeatTimeout: c.HeartbeatTimeout}
	g, err := gateway.New(gateway.Config{InstanceID: s.instanceID, Clock: clock.Real{}, Auth: daemonAuth{s.authn, s.st}, Registry: registry, Bus: b, Hooks: r, Logger: s.logger, LinkOptions: opts})
	if err != nil {
		_ = r.Close(ctx)
		return err
	}
	w, err := workerlink.New(workerlink.Config{Auth: s.authn, Router: r, InstanceID: s.instanceID, Wire: opts, Logger: s.logger})
	if err != nil {
		_ = g.Shutdown(ctx)
		_ = r.Close(ctx)
		return err
	}
	s.router = r
	s.Mount("/v1/link/daemon", g)
	s.Mount("/v1/link/worker", w)
	s.AddReadyCheck("calls", func(ctx context.Context) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !r.Healthy() {
			return errors.New("call store unhealthy")
		}
		return nil
	})
	s.closeLinks = func(ctx context.Context) error {
		return closeCallPlane(ctx, w, g, r, s.stopBg, s.timersDone)
	}
	s.AddShutdownHook("call plane", s.closeLinks)
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
