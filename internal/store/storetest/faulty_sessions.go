package storetest

import (
	"context"

	"github.com/EricWvi/circus/internal/store"
)

// Session operations. CheckSessionDeletable, CheckFence, AdmitSessionCall, LoadTranscript,
// HasTranscriptUUID and ReadEvents are reads, like Get* and List*; every
// other method is a write point. DeleteSession is one write point however
// many transactions it runs.

func (o *faultyOps) CreateSession(ctx context.Context, s store.Session, cwds []store.SessionCwd, maxSessions int) error {
	return o.write(ctx, func(x store.Ops) error { return x.CreateSession(ctx, s, cwds, maxSessions) })
}

func (o *faultyOps) GetSession(ctx context.Context, sessionID string, now int64) (v store.Session, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetSession(ctx, sessionID, now); return err })
	return v, err
}

func (o *faultyOps) ListSessions(ctx context.Context, f store.SessionFilter) (v []store.Session, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ListSessions(ctx, f); return err })
	return v, err
}

func (o *faultyOps) StopSession(ctx context.Context, sessionID string, at int64, reason string) (s store.Session, l store.Lease, changed bool, err error) {
	err = o.write(ctx, func(x store.Ops) error { s, l, changed, err = x.StopSession(ctx, sessionID, at, reason); return err })
	return s, l, changed, err
}

func (o *faultyOps) DeleteSession(ctx context.Context, sessionID string, now int64) error {
	return o.write(ctx, func(x store.Ops) error { return x.DeleteSession(ctx, sessionID, now) })
}

func (o *faultyOps) TranscriptRepaired(ctx context.Context, f store.Fence) (v store.Session, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.TranscriptRepaired(ctx, f); return err })
	return v, err
}

func (o *faultyOps) InterruptSession(ctx context.Context, sessionID string, at int64) (s store.Session, l store.Lease, changed bool, err error) {
	err = o.write(ctx, func(x store.Ops) error { s, l, changed, err = x.InterruptSession(ctx, sessionID, at); return err })
	return s, l, changed, err
}

func (o *faultyOps) SettleSession(ctx context.Context, sessionID string, now int64) (v store.SessionSettled, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.SettleSession(ctx, sessionID, now); return err })
	return v, err
}

func (o *faultyOps) AcquireLease(ctx context.Context, r store.LeaseRequest) (v store.LeaseAcquired, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.AcquireLease(ctx, r); return err })
	return v, err
}

func (o *faultyOps) GetRecovery(ctx context.Context, f store.Fence) (v store.Recovery, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetRecovery(ctx, f); return err })
	return v, err
}

func (o *faultyOps) GetLease(ctx context.Context, sessionID string) (v store.Lease, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetLease(ctx, sessionID); return err })
	return v, err
}

func (o *faultyOps) ExtendLeases(ctx context.Context, keys []store.LeaseKey, now, expiresAt int64) (v []store.LeaseKey, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.ExtendLeases(ctx, keys, now, expiresAt); return err })
	return v, err
}

func (o *faultyOps) ReleaseLease(ctx context.Context, f store.Fence) (v store.LeaseLoss, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.ReleaseLease(ctx, f); return err })
	return v, err
}

func (o *faultyOps) ReleaseWorkerLeases(ctx context.Context, workerID, keepBootID string, now int64, limit int) (v []store.LeaseLoss, err error) {
	err = o.write(ctx, func(x store.Ops) error {
		v, err = x.ReleaseWorkerLeases(ctx, workerID, keepBootID, now, limit)
		return err
	})
	return v, err
}

func (o *faultyOps) ResumeLeases(ctx context.Context, workerID, bootID, connID string, keys []store.SessionEpoch, now, expiresAt int64) (v []store.LeaseResumed, err error) {
	err = o.write(ctx, func(x store.Ops) error {
		v, err = x.ResumeLeases(ctx, workerID, bootID, connID, keys, now, expiresAt)
		return err
	})
	return v, err
}

func (o *faultyOps) ExpireLeases(ctx context.Context, now int64, limit int) (v []store.LeaseLoss, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.ExpireLeases(ctx, now, limit); return err })
	return v, err
}

func (o *faultyOps) CheckFence(ctx context.Context, f store.Fence) (v store.Lease, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.CheckFence(ctx, f); return err })
	return v, err
}

func (o *faultyOps) AdmitSessionCall(ctx context.Context, f store.Fence, callID string, maxActive int) error {
	return o.read(ctx, func(x store.Ops) error { return x.AdmitSessionCall(ctx, f, callID, maxActive) })
}

func (o *faultyOps) AppendTranscript(ctx context.Context, f store.Fence, b store.TranscriptBatch, q store.TranscriptQuota) (v store.TranscriptAppended, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.AppendTranscript(ctx, f, b, q); return err })
	return v, err
}

func (o *faultyOps) LoadTranscript(ctx context.Context, f store.Fence, r store.TranscriptPageRequest) (v store.TranscriptPage, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.LoadTranscript(ctx, f, r); return err })
	return v, err
}

