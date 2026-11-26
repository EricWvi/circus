package sqlite

import (
	"context"

	"github.com/EricWvi/circus/internal/store"
)

// Worker agents.

const workerAgentCols = `worker_id, agent, state, installed_version, target_version, error, downloaded_bytes, total_bytes,
	reported_at, wanted, wanted_version, wanted_attempt`

func scanWorkerAgent(s scanner) (store.WorkerAgent, error) {
	var a store.WorkerAgent
	var agent, state string
	var wanted *int64
	err := s.Scan(&a.WorkerID, &agent, nullStr{&state}, &a.Status.InstalledVersion, &a.Status.TargetVersion, &a.Status.Error,
		&a.Status.DownloadedBytes, &a.Status.TotalBytes, nullInt{&a.ReportedAt}, &wanted, nullStr{&a.WantedVersion}, &a.WantedAttempt)
	a.Agent, a.Status.Agent, a.Status.State = store.AgentKind(agent), store.AgentKind(agent), store.AgentState(state)
	a.Wished = wanted != nil
	a.Wanted = wanted != nil && *wanted == 1
	return a, mapErr(err)
}

// workerAgents reads the worker's agents (ErrNotFound for an unknown
// worker).
func (o *ops) workerAgents(ctx context.Context, workerID string) (store.WorkerAgents, error) {
	out := store.WorkerAgents{WorkerID: workerID}
	if err := o.q.QueryRow(ctx, `SELECT agents_reported_at, agents_rev FROM workers WHERE id = ?`, workerID).Scan(nullInt{&out.ReportedAt}, &out.Rev); err != nil {
		return out, mapErr(err)
	}
	rows, err := o.q.QueryContext(ctx, `SELECT `+workerAgentCols+` FROM worker_agents WHERE worker_id = ?`, workerID)
	if err != nil {
		return out, mapErr(err)
	}
	if out.Agents, err = collect(rows, scanWorkerAgent); err != nil {
		return out, err
	}
	store.SortWorkerAgents(out.Agents)
	return out, nil
}

func (o *ops) ListWorkerAgents(ctx context.Context, workerID string) (store.WorkerAgents, error) {
	if err := store.CheckID("worker id", workerID); err != nil {
		return store.WorkerAgents{}, err
	}
	var out store.WorkerAgents
	err := o.atomic(ctx, func(tx *ops) error {
		var err error
		out, err = tx.workerAgents(ctx, workerID)
		return err
	})
	return out, err
}

func (o *ops) ListUserWorkerAgents(ctx context.Context, userID string) ([]store.WorkerAgent, error) {
	if err := store.CheckID("user id", userID); err != nil {
		return nil, err
	}
	rows, err := o.q.QueryContext(ctx, `SELECT `+workerAgentCols+` FROM worker_agents
		WHERE worker_id IN (SELECT id FROM workers WHERE user_id = ? AND deleted_at IS NULL)`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	out, err := collect(rows, scanWorkerAgent)
	if err != nil {
		return nil, err
	}
	store.SortWorkerAgents(out)
	return out, nil
}

func (o *ops) ReportWorkerAgents(ctx context.Context, workerID string, from store.AgentReporter, report []store.AgentStatus, at int64) (store.WorkerAgents, error) {
	if err := store.CheckID("worker id", workerID); err != nil {
		return store.WorkerAgents{}, err
	}
	if err := store.CheckID("boot id", from.BootID); err != nil {
		return store.WorkerAgents{}, err
	}
	if err := store.CheckID("connection id", from.ConnID); err != nil {
		return store.WorkerAgents{}, err
	}
	if err := store.CheckAgentReport(report); err != nil {
		return store.WorkerAgents{}, err
	}
	if at == 0 {
		return store.WorkerAgents{}, invalid("ReportWorkerAgents needs a time")
	}
	var out store.WorkerAgents
	err := o.atomic(ctx, func(tx *ops) error {
		var boot, conn, start string
		var seq int64
		if err := tx.q.QueryRow(ctx, `SELECT COALESCE(current_boot_id, ''), COALESCE(agents_conn_id, ''), COALESCE(agents_conn_start, ''),
			COALESCE(agents_conn_seq, 0) FROM workers WHERE id = ?`, workerID).Scan(&boot, &conn, &start, &seq); err != nil {
			return mapErr(err)
		}
		switch {
		case boot != from.BootID:
			return store.ErrStaleReport
		case !from.Attach && conn != from.ConnID:
			return store.ErrStaleReport
		case from.Attach && conn != "" && start == from.ConnStart && seq > from.ConnSeq:
			// A later connection of this start claimed the reports first.
			return store.ErrStaleReport
		}
		if _, err := tx.exec(ctx, `UPDATE workers SET agents_reported_at = ?, agents_rev = agents_rev + 1, agents_conn_id = ?,
			agents_conn_start = ?, agents_conn_seq = ? WHERE id = ?`, at, from.ConnID, from.ConnStart, from.ConnSeq, workerID); err != nil {
			return err
		}
		reported := make([]any, 0, len(report))
		for _, s := range report {
			reported = append(reported, string(s.Agent))
		}
		// Agents the report leaves out: their state goes, and so do
		// entries without a wish to keep.
		notIn := ``
		if len(reported) > 0 {
			notIn = ` AND agent NOT IN (` + placeholders(len(reported)) + `)`
		}
		args := append([]any{workerID}, reported...)
		if _, err := tx.exec(ctx, `DELETE FROM worker_agents WHERE worker_id = ? AND wanted IS NOT 1`+notIn, args...); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `UPDATE worker_agents SET state = NULL, installed_version = '', target_version = '', error = '',
			downloaded_bytes = 0, total_bytes = 0, reported_at = NULL WHERE worker_id = ?`+notIn, args...); err != nil {
			return err
		}
		for _, s := range report {
			// An agent on disk without a recorded wish is adopted: wanted
			// at the version it has.
			adopt := (s.State == store.AgentInstalled || s.State == store.AgentOutdated) && s.InstalledVersion != ""
			if _, err := tx.exec(ctx, `INSERT INTO worker_agents (worker_id, agent, state, installed_version, target_version, error,
				downloaded_bytes, total_bytes, reported_at, wanted, wanted_version) VALUES (?1,?2,?3,?4,?5,?6,?7,?8,?9,
				CASE WHEN ?10 THEN 1 END, CASE WHEN ?10 THEN ?4 END)
				ON CONFLICT (worker_id, agent) DO UPDATE SET state = excluded.state, installed_version = excluded.installed_version,
				target_version = excluded.target_version, error = excluded.error, downloaded_bytes = excluded.downloaded_bytes,
				total_bytes = excluded.total_bytes, reported_at = excluded.reported_at,
				wanted = CASE WHEN worker_agents.wanted IS NULL THEN excluded.wanted ELSE worker_agents.wanted END,
				wanted_version = CASE WHEN worker_agents.wanted IS NULL THEN excluded.wanted_version ELSE worker_agents.wanted_version END`,
				workerID, string(s.Agent), string(s.State), s.InstalledVersion, s.TargetVersion, s.Error,
				s.DownloadedBytes, s.TotalBytes, at, adopt); err != nil {
				return err
			}
		}
		var err error
		out, err = tx.workerAgents(ctx, workerID)
		return err
	})
	return out, err
}

