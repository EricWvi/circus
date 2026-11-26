package sqlite

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/EricWvi/circus/internal/store"
)

const approvalCols = `approval_id, session_id, user_id, kind, tool_use_id, agent_id, tool, device_id, match_key, display,
	reason, input_json, state, created_at, expires_at, resolved_at`

func scanApproval(s scanner) (store.Approval, error) {
	var a store.Approval
	var state, kind string
	err := s.Scan(&a.ApprovalID, &a.SessionID, &a.UserID, &kind, nullStr{&a.ToolUseID}, nullStr{&a.AgentID}, &a.Tool,
		nullStr{&a.DeviceID}, nullStr{&a.MatchKey}, nullStr{&a.Display}, nullStr{&a.Reason}, nullStr{&a.InputJSON}, &state,
		&a.CreatedAt, nullInt{&a.ExpiresAt}, nullInt{&a.ResolvedAt})
	a.State, a.Kind = store.ApprovalState(state), store.ApprovalKind(kind)
	return a, mapErr(err)
}

const grantCols = `grant_id, session_id, tool, device_id, match_key, created_at, expires_at`

func scanGrant(s scanner) (store.Grant, error) {
	var g store.Grant
	err := s.Scan(&g.GrantID, &g.SessionID, &g.Key.Tool, &g.Key.DeviceID, &g.Key.MatchKey, &g.CreatedAt, &g.ExpiresAt)
	return g, mapErr(err)
}

func (o *ops) getApproval(ctx context.Context, id string) (store.Approval, error) {
	return scanApproval(o.q.QueryRow(ctx, `SELECT `+approvalCols+` FROM session_approvals WHERE approval_id = ?`, id))
}

// trimResolved keeps the session's newest KeptResolvedApprovals resolved
// approvals.
func (o *ops) trimResolved(ctx context.Context, sessionID string) error {
	_, err := o.exec(ctx, `DELETE FROM session_approvals WHERE approval_id IN (
		SELECT approval_id FROM session_approvals WHERE session_id = ? AND state <> 'pending'
		ORDER BY resolved_at DESC, approval_id DESC LIMIT -1 OFFSET ?)`, sessionID, store.KeptResolvedApprovals)
	return err
}

// endQuestions expires the session's pending questions (at most one): its
// session stopped at at.
func (o *ops) endQuestions(ctx context.Context, sessionID string, at int64) error {
	_, err := o.exec(ctx, `UPDATE session_approvals SET state = 'expired', resolved_at = ?, input_json = NULL
		WHERE session_id = ? AND kind = 'question' AND state = 'pending'`, at, sessionID)
	if err != nil {
		return err
	}
	return o.trimResolved(ctx, sessionID)
}

func checkApproval(a store.Approval) error {
	if err := store.CheckID("approval id", a.ApprovalID); err != nil {
		return err
	}
	for _, c := range []struct{ what, v string }{{"tool use id", a.ToolUseID}, {"agent id", a.AgentID}} {
		if err := store.CheckOptionalID(c.what, c.v); err != nil {
			return err
		}
	}
	for _, c := range []struct{ what, v string }{{"approval display", a.Display}, {"approval reason", a.Reason}} {
		if err := store.CheckText(c.what, c.v, store.MaxDescriptionBytes, true); err != nil {
			return err
		}
	}
	if a.CreatedAt == 0 {
		return invalid("approval needs a creation time")
	}
	switch a.Kind {
	case store.ApprovalTool:
		if err := (store.GrantKey{Tool: a.Tool, DeviceID: a.DeviceID, MatchKey: a.MatchKey}).Check(); err != nil {
			return err
		}
		if a.InputJSON != "" {
			return invalid("a tool approval has no input")
		}
		if a.ExpiresAt <= a.CreatedAt {
			return invalid("a tool approval needs an expiry after its creation")
		}
	case store.ApprovalQuestion:
		if err := store.CheckText("tool", a.Tool, store.MaxToolBytes, false); err != nil {
			return err
		}
		if a.ToolUseID == "" {
			return invalid("a question needs a tool use id")
		}
		if a.DeviceID != "" || a.MatchKey != "" || a.ExpiresAt != 0 {
			return invalid("a question has no device, match key or expiry")
		}
		if a.InputJSON == "" || len(a.InputJSON) > store.MaxQuestionInputBytes {
			return invalid("a question's input must be 1..%d bytes", store.MaxQuestionInputBytes)
		}
	default:
		return invalid("unknown approval kind %q", a.Kind)
	}
	return nil
}