func (o *faultyOps) ListTranscriptSubpaths(ctx context.Context, f store.Fence) (v []store.TranscriptSubpath, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ListTranscriptSubpaths(ctx, f); return err })
	return v, err
}

func (o *faultyOps) HasTranscriptUUID(ctx context.Context, f store.Fence, uuid string) (v bool, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.HasTranscriptUUID(ctx, f, uuid); return err })
	return v, err
}

func (o *faultyOps) SubmitInput(ctx context.Context, in store.Input) (v store.InputSubmitted, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.SubmitInput(ctx, in); return err })
	return v, err
}

func (o *faultyOps) TakeInputs(ctx context.Context, f store.Fence) (v []store.Input, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.TakeInputs(ctx, f); return err })
	return v, err
}

func (o *faultyOps) ReportTurn(ctx context.Context, f store.Fence, r store.TurnReport) (v store.TurnReported, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.ReportTurn(ctx, f, r); return err })
	return v, err
}

func (o *faultyOps) ListTurns(ctx context.Context, f store.TurnFilter) (v []store.Turn, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ListTurns(ctx, f); return err })
	return v, err
}

func (o *faultyOps) PatchSessionRecords(ctx context.Context, f store.Fence, p store.RecordsPatch) error {
	return o.write(ctx, func(x store.Ops) error { return x.PatchSessionRecords(ctx, f, p) })
}

func (o *faultyOps) GetSessionRecords(ctx context.Context, f store.Fence, r store.RecordsRequest) (v store.SessionRecords, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetSessionRecords(ctx, f, r); return err })
	return v, err
}

func (o *faultyOps) PurgeSessionJobs(ctx context.Context, before int64, limit int) (n int, err error) {
	err = o.write(ctx, func(x store.Ops) error { n, err = x.PurgeSessionJobs(ctx, before, limit); return err })
	return n, err
}

func (o *faultyOps) AppendEvents(ctx context.Context, sessionID string, pub *store.EventPublish, events []store.EventInput, maxViewBytes int64) (v store.EventsAppended, err error) {
	err = o.write(ctx, func(x store.Ops) error {
		v, err = x.AppendEvents(ctx, sessionID, pub, events, maxViewBytes)
		return err
	})
	return v, err
}

func (o *faultyOps) ReadEvents(ctx context.Context, f store.EventFilter) (v store.EventPage, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ReadEvents(ctx, f); return err })
	return v, err
}

func (o *faultyOps) RequestApproval(ctx context.Context, f store.Fence, a store.Approval) (v store.ApprovalRequested, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.RequestApproval(ctx, f, a); return err })
	return v, err
}

func (o *faultyOps) ResolveApproval(ctx context.Context, r store.ApprovalResolution) (v store.ApprovalResolved, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.ResolveApproval(ctx, r); return err })
	return v, err
}

func (o *faultyOps) GetApproval(ctx context.Context, approvalID string) (v store.Approval, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.GetApproval(ctx, approvalID); return err })
	return v, err
}

func (o *faultyOps) ListApprovals(ctx context.Context, f store.ApprovalFilter) (v []store.Approval, err error) {
	err = o.read(ctx, func(x store.Ops) error { v, err = x.ListApprovals(ctx, f); return err })
	return v, err
}

func (o *faultyOps) ConsumeGrant(ctx context.Context, f store.Fence, k store.GrantKey, toolUseID string) (v bool, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.ConsumeGrant(ctx, f, k, toolUseID); return err })
	return v, err
}

func (o *faultyOps) ExtendLeasesAfterRestart(ctx context.Context, expiresAt int64) (n int, err error) {
	err = o.write(ctx, func(x store.Ops) error { n, err = x.ExtendLeasesAfterRestart(ctx, expiresAt); return err })
	return n, err
}

func (o *faultyOps) RecoverSession(ctx context.Context, sessionID string, accept bool, at int64, maxViewBytes int64) (v store.SessionRecovered, err error) {
	err = o.write(ctx, func(x store.Ops) error {
		v, err = x.RecoverSession(ctx, sessionID, accept, at, maxViewBytes)
		return err
	})
	return v, err
}

func (o *faultyOps) ExpireApprovals(ctx context.Context, now int64, limit int) (v []store.Approval, err error) {
	err = o.write(ctx, func(x store.Ops) error { v, err = x.ExpireApprovals(ctx, now, limit); return err })
	return v, err
}

func (o *faultyOps) SetSessionTitle(ctx context.Context, sessionID, title string) error {
	return o.write(ctx, func(x store.Ops) error { return x.SetSessionTitle(ctx, sessionID, title) })
}

func (o *faultyOps) CheckSessionDeletable(ctx context.Context, sessionID string, now int64) (err error) {
	return o.read(ctx, func(x store.Ops) error { return x.CheckSessionDeletable(ctx, sessionID, now) })
}