func (o *ops) SetWorkerAgentWish(ctx context.Context, workerID string, agent store.AgentKind, wanted bool, version string) (bool, error) {
	if err := store.CheckID("worker id", workerID); err != nil {
		return false, err
	}
	switch {
	case !agent.Valid():
		return false, invalid("unknown agent %q", agent)
	case wanted && version == "":
		return false, invalid("a wanted agent needs a version")
	case !wanted && version != "":
		return false, invalid("an unwanted agent has no version")
	case len(version) > store.MaxAgentVersionBytes:
		return false, invalid("agent version is longer than %d bytes", store.MaxAgentVersionBytes)
	}
	changed := false
	err := o.atomic(ctx, func(tx *ops) error {
		var exists int
		if err := tx.q.QueryRow(ctx, `SELECT 1 FROM workers WHERE id = ?`, workerID).Scan(&exists); err != nil {
			return mapErr(err)
		}
		// Wanting it is an install request: the attempt grows every time.
		n, err := tx.exec(ctx, `INSERT INTO worker_agents (worker_id, agent, wanted, wanted_version, wanted_attempt)
			VALUES (?1, ?2, ?3, ?4, ?3)
			ON CONFLICT (worker_id, agent) DO UPDATE SET wanted = excluded.wanted, wanted_version = excluded.wanted_version,
			wanted_attempt = worker_agents.wanted_attempt + excluded.wanted
			WHERE excluded.wanted = 1 OR worker_agents.wanted IS NOT excluded.wanted OR worker_agents.wanted_version IS NOT excluded.wanted_version`,
			workerID, string(agent), b2i(wanted), nstr(version))
		if err != nil || n == 0 {
			return err
		}
		changed = true
		_, err = tx.exec(ctx, `UPDATE workers SET agents_rev = agents_rev + 1 WHERE id = ?`, workerID)
		return err
	})
	return changed, err
}

func (o *ops) HasAgentLease(ctx context.Context, workerID string, agent store.AgentKind, now int64) (bool, error) {
	if err := store.CheckID("worker id", workerID); err != nil {
		return false, err
	}
	if !agent.Valid() {
		return false, invalid("unknown agent %q", agent)
	}
	if now == 0 {
		return false, invalid("HasAgentLease needs a time")
	}
	var held bool
	err := o.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM session_leases l JOIN sessions s ON s.session_id = l.session_id
		WHERE l.holder_worker_id = ? AND l.expires_at > ? AND s.agent = ?)`, workerID, now, string(agent)).Scan(&held)
	return held, mapErr(err)
}