func (o *ops) RequestApproval(ctx context.Context, f store.Fence, a store.Approval) (store.ApprovalRequested, error) {
	if a.Kind == "" {
		a.Kind = store.ApprovalTool
	}
	if err := checkApproval(a); err != nil {
		return store.ApprovalRequested{}, err
	}
	if a.SessionID != "" && a.SessionID != f.SessionID {
		return store.ApprovalRequested{}, invalid("approval names another session than its fence")
	}
	var out store.ApprovalRequested
	err := o.fenced(ctx, f, func(tx *ops) error {
		s, err := tx.getSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		if err := s.State.Refusal(); err != nil {
			return fmt.Errorf("%w: session %q", err, f.SessionID)
		}
		var old store.Approval
		if a.Kind == store.ApprovalQuestion {
			old, err = scanApproval(tx.q.QueryRow(ctx, `SELECT `+approvalCols+` FROM session_approvals
				WHERE session_id = ? AND kind = 'question' AND tool_use_id = ?`, f.SessionID, a.ToolUseID))
		} else {
			// A pending approval of the key that expired (the sweep has not
			// run yet) takes no more requests: it is expired here, as the
			// sweep would, so that the new one is created.
			if _, err := tx.exec(ctx, `UPDATE session_approvals SET state = 'expired', resolved_at = ?1
				WHERE session_id = ?2 AND kind = 'tool' AND tool = ?3 AND device_id = ?4 AND match_key = ?5 AND state = 'pending'
					AND expires_at <= ?1`, f.Now, f.SessionID, a.Tool, a.DeviceID, a.MatchKey); err != nil {
				return err
			}
			old, err = scanApproval(tx.q.QueryRow(ctx, `SELECT `+approvalCols+` FROM session_approvals
				WHERE session_id = ? AND kind = 'tool' AND tool = ? AND device_id = ? AND match_key = ? AND state = 'pending'`,
				f.SessionID, a.Tool, a.DeviceID, a.MatchKey))
		}
		if err == nil {
			out.Approval = old
			return nil
		}
		if !isNotFound(err) {
			return err
		}
		if a.Kind == store.ApprovalQuestion {
			rows, err := tx.q.QueryContext(ctx, `UPDATE session_approvals SET state = 'superseded', resolved_at = ?, input_json = NULL
				WHERE session_id = ? AND kind = 'question' AND state = 'pending' RETURNING `+approvalCols, a.CreatedAt, f.SessionID)
			if err != nil {
				return mapErr(err)
			}
			if out.Superseded, err = collect(rows, scanApproval); err != nil {
				return err
			}
		}
		n, err := tx.count(ctx, `SELECT COUNT(*) FROM session_approvals WHERE session_id = ? AND state = 'pending'`, f.SessionID)
		if err != nil {
			return err
		}
		if n >= store.MaxPendingApprovals {
			// At most one pending approval is a question, so the cap
			// always finds a tool approval to expire.
			oldest, err := scanApproval(tx.q.QueryRow(ctx, `UPDATE session_approvals SET state = 'expired', resolved_at = ?
				WHERE approval_id = (SELECT approval_id FROM session_approvals WHERE session_id = ? AND state = 'pending'
					AND kind = 'tool' ORDER BY created_at, approval_id LIMIT 1) RETURNING `+approvalCols, a.CreatedAt, f.SessionID))
			if err != nil {
				return err
			}
			out.Expired = &oldest
		}
		if _, err := tx.exec(ctx, `INSERT INTO session_approvals (approval_id, session_id, user_id, kind, tool_use_id, agent_id,
			tool, device_id, match_key, display, reason, input_json, state, created_at, expires_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,'pending',?,?)`, a.ApprovalID, f.SessionID, s.UserID, string(a.Kind), nstr(a.ToolUseID),
			nstr(a.AgentID), a.Tool, nstr(a.DeviceID), nstr(a.MatchKey), nstr(a.Display), nstr(a.Reason), nstr(a.InputJSON),
			a.CreatedAt, nint(a.ExpiresAt)); err != nil {
			return err
		}
		if err := tx.trimResolved(ctx, f.SessionID); err != nil {
			return err
		}
		out.Created = true
		out.Approval, err = tx.getApproval(ctx, a.ApprovalID)
		return err
	})
	if err != nil {
		return store.ApprovalRequested{}, err
	}
	return out, nil
}

