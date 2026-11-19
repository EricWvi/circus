package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/EricWvi/circus/internal/store"
)

func checkRecordsPatch(p store.RecordsPatch) error {
	n := len(p.Cwds) + len(p.Files) + len(p.Jobs) + len(p.ReturnedCallIDs) + len(p.AwaitingCallIDs) +
		len(p.HandedJobCalls) + len(p.BgTasksStarted) + len(p.BgTasksEnded) + p.Persisted.CountIDs()
	if n > store.MaxPatchItems {
		return invalid("%d patch items, at most %d", n, store.MaxPatchItems)
	}
	if len(p.AwaitingCallIDs) > store.MaxOwedCalls {
		return invalid("%d awaiting call ids, at most %d", len(p.AwaitingCallIDs), store.MaxOwedCalls)
	}
	for _, c := range p.Cwds {
		if err := checkCwd(c); err != nil {
			return err
		}
	}
	for _, f := range p.Files {
		if err := store.CheckID("device id", f.DeviceID); err != nil {
			return err
		}
		if err := store.CheckText("file path", f.Path, store.MaxPathBytes, false); err != nil {
			return err
		}
		if len(f.SHA256) != 0 && len(f.SHA256) != 32 {
			return invalid("file record hash must be 32 bytes or empty")
		}
	}
	for _, j := range p.Jobs {
		for _, c := range []struct{ what, v string }{{"call id", j.CallID}, {"job id", j.JobID}} {
			if err := store.CheckID(c.what, c.v); err != nil {
				return err
			}
		}
		if err := store.CheckOptionalID("tool use id", j.ToolUseID); err != nil {
			return err
		}
		if err := store.CheckText("job description", j.Description, store.MaxDescriptionBytes, true); err != nil {
			return err
		}
		if err := store.CheckText("job output path", j.OutputPath, store.MaxPathBytes, true); err != nil {
			return err
		}
	}
	for _, b := range p.BgTasksStarted {
		if err := store.CheckID("task id", b.TaskID); err != nil {
			return err
		}
		if err := store.CheckOptionalID("tool use id", b.ToolUseID); err != nil {
			return err
		}
		if b.Kind != "agent" {
			return invalid("background task kind %q, want agent", b.Kind)
		}
		if err := store.CheckText("task description", b.Description, store.MaxDescriptionBytes, true); err != nil {
			return err
		}
	}
	for _, l := range []struct {
		what string
		ids  []string
	}{{"returned call id", p.ReturnedCallIDs}, {"awaiting call id", p.AwaitingCallIDs}, {"job call id", p.HandedJobCalls}, {"task id", p.BgTasksEnded}} {
		if err := store.CheckIDs(l.what, l.ids); err != nil {
			return err
		}
	}
	return p.Persisted.Check()
}

func (o *ops) count(ctx context.Context, query string, args ...any) (int, error) {
	var n int
	err := o.q.QueryRow(ctx, query, args...).Scan(&n)
	return n, mapErr(err)
}

// inIDs runs query, which ends with "IN ", on pre and the ids.
func (o *ops) inIDs(ctx context.Context, query string, ids []string, pre ...any) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := o.exec(ctx, query+`(`+placeholders(len(ids))+`)`, append(pre, idArgs(ids)...)...)
	return err
}

