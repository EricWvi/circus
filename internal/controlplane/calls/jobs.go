package calls

import (
	"context"
	"errors"
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
	j := r.job(conn, event)
	wake := false
	err = r.config.Store.Tx(op.ctx, func(ctx context.Context, tx store.Ops) error {
		wake = false
		changed, err := tx.PutJob(ctx, j)
		if err != nil || !changed {
			return err
		}
		if err = registerLostCallJob(ctx, tx, j, event.OutputPath); err != nil {
			return err
		}
		wake, err = r.endWake(ctx, tx, j)
		return err
	})
	if err != nil {
		r.storeFailed(err)
		return err
	}
	if wake {
		r.jobEnded(j.SessionID)
	}
	d.seq = event.Seq
	return nil
}

// endWake reports, for a stored job j that just changed, whether its end
// wakes its session: j is terminal and names a session of the job's own
// user (the daemon's connection's). The session id is the daemon's claim,
// so another user's or a made-up session is never woken.
func (r *router) endWake(ctx context.Context, ops store.Ops, j store.Job) (bool, error) {
	if !j.State.Terminal() || j.SessionID == "" || r.config.JobEnded == nil {
		return false, nil
	}
	s, err := ops.GetSession(ctx, j.SessionID, r.config.Clock.Now().UnixMilli())
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return s.UserID == j.UserID, nil
}

// jobEnded tells the session scheduler that a job of the session ended (or
// was registered already ended), so the session's holder fetches the
// notice or the session is offered.
func (r *router) jobEnded(sessionID string) {
	if r.config.JobEnded != nil {
		r.config.JobEnded(sessionID)
	}
}

// maxLostCallJobs bounds the jobs looked up for one lost call (a call
// starts at most one; more rows only come from a non-conforming daemon).
const maxLostCallJobs = 4

// The jobs of a call settled LOST: its result never arrived (or arrived
// after the loss), so complete never registered the job, and the session's
// holder would never learn that it ended. They are registered when a
// changed job event or snapshot entry names the call (registerLostCallJob),
// and at the loss and at a late result from the jobs already stored for the
// call (registerJobsOfLostCall). A job's call and session are write-once in
// the jobs table, so a job named under another call later stays with its
// first one.

// registerLostCallJob registers, in tx, the changed job j when the call it
// names settled LOST; see registerLostJob for the checks. The caller's end
// wake (endWake) then tells the holder of an ended job.
func registerLostCallJob(ctx context.Context, tx store.Ops, j store.Job, outputPath string) error {
	c, err := tx.GetCall(ctx, j.CallID)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = registerLostJob(ctx, tx, c, j, outputPath)
	return err
}

// registerJobsOfLostCall registers, in tx, the stored jobs of the LOST call
// c, looked up by its device, dispatch boot and call id. outputPath is used
// for the job with id jobID (a late result's). It reports whether one that
// already ended was registered, which the caller wakes the session for.
func registerJobsOfLostCall(ctx context.Context, tx store.Ops, c store.Call, jobID, outputPath string) (bool, error) {
	if c.State != store.CallLost || c.ToolUseID == "" || c.DispatchedBootID == "" {
		return false, nil
	}
	jobs, err := tx.ListJobs(ctx, store.JobFilter{DeviceID: c.DeviceID, BootID: c.DispatchedBootID, CallID: c.CallID, Limit: maxLostCallJobs})
	if err != nil {
		return false, err
	}
	wake := false
	for _, j := range jobs {
		path := ""
		if j.JobID == jobID {
			path = outputPath
		}
		ok, err := registerLostJob(ctx, tx, c, j, path)
		if err != nil {
			return false, err
		}
		wake = wake || ok && j.State.Terminal()
	}
	return wake, nil
}

// registerLostJob registers, in tx, the job j with the session of call c
// when c settled LOST. Like complete, it registers only for a call admitted
// under the session's lease (it carries a tool_use_id), and only when j is
// the call's: the same user, session, device, dispatch boot and call.
// Refusals are handled by registerJob. A repeat updates the row.
func registerLostJob(ctx context.Context, tx store.Ops, c store.Call, j store.Job, outputPath string) (bool, error) {
	if c.State != store.CallLost || c.ToolUseID == "" || c.CallID != j.CallID || c.UserID != j.UserID || c.SessionID != j.SessionID ||
		c.DeviceID != j.DeviceID || c.DispatchedBootID != j.BootID {
		return false, nil
	}
	if len(outputPath) > store.MaxPathBytes {
		outputPath = ""
	}
	return registerJob(ctx, tx, store.SessionJob{CallID: c.CallID, JobID: j.JobID, ToolUseID: c.ToolUseID, OutputPath: outputPath, CreatedAt: j.UpdatedAt})
}

// registerJob registers job in tx. Registration is a side effect of call
// settlement, job events and device connects, and never fails them for a
// refusal the daemon's reports can provoke: a full job table (ErrLimit), a
// job the table cannot hold (ErrInvalid), or a device job already
// registered under another call (ErrExists, a daemon naming one job under
// two calls). Those are logged and skipped (only the registration's own
// writes roll back); other store errors fail the caller's Tx.
func registerJob(ctx context.Context, tx store.Ops, job store.SessionJob) (bool, error) {
	ok, err := tx.RegisterSessionJob(ctx, job)
	if errors.Is(err, store.ErrLimit) || errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrExists) {
		slog.Warn("session job not registered", "call_id", job.CallID, "job_id", job.JobID, "error", err)
		return false, nil
	}
	return ok, err
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
	ended := make([]store.Job, 0, len(s.Jobs)) // bounded by MaxSnapshotJobs
	err = r.config.Store.Tx(op.ctx, func(ctx context.Context, tx store.Ops) error {
		ended = ended[:0]
		for _, e := range s.Jobs {
			j := r.job(conn, e)
			changed, err := tx.PutJob(ctx, j)
			if err != nil {
				return err
			}
			if !changed {
				continue
			}
			if err = registerLostCallJob(ctx, tx, j, e.OutputPath); err != nil {
				return err
			}
			if wake, err := r.endWake(ctx, tx, j); err != nil {
				return err
			} else if wake {
				ended = append(ended, j)
			}
		}
		return nil
	})
	if err != nil {
		r.storeFailed(err)
		return err
	}
	for _, j := range ended {
		r.jobEnded(j.SessionID)
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
				wake, err := r.endWake(ctx, r.config.Store, gone)
				if err != nil {
					r.storeFailed(err)
					return err
				}
				if wake {
					r.jobEnded(gone.SessionID)
				}
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