func (o *ops) ResolveApproval(ctx context.Context, r store.ApprovalResolution) (store.ApprovalResolved, error) {
	if err := store.CheckID("approval id", r.ApprovalID); err != nil {
		return store.ApprovalResolved{}, err
	}
	if r.At == 0 {
		return store.ApprovalResolved{}, invalid("ResolveApproval needs a time")
	}
	var out store.ApprovalResolved
	err := o.unit(ctx, func(tx *ops) error {
		a, err := tx.getApproval(ctx, r.ApprovalID)
		if err != nil {
			return err
		}
		question := a.Kind == store.ApprovalQuestion
		if a.State == store.ApprovalPending {
			var in store.Input
			if r.Approve {
				if !question && r.GrantExpiresAt <= r.At {
					return invalid("approving needs a grant expiry after the resolution")
				}
				in = r.Input
				in.SessionID, in.ClientMsgID, in.Kind, in.Priority = a.SessionID, approvalMsgPrefix+a.ApprovalID, store.InputApproval, store.InputNormal
				if question {
					in.Kind = store.InputAnswer
				}
				if in.CreatedAt == 0 {
					in.CreatedAt = r.At
				}
				if err := checkInput(in); err != nil {
					return err
				}
			}
			s, err := tx.getSession(ctx, a.SessionID, r.At)
			if err != nil {
				return err
			}
			if s.State == store.SessionStopped {
				return fmt.Errorf("%w: session %q", store.ErrStopped, a.SessionID)
			}
			var state store.ApprovalState
			switch {
			case a.Expired(r.At):
				// Past its expiry, the requester has given up: the sweep
				// had not run yet.
				state = store.ApprovalExpired
			case question && r.Approve:
				state = store.ApprovalAnswered
			case question:
				state = store.ApprovalDismissed
			case r.Approve:
				state = store.ApprovalApproved
			default:
				state = store.ApprovalDenied
			}
			if _, err := tx.exec(ctx, `UPDATE session_approvals SET state = ?, resolved_at = ?, input_json = NULL WHERE approval_id = ?`,
				string(state), r.At, r.ApprovalID); err != nil {
				return err
			}
			out.Changed = state != store.ApprovalExpired
			if state == store.ApprovalApproved {
				if _, err := tx.exec(ctx, `INSERT INTO session_grants (grant_id, session_id, tool, device_id, match_key,
					created_at, expires_at) VALUES (?,?,?,?,?,?,?)`, a.ApprovalID, a.SessionID, a.Tool, a.DeviceID,
					a.MatchKey, r.At, r.GrantExpiresAt); err != nil {
					return err
				}
				if _, err := tx.exec(ctx, `DELETE FROM session_grants WHERE grant_id IN (
					SELECT grant_id FROM session_grants WHERE session_id = ?1 AND consumed_tool_use_id IS NULL
					ORDER BY expires_at, grant_id
					LIMIT max(0, (SELECT COUNT(*) FROM session_grants WHERE session_id = ?1 AND consumed_tool_use_id IS NULL) - ?2))`,
					a.SessionID, store.MaxSessionGrants); err != nil {
					return err
				}
			}
			if state == store.ApprovalApproved || state == store.ApprovalAnswered {
				sub, err := tx.submitInput(ctx, in)
				if err != nil {
					return err
				}
				out.Input = &sub.Input
			}
			if err := tx.trimResolved(ctx, a.SessionID); err != nil {
				return err
			}
			if a, err = tx.getApproval(ctx, r.ApprovalID); err != nil {
				return err
			}
		}
		out.Approval = a
		if out.Session, err = tx.getSession(ctx, a.SessionID, r.At); err != nil {
			return err
		}
		out.Lease, err = tx.getLease(ctx, a.SessionID)
		return err
	})
	if err != nil {
		return store.ApprovalResolved{}, err
	}
	return out, nil
}

func (o *ops) GetApproval(ctx context.Context, approvalID string) (store.Approval, error) {
	return o.getApproval(ctx, approvalID)
}

func (o *ops) ListApprovals(ctx context.Context, f store.ApprovalFilter) ([]store.Approval, error) {
	if err := store.CheckLimit(f.Limit); err != nil {
		return nil, err
	}
	if f.MaxBytes < 1 || f.MaxBytes > store.MaxTranscriptPageBytes {
		return nil, invalid("approvals page needs a byte budget of 1..%d", store.MaxTranscriptPageBytes)
	}
	for _, c := range []struct{ what, v string }{{"user id", f.UserID}, {"session id", f.SessionID}} {
		if err := store.CheckOptionalID(c.what, c.v); err != nil {
			return nil, err
		}
	}
	var where []string
	var args []any
	switch {
	case f.SessionID != "":
		where, args = append(where, "session_id = ?"), append(args, f.SessionID)
		if f.UserID != "" {
			where, args = append(where, "user_id = ?"), append(args, f.UserID)
		}
	case f.UserID != "":
		where, args = append(where, "user_id = ?"), append(args, f.UserID)
	default:
		return nil, invalid("ListApprovals needs a user id or a session id")
	}
	if f.After != nil {
		where, args = append(where, "(created_at, approval_id) > (?, ?)"), append(args, f.After.CreatedAt, f.After.ApprovalID)
	}
	if len(f.States) > 4 {
		return nil, invalid("too many approval states")
	}
	if len(f.States) > 0 {
		for _, st := range f.States {
			if !st.Valid() {
				return nil, invalid("unknown approval state %q", st)
			}
		}
		where, args = append(where, "state IN ("+placeholders(len(f.States))+")"), append(args, anys(f.States)...)
	}
	rows, err := o.q.QueryContext(ctx, `SELECT `+approvalCols+` FROM session_approvals WHERE `+
		strings.Join(where, " AND ")+` ORDER BY created_at, approval_id LIMIT ?`, append(args, f.Limit)...)
	if err != nil {
		return nil, mapErr(err)
	}
	var out []approvalRow
	if _, err := pageRows(rows, func(s scanner) (approvalRow, error) {
		a, err := scanApproval(s)
		return approvalRow{a}, err
	}, &out, &pageBudget{max: f.MaxBytes}); err != nil {
		return nil, err
	}
	list := make([]store.Approval, len(out))
	for i, r := range out {
		list[i] = r.Approval
	}
	return list, nil
}