func (o *ops) PatchSessionRecords(ctx context.Context, f store.Fence, p store.RecordsPatch) error {
	if err := checkRecordsPatch(p); err != nil {
		return err
	}
	return o.fenced(ctx, f, func(tx *ops) error {
		sid, now := f.SessionID, f.Now
		for _, c := range p.Cwds {
			at := c.UpdatedAt
			if at == 0 {
				at = now
			}
			if _, err := tx.exec(ctx, `INSERT INTO session_cwds (session_id, agent_id, device_id, cwd, updated_at)
				VALUES (?,?,?,?,?) ON CONFLICT DO UPDATE SET cwd = excluded.cwd, updated_at = excluded.updated_at`,
				sid, c.AgentID, c.DeviceID, c.Cwd, at); err != nil {
				return err
			}
		}
		if n, err := tx.count(ctx, `SELECT COUNT(*) FROM session_cwds WHERE session_id = ?`, sid); err != nil {
			return err
		} else if n > store.MaxSessionCwds {
			return fmt.Errorf("%w: %d working directories", store.ErrLimit, n)
		}

		for _, fr := range p.Files {
			if len(fr.SHA256) == 0 {
				if _, err := tx.exec(ctx, `DELETE FROM session_files WHERE session_id = ? AND device_id = ? AND path = ?`,
					sid, fr.DeviceID, fr.Path); err != nil {
					return err
				}
				continue
			}
			at := fr.TouchedAt
			if at == 0 {
				at = now
			}
			if _, err := tx.exec(ctx, `INSERT INTO session_files (session_id, device_id, path, sha256, mtime_ms, touched_at)
				VALUES (?,?,?,?,?,?) ON CONFLICT DO UPDATE SET sha256 = excluded.sha256, mtime_ms = excluded.mtime_ms,
				touched_at = excluded.touched_at`, sid, fr.DeviceID, fr.Path, fr.SHA256, fr.MtimeMs, at); err != nil {
				return err
			}
		}
		if len(p.Files) > 0 {
			if _, err := tx.exec(ctx, `DELETE FROM session_files WHERE rowid IN (
				SELECT rowid FROM session_files WHERE session_id = ?1 ORDER BY touched_at, device_id, path
				LIMIT max(0, (SELECT COUNT(*) FROM session_files WHERE session_id = ?1) - ?2))`,
				sid, store.MaxSessionFiles); err != nil {
				return err
			}
		}

		for _, j := range p.Jobs {
			var device, boot string
			err := mapErr(tx.q.QueryRow(ctx, `SELECT c.device_id, c.dispatched_boot_id FROM calls c
				JOIN sessions s ON s.session_id = c.session_id AND s.user_id = c.user_id
				WHERE c.call_id = ? AND c.session_id = ?`, j.CallID, sid).Scan(&device, nullStr{&boot}))
			if err != nil && !isNotFound(err) {
				return err
			}
			if err != nil || boot == "" {
				return fmt.Errorf("%w: dispatched call %q of this session", store.ErrNotFound, j.CallID)
			}
			if _, err := tx.exec(ctx, `INSERT INTO session_jobs (session_id, call_id, device_id, boot_id, job_id, tool_use_id,
				description, output_path, created_at) VALUES (?,?,?,?,?,?,?,?,?)
				ON CONFLICT (session_id, call_id) DO UPDATE SET job_id = excluded.job_id, tool_use_id = excluded.tool_use_id,
				description = excluded.description, output_path = excluded.output_path`,
				sid, j.CallID, device, boot, j.JobID, nstr(j.ToolUseID), nstr(j.Description), nstr(j.OutputPath), now); err != nil {
				return err
			}
		}
		if n, err := tx.count(ctx, `SELECT COUNT(*) FROM session_jobs WHERE session_id = ? AND `+sessionJobDeviceMatchesOwner, sid); err != nil {
			return err
		} else if n > store.MaxSessionJobs {
			return fmt.Errorf("%w: %d session jobs", store.ErrLimit, n)
		}

		if err := tx.inIDs(ctx, `UPDATE calls SET returned_at = ? WHERE session_id = ? AND `+callMatchesSessionOwner+` AND returned_at IS NULL AND call_id IN `,
			p.ReturnedCallIDs, now, sid); err != nil {
			return err
		}
		if err := tx.inIDs(ctx, `UPDATE calls SET notice_owed = 1 WHERE session_id = ? AND `+callMatchesSessionOwner+` AND call_id IN `,
			p.AwaitingCallIDs, sid); err != nil {
			return err
		}
		if n, err := tx.count(ctx, `SELECT COUNT(*) FROM calls WHERE session_id = ? AND `+callMatchesSessionOwner+` AND notice_owed = 1`, sid); err != nil {
			return err
		} else if n > store.MaxOwedCalls {
			return fmt.Errorf("%w: %d calls owe a notice", store.ErrLimit, n)
		}
		if err := tx.inIDs(ctx, `UPDATE session_jobs SET notice = 'handed', notice_at = ? WHERE session_id = ?
			AND `+sessionJobDeviceMatchesOwner+` AND notice = 'no' AND call_id IN `, p.HandedJobCalls, now, sid); err != nil {
			return err
		}

		for _, b := range p.BgTasksStarted {
			if _, err := tx.exec(ctx, `INSERT INTO session_bgtasks (session_id, task_id, tool_use_id, kind, description,
				epoch, started_at) VALUES (?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
				sid, b.TaskID, nstr(b.ToolUseID), b.Kind, nstr(b.Description), f.Epoch, now); err != nil {
				return err
			}
		}
		if err := tx.inIDs(ctx, `DELETE FROM session_bgtasks WHERE session_id = ? AND task_id IN `, p.BgTasksEnded, sid); err != nil {
			return err
		}
		if n, err := tx.count(ctx, `SELECT COUNT(*) FROM session_bgtasks WHERE session_id = ?`, sid); err != nil {
			return err
		} else if n > store.MaxSessionBgTasks {
			return fmt.Errorf("%w: %d background tasks", store.ErrLimit, n)
		}
		return tx.markPersisted(ctx, f, p.Persisted, now)
	})
}

const callMatchesSessionOwner = `user_id = (SELECT s.user_id FROM sessions s WHERE s.session_id = calls.session_id)`

const sessionJobDeviceMatchesOwner = `EXISTS (SELECT 1 FROM sessions s JOIN devices d ON d.user_id = s.user_id
	WHERE s.session_id = session_jobs.session_id AND d.id = session_jobs.device_id)`

// sessionJobQuery selects a session's jobs, with the state of their jobs
// row, for scanSessionJob; it takes the session id.
const sessionJobQuery = `SELECT sj.call_id, sj.device_id, sj.boot_id, sj.job_id, sj.tool_use_id,
	sj.description, sj.output_path, j.state, j.exit_code, j.ended_at, sj.notice, sj.notice_at, sj.created_at
	FROM session_jobs sj
	JOIN sessions s ON s.session_id = sj.session_id
	JOIN devices d ON d.id = sj.device_id AND d.user_id = s.user_id
	LEFT JOIN jobs j ON j.device_id = sj.device_id AND j.boot_id = sj.boot_id AND j.job_id = sj.job_id AND j.user_id = s.user_id
	WHERE sj.session_id = ?`

func scanSessionJob(s scanner) (store.SessionJob, error) {
	var x store.SessionJob
	var state, notice string
	err := s.Scan(&x.CallID, &x.DeviceID, &x.BootID, &x.JobID, nullStr{&x.ToolUseID}, nullStr{&x.Description},
		nullStr{&x.OutputPath}, nullStr{&state}, nullExit{&x.ExitCode}, nullInt{&x.EndedAt}, &notice,
		nullInt{&x.NoticeAt}, &x.CreatedAt)
	x.State, x.Notice = store.JobState(state), store.JobNotice(notice)
	return x, mapErr(err)
}

const bgTaskCols = `task_id, tool_use_id, kind, description, epoch, started_at`

func scanBgTask(s scanner) (store.BgTask, error) {
	var b store.BgTask
	return b, mapErr(s.Scan(&b.TaskID, nullStr{&b.ToolUseID}, &b.Kind, nullStr{&b.Description}, &b.Epoch, &b.StartedAt))
}

func scanCwd(s scanner) (store.SessionCwd, error) {
	var c store.SessionCwd
	return c, mapErr(s.Scan(&c.AgentID, &c.DeviceID, &c.Cwd, &c.UpdatedAt))
}

func scanFile(s scanner) (store.SessionFile, error) {
	var x store.SessionFile
	return x, mapErr(s.Scan(&x.DeviceID, &x.Path, &x.SHA256, &x.MtimeMs, &x.TouchedAt))
}

// pageBudget tracks one page's size: rows stop once the next would bring
// it over max, but the page always gets one row.
type pageBudget struct {
	max, used int64
	rows      int
}

// pageRows appends rows to dst while the budget allows, and reports
// whether it stopped on the budget (more rows follow).
func pageRows[T interface{ Size() int64 }](rows *sql.Rows, scan func(scanner) (T, error), dst *[]T, b *pageBudget) (bool, error) {
	defer rows.Close()
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return false, err
		}
		size := v.Size()
		if b.rows > 0 && b.used+size > b.max {
			return true, nil
		}
		*dst = append(*dst, v)
		b.used += size
		b.rows++
	}
	return false, mapErr(rows.Err())
}

// recordSection is one section of GetSessionRecords: its query (after
// the session id, and the cursor's keys when resuming in it) and how to
// page it into the result.
type recordSection struct {
	id    store.RecordSections
	query string // ends with the WHERE clause; keyCols, then ORDER BY are added
	keys  string // the key columns, as a row value
	order string
	page  func(rows *sql.Rows, out *store.SessionRecords, b *pageBudget) (full bool, last [2]string, err error)
}

// last returns the key of the last row of a section's slice.
func last[T any](xs []T, key func(T) [2]string) [2]string {
	if len(xs) == 0 {
		return [2]string{}
	}
	return key(xs[len(xs)-1])
}

var recordSections = []recordSection{
	{store.RecordCwds, `SELECT agent_id, device_id, cwd, updated_at FROM session_cwds WHERE session_id = ?`,
		`(agent_id, device_id)`, `agent_id, device_id`,
		func(rows *sql.Rows, out *store.SessionRecords, b *pageBudget) (bool, [2]string, error) {
			full, err := pageRows(rows, scanCwd, &out.Cwds, b)
			return full, last(out.Cwds, func(c store.SessionCwd) [2]string { return [2]string{c.AgentID, c.DeviceID} }), err
		}},
	{store.RecordFiles, `SELECT device_id, path, sha256, mtime_ms, touched_at FROM session_files WHERE session_id = ?`,
		`(device_id, path)`, `device_id, path`,
		func(rows *sql.Rows, out *store.SessionRecords, b *pageBudget) (bool, [2]string, error) {
			full, err := pageRows(rows, scanFile, &out.Files, b)
			return full, last(out.Files, func(x store.SessionFile) [2]string { return [2]string{x.DeviceID, x.Path} }), err
		}},
	{store.RecordJobs, sessionJobQuery, `(sj.call_id, '')`, `sj.call_id`,
		func(rows *sql.Rows, out *store.SessionRecords, b *pageBudget) (bool, [2]string, error) {
			full, err := pageRows(rows, scanSessionJob, &out.Jobs, b)
			return full, last(out.Jobs, func(x store.SessionJob) [2]string { return [2]string{x.CallID, ""} }), err
		}},
	{store.RecordBgTasks, `SELECT ` + bgTaskCols + ` FROM session_bgtasks WHERE session_id = ?`,
		`(task_id, '')`, `task_id`,
		func(rows *sql.Rows, out *store.SessionRecords, b *pageBudget) (bool, [2]string, error) {
			full, err := pageRows(rows, scanBgTask, &out.BgTasks, b)
			return full, last(out.BgTasks, func(x store.BgTask) [2]string { return [2]string{x.TaskID, ""} }), err
		}},
	{store.RecordGrants, `SELECT ` + grantCols + ` FROM session_grants WHERE session_id = ? AND expires_at > ? AND consumed_tool_use_id IS NULL`,
		`(grant_id, '')`, `grant_id`,
		func(rows *sql.Rows, out *store.SessionRecords, b *pageBudget) (bool, [2]string, error) {
			full, err := pageRows(rows, scanGrant, &out.Grants, b)
			return full, last(out.Grants, func(x store.Grant) [2]string { return [2]string{x.GrantID, ""} }), err
		}},
}

func (o *ops) RegisterSessionJob(ctx context.Context, j store.SessionJob) (bool, error) {
	if err := checkRecordsPatch(store.RecordsPatch{Jobs: []store.SessionJob{j}}); err != nil {
		return false, err
	}
	registered := false
	err := o.unit(ctx, func(tx *ops) error {
		var device, boot, sid string
		// Only a call of the session's own user registers a job there.
		err := mapErr(tx.q.QueryRow(ctx, `SELECT c.device_id, c.dispatched_boot_id, c.session_id FROM calls c
			JOIN sessions s ON s.session_id = c.session_id AND s.user_id = c.user_id WHERE c.call_id = ?`, j.CallID).Scan(&device, nullStr{&boot}, &sid))
		if isNotFound(err) || (err == nil && boot == "") {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `INSERT INTO session_jobs (session_id, call_id, device_id, boot_id, job_id, tool_use_id,
			description, output_path, created_at) VALUES (?,?,?,?,?,?,?,?,?)
			ON CONFLICT (session_id, call_id) DO UPDATE SET job_id = excluded.job_id, tool_use_id = excluded.tool_use_id,
			description = COALESCE(excluded.description, session_jobs.description),
			output_path = COALESCE(excluded.output_path, session_jobs.output_path)`,
			sid, j.CallID, device, boot, j.JobID, nstr(j.ToolUseID), nstr(j.Description), nstr(j.OutputPath), j.CreatedAt); err != nil {
			return err
		}
		if n, err := tx.count(ctx, `SELECT COUNT(*) FROM session_jobs WHERE session_id = ? AND `+sessionJobDeviceMatchesOwner, sid); err != nil {
			return err
		} else if n > store.MaxSessionJobs {
			return fmt.Errorf("%w: %d session jobs", store.ErrLimit, n)
		}
		registered = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return registered, nil
}

func (o *ops) GetSessionRecords(ctx context.Context, f store.Fence, r store.RecordsRequest) (store.SessionRecords, error) {
	if r.Sections == 0 || r.Sections&^store.RecordAll != 0 {
		return store.SessionRecords{}, invalid("unknown record sections %b", r.Sections)
	}
	if r.MaxBytes < 1 || r.MaxBytes > store.MaxTranscriptPageBytes {
		return store.SessionRecords{}, invalid("records page needs a byte budget of 1..%d", store.MaxTranscriptPageBytes)
	}
	if a := r.After; a != nil {
		if a.Section&r.Sections == 0 || a.Section&(a.Section-1) != 0 {
			return store.SessionRecords{}, invalid("records cursor names section %b, not one of %b", a.Section, r.Sections)
		}
		if len(a.Key1) > store.MaxPathBytes || len(a.Key2) > store.MaxPathBytes {
			return store.SessionRecords{}, invalid("records cursor key too long")
		}
	}
	var out store.SessionRecords
	err := o.fenced(ctx, f, func(tx *ops) error {
		b := &pageBudget{max: r.MaxBytes}
		for _, sec := range recordSections {
			if r.Sections&sec.id == 0 || (r.After != nil && sec.id < r.After.Section) {
				continue
			}
			q, args := sec.query, []any{f.SessionID}
			if sec.id == store.RecordGrants {
				args = append(args, f.Now)
			}
			if r.After != nil && sec.id == r.After.Section {
				q += ` AND ` + sec.keys + ` > (?, ?)`
				args = append(args, r.After.Key1, r.After.Key2)
			}
			rows, err := tx.q.QueryContext(ctx, q+` ORDER BY `+sec.order, args...)
			if err != nil {
				return mapErr(err)
			}
			full, key, err := sec.page(rows, &out, b)
			if err != nil {
				return err
			}
			if full {
				// The section's last row in this page; ("", "") sorts before
				// every key, so a section with none restarts from its start.
				out.Next = &store.RecordsCursor{Section: sec.id, Key1: key[0], Key2: key[1]}
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return store.SessionRecords{}, err
	}
	return out, nil
}

func (o *ops) PurgeSessionJobs(ctx context.Context, before int64, limit int) (int, error) {
	if err := store.CheckLimit(limit); err != nil {
		return 0, err
	}
	if before == 0 {
		return 0, invalid("PurgeSessionJobs needs a time")
	}
	return o.exec(ctx, `DELETE FROM session_jobs WHERE (session_id, call_id) IN (
		SELECT sj.session_id, sj.call_id FROM session_jobs sj
		JOIN sessions s ON s.session_id = sj.session_id
		JOIN jobs j ON j.device_id = sj.device_id AND j.boot_id = sj.boot_id AND j.job_id = sj.job_id AND j.user_id = s.user_id
		WHERE sj.notice = 'persisted' AND sj.notice_at <= ? AND j.state <> 'running'
		ORDER BY sj.notice_at, sj.session_id, sj.call_id LIMIT ?)`, before, limit)
}
