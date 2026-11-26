package storetest

import (
	"context"

	"github.com/EricWvi/circus/internal/store"
)

func (o *faultyOps) ReportWorkerAgents(ctx context.Context, workerID string, from store.AgentReporter, report []store.AgentStatus, at int64) (v store.WorkerAgents, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.ReportWorkerAgents(ctx, workerID, from, report, at); return err })
	return
}
func (o *faultyOps) ListWorkerAgents(ctx context.Context, workerID string) (v store.WorkerAgents, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ListWorkerAgents(ctx, workerID); return err })
	return
}
func (o *faultyOps) ListUserWorkerAgents(ctx context.Context, userID string) (v []store.WorkerAgent, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ListUserWorkerAgents(ctx, userID); return err })
	return
}
func (o *faultyOps) SetWorkerAgentWish(ctx context.Context, workerID string, agent store.AgentKind, wanted bool, version string) (v bool, err error) {
	err = o.write(ctx, func(x store.Ops) error {
		v, err = x.SetWorkerAgentWish(ctx, workerID, agent, wanted, version)
		return err
	})
	return
}
func (o *faultyOps) HasAgentLease(ctx context.Context, workerID string, agent store.AgentKind, now int64) (v bool, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.HasAgentLease(ctx, workerID, agent, now); return err })
	return
}