// approvalRow gives an approval the page size of ListApprovals.
type approvalRow struct{ store.Approval }

func (r approvalRow) Size() int64 { return store.ApprovalSize(r.Approval) }

func (o *ops) ConsumeGrant(ctx context.Context, f store.Fence, k store.GrantKey, toolUseID string) (bool, error) {
	if err := k.Check(); err != nil {
		return false, err
	}
	if err := store.CheckID("tool use id", toolUseID); err != nil {
		return false, err
	}
	var ok bool
	err := o.fenced(ctx, f, func(tx *ops) error {
		s, err := tx.getSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		if err := s.State.Refusal(); err != nil {
			return fmt.Errorf("%w: session %q", err, f.SessionID)
		}
		// A resent request of a call that already consumed a grant gets the
		// same answer while that grant would still be valid.
		var g store.Grant
		g, err = scanGrant(tx.q.QueryRow(ctx, `SELECT `+grantCols+` FROM session_grants
			WHERE session_id = ? AND consumed_tool_use_id = ?`, f.SessionID, toolUseID))
		if err == nil {
			ok = g.Key == k && g.ExpiresAt > f.Now
			return nil
		}
		if !isNotFound(err) {
			return err
		}
		n, err := tx.exec(ctx, `UPDATE session_grants SET consumed_tool_use_id = ?, consumed_at = ? WHERE grant_id = (
			SELECT grant_id FROM session_grants WHERE session_id = ? AND tool = ? AND device_id = ? AND match_key = ?
				AND consumed_tool_use_id IS NULL AND expires_at > ? ORDER BY expires_at, grant_id LIMIT 1)`,
			toolUseID, f.Now, f.SessionID, k.Tool, k.DeviceID, k.MatchKey, f.Now)
		if err != nil {
			return err
		}
		ok = n > 0
		if !ok {
			return nil
		}
		// Consumed grants are kept for replay only: the oldest consumption
		// goes past the cap, expired ones (which can no longer replay true)
		// before live ones.
		_, err = tx.exec(ctx, `DELETE FROM session_grants WHERE grant_id IN (
			SELECT grant_id FROM session_grants WHERE session_id = ?1 AND consumed_tool_use_id IS NOT NULL
			ORDER BY expires_at > ?3, consumed_at, grant_id
			LIMIT max(0, (SELECT COUNT(*) FROM session_grants WHERE session_id = ?1 AND consumed_tool_use_id IS NOT NULL) - ?2))`,
			f.SessionID, store.MaxSessionGrants, f.Now)
		return err
	})
	return ok, err
}

func (o *ops) ExpireApprovals(ctx context.Context, now int64, limit int) ([]store.Approval, error) {
	if err := store.CheckLimit(limit); err != nil {
		return nil, err
	}
	if now == 0 {
		return nil, invalid("ExpireApprovals needs a time")
	}
	var out []store.Approval
	err := o.unit(ctx, func(tx *ops) error {
		rows, err := tx.q.QueryContext(ctx, `UPDATE session_approvals SET state = 'expired', resolved_at = ?1
			WHERE approval_id IN (SELECT approval_id FROM session_approvals WHERE state = 'pending' AND expires_at <= ?1
				ORDER BY expires_at, approval_id LIMIT ?2) RETURNING `+approvalCols, now, limit)
		if err != nil {
			return mapErr(err)
		}
		if out, err = collect(rows, scanApproval); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, a := range out {
			if !seen[a.SessionID] {
				seen[a.SessionID] = true
				if err := tx.trimResolved(ctx, a.SessionID); err != nil {
					return err
				}
			}
		}
		_, err = tx.exec(ctx, `DELETE FROM session_grants WHERE expires_at <= ?`, now)
		return err
	})
	// RETURNING order is unspecified.
	slices.SortFunc(out, func(a, b store.Approval) int {
		if a.ExpiresAt != b.ExpiresAt {
			if a.ExpiresAt < b.ExpiresAt {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ApprovalID, b.ApprovalID)
	})
	return out, err
}
