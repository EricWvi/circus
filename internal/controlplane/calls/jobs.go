package calls

import (
	"context"
	"log/slog"
	"math"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/identifier"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/wire"
)

// Job tracking is fenced by connection generation: one sequence cursor per
// current generation, not an event log. A superseded generation's events are
// dropped; the current generation's snapshot reconciles them.

func (r *router) OnJobEvent(ctx context.Context, conn gateway.Connection, event *circusv1.JobEvent) error {
	op, err := r.enter(ctx, true, "job-event")
	if err != nil {
		return err
	}
	defer op.finish()
	if err = validateJob(event); err != nil {
		return err
	}
	d := r.devices[conn.Target.DeviceID]
	if d == nil || !sameConnection(d.connection, conn) {
		return nil
	}
	// At or below the snapshot watermark the snapshot already covers it.
	if event.Seq <= d.snapshotSeq || event.Seq <= d.seq {
		return nil
	}
	if event.Seq != d.seq+1 && !d.snapshotPending {
		// One coalesced ListJobs per gap; snapshotPending bounds it to one.
		d.snapshotPending = true
		if err = r.config.Sender.Deliver(op.ctx, conn.Target, &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_ListJobs{ListJobs: &circusv1.ListJobs{}}}); err != nil {
			return err
		}
	}
	if _, err = r.config.Store.PutJob(op.ctx, r.job(conn, event)); err != nil {
		r.storeFailed(err)
		return err
	}
	d.seq = event.Seq
	return nil
}

func validateJob(j *circusv1.JobEvent) error {
	if j == nil || len(j.JobId) != 12 || !identifier.Valid(j.SessionId, false) || !identifier.Valid(j.CallId, false) || j.Seq == 0 || j.Seq > math.MaxInt64 {
		return protocol("invalid job identity or sequence")
	}
	for _, c := range j.JobId {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return protocol("invalid job id")
		}
	}
	switch j.State {
	case circusv1.JobEvent_STATE_RUNNING, circusv1.JobEvent_STATE_EXITED, circusv1.JobEvent_STATE_KILLED:
		return nil
	}
	return protocol("invalid daemon job state")
}

// job converts an event; JobEvent.output_path is never persisted.
func (r *router) job(conn gateway.Connection, event *circusv1.JobEvent) store.Job {
	j := store.Job{DeviceID: conn.Target.DeviceID, BootID: conn.Target.BootID, JobID: event.JobId, UserID: conn.UserID, SessionID: event.SessionId, CallID: event.CallId, LastSeq: int64(event.Seq), UpdatedAt: r.config.Clock.Now().UnixMilli()}
	switch event.State {
	case circusv1.JobEvent_STATE_RUNNING:
		j.State = store.JobRunning
		j.StartedAt = j.UpdatedAt
	case circusv1.JobEvent_STATE_EXITED, circusv1.JobEvent_STATE_KILLED:
		j.State = store.JobExited
		if event.State == circusv1.JobEvent_STATE_KILLED {
			j.State = store.JobKilled
		}
		exit := event.ExitCode
		j.ExitCode = &exit
		j.EndedAt = j.UpdatedAt
	}
	return j
}

func (r *router) OnJobSnapshot(ctx context.Context, conn gateway.Connection, s *circusv1.JobSnapshot) error {
	op, err := r.enter(ctx, true, "job-snapshot")
	if err != nil {
		return err
	}
	defer op.finish()
	if s == nil || s.Seq > math.MaxInt64 || len(s.Jobs) > wire.MaxSnapshotJobs {
		return protocol("invalid snapshot sequence or size")
	}
	r.replied(conn, listJobsKey)
	d := r.devices[conn.Target.DeviceID]
	if d == nil || !sameConnection(d.connection, conn) {
		return nil
	}
	if s.Seq < d.snapshotSeq {
		return nil
	}
	seen := make(map[string]struct{}, len(s.Jobs)) // bounded by MaxSnapshotJobs
	for _, j := range s.Jobs {
		if err = validateJob(j); err != nil {
			return err
		}
		if j.Seq > s.Seq {
			return protocol("job exceeds snapshot watermark")
		}
		if _, ok := seen[j.JobId]; ok {
			return protocol("duplicate snapshot job")
		}
		seen[j.JobId] = struct{}{}
	}
	err = r.config.Store.Tx(op.ctx, func(ctx context.Context, tx store.Ops) error {
		for _, j := range s.Jobs {
			if _, err := tx.PutJob(ctx, r.job(conn, j)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		r.storeFailed(err)
		return err
	}
	// The router currently requests only unfiltered snapshots. A truncated one
	// omitted jobs to fit its caps, so omissions prove nothing.
	if !s.Truncated {
		if err = r.settleOmissions(op.ctx, conn, int64(s.Seq), seen); err != nil {
			return err
		}
	}
	d.snapshotSeq = s.Seq
	d.seq = max(d.seq, s.Seq)
	d.snapshotPending = false
	return nil
}

// settleOmissions settles an unfiltered, untruncated snapshot of the current
// boot: a stored running job with LastSeq <= seq that the snapshot omits
// becomes lost. PutJob applies only strictly newer sequences,
// so the loss is written at seq, or at seq+1 when the job's last event is the
// watermark itself (lost is terminal and job writes are serialized, so no
// real event can collide). A newer live event keeps the job. The fixed reason
// has no job column, so it is logged, never stored.
func (r *router) settleOmissions(ctx context.Context, conn gateway.Connection, seq int64, seen map[string]struct{}) error {
	var after *store.JobCursor
	now := r.config.Clock.Now().UnixMilli()
	for {
		rows, err := r.config.Store.ListJobs(ctx, store.JobFilter{DeviceID: conn.Target.DeviceID, BootID: conn.Target.BootID, States: []store.JobState{store.JobRunning}, Limit: pageSize, After: after})
		if err != nil {
			r.storeFailed(err)
			return err
		}
		for _, j := range rows {
			if _, ok := seen[j.JobID]; ok || j.LastSeq > seq {
				continue
			}
			at := seq
			if j.LastSeq == seq {
				if seq == math.MaxInt64 {
					slog.Warn("daemon snapshot omits a running job at the maximum watermark", "device_id", j.DeviceID, "job_id", j.JobID)
					continue
				}
				at = seq + 1
			}
			gone := j
			gone.State, gone.LastSeq, gone.EndedAt, gone.UpdatedAt, gone.ExitCode = store.JobLost, at, now, now, nil
			changed, err := r.config.Store.PutJob(ctx, gone)
			if err != nil {
				r.storeFailed(err)
				return err
			}
			if changed {
				slog.Info("job lost", "device_id", j.DeviceID, "boot_id", j.BootID, "job_id", j.JobID, "reason", "missing from daemon snapshot")
			}
		}
		if len(rows) < pageSize {
			return nil
		}
		j := rows[len(rows)-1]
		after = &store.JobCursor{DeviceID: j.DeviceID, BootID: j.BootID, JobID: j.JobID}
	}
}
