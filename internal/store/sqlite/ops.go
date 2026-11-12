package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/EricWvi/circus/internal/store"
)

// querier runs statements on the pool or in a transaction.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRow(ctx context.Context, query string, args ...any) scanner
}

// txQuerier runs statements in an open transaction.
type txQuerier struct{ *sql.Tx }

func (q txQuerier) QueryRow(ctx context.Context, query string, args ...any) scanner {
	return q.QueryRowContext(ctx, query, args...)
}

// poolQuerier runs statements on the pool, each in its own implicit
// transaction. Every statement first checks that ctx is not inside an open
// Tx of the same DB (store.CheckOutsideTx): the pool's single connection is
// held by that Tx, so the statement would wait for it until ctx ends.
type poolQuerier struct{ d *DB }

func (q poolQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := store.CheckOutsideTx(ctx, q.d); err != nil {
		return nil, err
	}
	return q.d.sqldb.ExecContext(ctx, query, args...)
}

func (q poolQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if err := store.CheckOutsideTx(ctx, q.d); err != nil {
		return nil, err
	}
	return q.d.sqldb.QueryContext(ctx, query, args...)
}

func (q poolQuerier) QueryRow(ctx context.Context, query string, args ...any) scanner {
	if err := store.CheckOutsideTx(ctx, q.d); err != nil {
		return errScanner{err}
	}
	return q.d.sqldb.QueryRowContext(ctx, query, args...)
}

type errScanner struct{ err error }

func (e errScanner) Scan(...any) error { return e.err }

// ops implements store.Ops on a querier: the pool (db != nil) or a
// transaction (db == nil).
type ops struct {
	q  querier
	db *DB
}

var _ store.Ops = (*ops)(nil)

// atomic runs fn in a transaction: o's own when o is a tx, else a new one.
func (o *ops) atomic(ctx context.Context, fn func(*ops) error) error {
	if o.db == nil {
		return fn(o)
	}
	return o.db.Tx(ctx, func(_ context.Context, tx store.Ops) error { return fn(tx.(*ops)) })
}

func (o *ops) exec(ctx context.Context, query string, args ...any) (int, error) {
	res, err := o.q.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, mapErr(err)
	}
	n, err := res.RowsAffected()
	return int(n), mapErr(err)
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", store.ErrInvalid, fmt.Sprintf(format, args...))
}

// Optional values: the Go zero value is stored as NULL.
func nstr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nint(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func nexit(v *int32) any {
	if v == nil {
		return nil
	}
	return int64(*v)
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// nullable scan targets.
type nullStr struct{ p *string }

func (n nullStr) Scan(v any) error {
	var s sql.NullString
	if err := s.Scan(v); err != nil {
		return err
	}
	*n.p = s.String
	return nil
}

type nullInt struct{ p *int64 }

func (n nullInt) Scan(v any) error {
	var s sql.NullInt64
	if err := s.Scan(v); err != nil {
		return err
	}
	*n.p = s.Int64
	return nil
}

type nullExit struct{ p **int32 }

func (n nullExit) Scan(v any) error {
	var s sql.NullInt64
	if err := s.Scan(v); err != nil {
		return err
	}
	if s.Valid {
		c := int32(s.Int64)
		*n.p = &c
	} else {
		*n.p = nil
	}
	return nil
}

type scanner interface{ Scan(dest ...any) error }

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func anys[T ~string](vs []T) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return out
}

// Users.

func (o *ops) CreateUser(ctx context.Context, u store.User) error {
	if u.ID == "" || u.Name == "" {
		return invalid("user needs id and name")
	}
	_, err := o.exec(ctx, `INSERT INTO users (id, name, created_at) VALUES (?, ?, ?)`, u.ID, u.Name, u.CreatedAt)
	return err
}

func (o *ops) getUser(ctx context.Context, where string, arg string) (store.User, error) {
	var u store.User
	err := o.q.QueryRow(ctx, `SELECT id, name, created_at FROM users WHERE `+where+` = ?`, arg).
		Scan(&u.ID, &u.Name, &u.CreatedAt)
	return u, mapErr(err)
}

func (o *ops) GetUser(ctx context.Context, id string) (store.User, error) {
	return o.getUser(ctx, "id", id)
}

func (o *ops) GetUserByName(ctx context.Context, name string) (store.User, error) {
	return o.getUser(ctx, "name", name)
}

// Identities.

func identityTable(kind store.PrincipalKind) (string, error) {
	switch kind {
	case store.KindDevice:
		return "devices", nil
	case store.KindWorker:
		return "workers", nil
	}
	return "", invalid("unknown principal kind %q", kind)
}

const identityCols = `id, user_id, name, machine_id, os, arch, hostname, client_version,
	current_boot_id, last_connected_at, last_disconnected_at, created_at, deleted_at`

func scanIdentity(s scanner, kind store.PrincipalKind) (store.Identity, error) {
	id := store.Identity{Kind: kind}
	err := s.Scan(&id.ID, &id.UserID, &id.Name, &id.MachineID,
		nullStr{&id.OS}, nullStr{&id.Arch}, nullStr{&id.Hostname}, nullStr{&id.ClientVersion},
		nullStr{&id.CurrentBootID}, nullInt{&id.LastConnectedAt}, nullInt{&id.LastDisconnectedAt},
		&id.CreatedAt, nullInt{&id.DeletedAt})
	return id, mapErr(err)
}

func (o *ops) CreateIdentity(ctx context.Context, id store.Identity) error {
	table, err := identityTable(id.Kind)
	if err != nil {
		return err
	}
	if id.ID == "" || id.UserID == "" || id.Name == "" || id.MachineID == "" {
		return invalid("identity needs id, user id, name and machine id")
	}
	_, err = o.exec(ctx, `INSERT INTO `+table+` (`+identityCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id.ID, id.UserID, id.Name, id.MachineID,
		nstr(id.OS), nstr(id.Arch), nstr(id.Hostname), nstr(id.ClientVersion),
		nstr(id.CurrentBootID), nint(id.LastConnectedAt), nint(id.LastDisconnectedAt),
		id.CreatedAt, nint(id.DeletedAt))
	return err
}

func (o *ops) GetIdentity(ctx context.Context, kind store.PrincipalKind, id string) (store.Identity, error) {
	table, err := identityTable(kind)
	if err != nil {
		return store.Identity{}, err
	}
	return scanIdentity(o.q.QueryRow(ctx, `SELECT `+identityCols+` FROM `+table+` WHERE id = ?`, id), kind)
}

func (o *ops) GetIdentityByName(ctx context.Context, kind store.PrincipalKind, userID, name string) (store.Identity, error) {
	table, err := identityTable(kind)
	if err != nil {
		return store.Identity{}, err
	}
	return scanIdentity(o.q.QueryRow(ctx, `SELECT `+identityCols+` FROM `+table+`
		WHERE user_id = ? AND name = ? AND deleted_at IS NULL`, userID, name), kind)
}

func (o *ops) ListIdentities(ctx context.Context, kind store.PrincipalKind, userID string) ([]store.Identity, error) {
	table, err := identityTable(kind)
	if err != nil {
		return nil, err
	}
	rows, err := o.q.QueryContext(ctx, `SELECT `+identityCols+` FROM `+table+`
		WHERE user_id = ? AND deleted_at IS NULL ORDER BY name`, userID)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, func(s scanner) (store.Identity, error) { return scanIdentity(s, kind) })
}

func (o *ops) SetConnected(ctx context.Context, kind store.PrincipalKind, id, bootID string, at int64) (string, error) {
	table, err := identityTable(kind)
	if err != nil {
		return "", err
	}
	if id == "" || bootID == "" || at == 0 {
		return "", invalid("SetConnected needs id, boot id and time")
	}
	var prev string
	err = o.atomic(ctx, func(tx *ops) error {
		if err := tx.q.QueryRow(ctx, `SELECT current_boot_id FROM `+table+` WHERE id = ?`, id).
			Scan(nullStr{&prev}); err != nil {
			return mapErr(err)
		}
		_, err := tx.exec(ctx, `UPDATE `+table+` SET current_boot_id = ?, last_connected_at = ? WHERE id = ?`, bootID, at, id)
		return err
	})
	return prev, err
}

func (o *ops) SetDisconnected(ctx context.Context, kind store.PrincipalKind, id string, at int64) error {
	table, err := identityTable(kind)
	if err != nil {
		return err
	}
	if at == 0 {
		return invalid("SetDisconnected needs a time")
	}
	n, err := o.exec(ctx, `UPDATE `+table+` SET last_disconnected_at = ? WHERE id = ?`, at, id)
	if err == nil && n == 0 {
		err = store.ErrNotFound
	}
	return err
}

func (o *ops) DeleteIdentity(ctx context.Context, kind store.PrincipalKind, id string, at int64) (bool, error) {
	table, err := identityTable(kind)
	if err != nil {
		return false, err
	}
	if id == "" || at == 0 {
		return false, invalid("DeleteIdentity needs id and time")
	}
	var deleted bool
	err = o.atomic(ctx, func(tx *ops) error {
		var was int64
		if err := tx.q.QueryRow(ctx, `SELECT deleted_at FROM `+table+` WHERE id = ?`, id).Scan(nullInt{&was}); err != nil {
			return mapErr(err)
		}
		if was != 0 {
			return nil
		}
		_, err := tx.exec(ctx, `UPDATE `+table+` SET deleted_at = ? WHERE id = ? AND deleted_at IS NULL`, at, id)
		deleted = err == nil
		return err
	})
	return deleted, err
}

// Credentials.

const credentialCols = `id, principal_kind, principal_id, token_hash, created_at, last_used_at, revoked_at`

func scanCredential(s scanner) (store.Credential, error) {
	var c store.Credential
	var kind string
	err := s.Scan(&c.ID, &kind, &c.PrincipalID, &c.TokenHash, &c.CreatedAt, nullInt{&c.LastUsedAt}, nullInt{&c.RevokedAt})
	c.Kind = store.PrincipalKind(kind)
	return c, mapErr(err)
}

func (o *ops) CreateCredential(ctx context.Context, c store.Credential) error {
	table, err := identityTable(c.Kind)
	if err != nil {
		return err
	}
	if c.ID == "" || c.PrincipalID == "" || len(c.TokenHash) == 0 {
		return invalid("credential needs id, principal id and token hash")
	}
	return o.atomic(ctx, func(tx *ops) error {
		var one int
		if err := tx.q.QueryRow(ctx, `SELECT 1 FROM `+table+` WHERE id = ?`, c.PrincipalID).Scan(&one); err != nil {
			if err = mapErr(err); errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("%w: %s %q", store.ErrNotFound, c.Kind, c.PrincipalID)
			}
			return err
		}
		_, err := tx.exec(ctx, `INSERT INTO credentials (`+credentialCols+`) VALUES (?,?,?,?,?,?,?)`,
			c.ID, string(c.Kind), c.PrincipalID, c.TokenHash, c.CreatedAt, nint(c.LastUsedAt), nint(c.RevokedAt))
		return err
	})
}

func (o *ops) LookupCredential(ctx context.Context, tokenHash []byte) (store.Credential, error) {
	if len(tokenHash) == 0 {
		return store.Credential{}, invalid("empty token hash")
	}
	return scanCredential(o.q.QueryRow(ctx, `SELECT `+credentialCols+` FROM credentials WHERE token_hash = ?`, tokenHash))
}

func (o *ops) TouchCredential(ctx context.Context, id string, at int64) error {
	if at == 0 {
		return invalid("TouchCredential needs a time")
	}
	n, err := o.exec(ctx, `UPDATE credentials SET last_used_at = ? WHERE id = ?`, at, id)
	if err == nil && n == 0 {
		err = store.ErrNotFound
	}
	return err
}

func (o *ops) RevokeCredentials(ctx context.Context, kind store.PrincipalKind, principalID string, at int64) (int, error) {
	if !kind.Valid() || principalID == "" || at == 0 {
		return 0, invalid("RevokeCredentials needs a kind, principal id and time")
	}
	return o.exec(ctx, `UPDATE credentials SET revoked_at = ?
		WHERE principal_kind = ? AND principal_id = ? AND revoked_at IS NULL`, at, string(kind), principalID)
}

// Enroll tokens.

const enrollCols = `id, user_id, token_hash, kinds, created_at, expires_at, used_at, used_by`

func scanEnrollToken(s scanner) (store.EnrollToken, error) {
	var t store.EnrollToken
	var kinds string
	if err := s.Scan(&t.ID, &t.UserID, &t.TokenHash, &kinds, &t.CreatedAt, &t.ExpiresAt,
		nullInt{&t.UsedAt}, nullStr{&t.UsedBy}); err != nil {
		return t, mapErr(err)
	}
	var err error
	t.Kinds, err = store.DecodeKinds(kinds)
	return t, err
}

func (o *ops) CreateEnrollToken(ctx context.Context, t store.EnrollToken) error {
	kinds, err := store.EncodeKinds(t.Kinds)
	if err != nil {
		return err
	}
	if t.ID == "" || t.UserID == "" || len(t.TokenHash) == 0 || t.ExpiresAt == 0 {
		return invalid("enroll token needs id, user id, token hash and expiry")
	}
	_, err = o.exec(ctx, `INSERT INTO enroll_tokens (`+enrollCols+`) VALUES (?,?,?,?,?,?,?,?)`,
		t.ID, t.UserID, t.TokenHash, kinds, t.CreatedAt, t.ExpiresAt, nint(t.UsedAt), nstr(t.UsedBy))
	return err
}

func (o *ops) UseEnrollToken(ctx context.Context, tokenHash []byte, now int64, usedBy string) (store.EnrollToken, error) {
	if len(tokenHash) == 0 || now == 0 {
		return store.EnrollToken{}, invalid("UseEnrollToken needs a token hash and time")
	}
	// One conditional UPDATE: the write lock serializes concurrent uses and
	// only the first still sees used_at IS NULL.
	t, err := scanEnrollToken(o.q.QueryRow(ctx, `UPDATE enroll_tokens SET used_at = ?, used_by = ?
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING `+enrollCols, now, nstr(usedBy), tokenHash, now))
	if errors.Is(err, store.ErrNotFound) {
		return store.EnrollToken{}, store.ErrTokenUnusable
	}
	return t, err
}

func (o *ops) PurgeEnrollTokens(ctx context.Context, now int64) (int, error) {
	return o.exec(ctx, `DELETE FROM enroll_tokens WHERE used_at IS NULL AND expires_at <= ?`, now)
}

// Calls.

const callCols = `call_id, user_id, device_id, session_id, agent_id, tool, request_hash, request_bytes,
	state, dispatched_boot_id, submitted_at, dispatched_at, acked_at, finished_at, cancel_requested_at,
	exit_code, is_error, fault_code, job_id, outcome_reason`

func scanCall(s scanner) (store.Call, error) {
	var c store.Call
	var state string
	var isError int64
	err := s.Scan(&c.CallID, &c.UserID, &c.DeviceID, &c.SessionID, nullStr{&c.AgentID}, &c.Tool,
		&c.RequestHash, &c.RequestBytes, &state, nullStr{&c.DispatchedBootID}, &c.SubmittedAt,
		nullInt{&c.DispatchedAt}, nullInt{&c.AckedAt}, nullInt{&c.FinishedAt}, nullInt{&c.CancelRequestedAt},
		nullExit{&c.ExitCode}, &isError, nullStr{&c.FaultCode}, nullStr{&c.JobID}, nullStr{&c.OutcomeReason})
	c.State = store.CallState(state)
	c.IsError = isError != 0
	return c, mapErr(err)
}

func (o *ops) CreateCall(ctx context.Context, c store.Call) error {
	if err := store.CheckNewCall(c); err != nil {
		return err
	}
	_, err := o.exec(ctx, `INSERT INTO calls (`+callCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.CallID, c.UserID, c.DeviceID, c.SessionID, nstr(c.AgentID), c.Tool, c.RequestHash, c.RequestBytes,
		string(c.State), nil, c.SubmittedAt, nil, nint(c.AckedAt), nint(c.FinishedAt), nint(c.CancelRequestedAt),
		nexit(c.ExitCode), b2i(c.IsError), nstr(c.FaultCode), nstr(c.JobID), nstr(c.OutcomeReason))
	return err
}

func (o *ops) GetCall(ctx context.Context, callID string) (store.Call, error) {
	return scanCall(o.q.QueryRow(ctx, `SELECT `+callCols+` FROM calls WHERE call_id = ?`, callID))
}

func (o *ops) ListCalls(ctx context.Context, f store.CallFilter) ([]store.Call, error) {
	var where []string
	var args []any
	if f.DeviceID != "" {
		where, args = append(where, "device_id = ?"), append(args, f.DeviceID)
	}
	if f.DispatchedBootID != "" {
		where, args = append(where, "dispatched_boot_id = ?"), append(args, f.DispatchedBootID)
	}
	if len(f.States) > 0 {
		if err := store.CheckStates(f.States); err != nil {
			return nil, err
		}
		where, args = append(where, "state IN ("+placeholders(len(f.States))+")"), append(args, anys(f.States)...)
	}
	q := `SELECT ` + callCols + ` FROM calls`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	rows, err := o.q.QueryContext(ctx, q+` ORDER BY submitted_at, call_id`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, scanCall)
}

// resubmitSets starts a fresh attempt (lost → accepted): the new submission
// time, and every per-attempt column cleared (store.CallPatch).
const resubmitSets = `submitted_at = ?, dispatched_at = NULL, acked_at = NULL, finished_at = NULL,
	cancel_requested_at = NULL, exit_code = NULL, is_error = 0, fault_code = NULL, job_id = NULL,
	outcome_reason = NULL`

// patchSets returns the SET clauses and arguments for patch (see
// store.CallPatch). Write-once columns keep their first value; outcome
// columns take the patch's value.
func patchSets(p store.CallPatch) ([]string, []any) {
	var sets []string
	var args []any
	once := func(col string, set bool, v any) {
		if set {
			sets, args = append(sets, col+" = COALESCE("+col+", ?)"), append(args, v)
		}
	}
	outcome := func(col string, set bool, v any) {
		if set {
			sets, args = append(sets, col+" = ?"), append(args, v)
		}
	}
	// The boot needs no COALESCE: the dispatch only matches rows without
	// one (updateCall), and the calls_boot_write_once trigger backs that.
	if p.DispatchedBootID != "" {
		sets, args = append(sets, "dispatched_boot_id = ?"), append(args, p.DispatchedBootID)
	}
	once("dispatched_at", p.DispatchedAt != 0, p.DispatchedAt)
	once("acked_at", p.AckedAt != 0, p.AckedAt)
	once("cancel_requested_at", p.CancelRequestedAt != 0, p.CancelRequestedAt)
	once("job_id", p.JobID != "", p.JobID)
	outcome("finished_at", p.FinishedAt != 0, p.FinishedAt)
	outcome("exit_code", p.ExitCode != nil, nexit(p.ExitCode))
	outcome("is_error", p.IsError, int64(1))
	outcome("fault_code", p.FaultCode != "", p.FaultCode)
	outcome("outcome_reason", p.OutcomeReason != "", p.OutcomeReason)
	return sets, args
}

// updateCall applies sets to the call when it matches expect, reporting
// whether a row changed. undispatched additionally requires an unset
// dispatched boot, whatever expect says. A miss on a missing call is
// ErrNotFound.
func (o *ops) updateCall(ctx context.Context, callID string, expect store.CallExpect, undispatched bool, sets []string, args []any) (bool, error) {
	q := `UPDATE calls SET ` + strings.Join(sets, ", ") +
		` WHERE call_id = ? AND state IN (` + placeholders(len(expect.States)) + `)`
	args = append(append(args, callID), anys(expect.States)...)
	if expect.BootID != "" {
		q += ` AND dispatched_boot_id = ?`
		args = append(args, expect.BootID)
	}
	if expect.Undispatched || undispatched {
		q += ` AND dispatched_boot_id IS NULL`
	}
	var changed bool
	err := o.atomic(ctx, func(tx *ops) error {
		n, err := tx.exec(ctx, q, args...)
		if err != nil || n > 0 {
			changed = n > 0
			return err
		}
		var one int
		return mapErr(tx.q.QueryRow(ctx, `SELECT 1 FROM calls WHERE call_id = ?`, callID).Scan(&one))
	})
	return changed, err
}

func (o *ops) TransitionCall(ctx context.Context, callID string, expect store.CallExpect, to store.CallState, patch store.CallPatch) (bool, error) {
	if err := store.CheckTransition(expect, to, patch); err != nil {
		return false, err
	}
	var sets []string
	var args []any
	if to == store.CallAccepted {
		sets, args = []string{resubmitSets}, []any{patch.SubmittedAt}
	} else {
		sets, args = patchSets(patch)
	}
	sets, args = append([]string{"state = ?"}, sets...), append([]any{string(to)}, args...)
	// A call is dispatched at most once: the dispatch, and the resubmission
	// that leads to one, match only a row without a boot, even if expect
	// would admit one (outline 4.4).
	undispatched := to == store.CallDispatched || to == store.CallAccepted
	return o.updateCall(ctx, callID, expect, undispatched, sets, args)
}

func (o *ops) PatchCall(ctx context.Context, callID string, expect store.CallExpect, patch store.CallPatch) (bool, error) {
	if err := store.CheckPatch(expect, patch); err != nil {
		return false, err
	}
	sets, args := patchSets(patch)
	return o.updateCall(ctx, callID, expect, false, sets, args)
}

func (o *ops) LoseCalls(ctx context.Context, deviceID, keepBootID string, at int64, reason string) ([]store.LostCall, error) {
	if deviceID == "" || keepBootID == "" || at == 0 {
		return nil, invalid("LoseCalls needs a device id, the boot to keep and a time")
	}
	if err := store.CheckCodes(reason, ""); err != nil {
		return nil, err
	}
	// RETURNING yields new values only, so read the prior states first; the
	// transaction keeps the two statements on the same rows.
	const where = `WHERE device_id = ? AND state IN ('dispatched', 'running', 'unknown') AND dispatched_boot_id <> ?`
	var lost []store.LostCall
	err := o.atomic(ctx, func(tx *ops) error {
		rows, err := tx.q.QueryContext(ctx, `SELECT call_id, state FROM calls `+where+` ORDER BY submitted_at, call_id`,
			deviceID, keepBootID)
		if err != nil {
			return mapErr(err)
		}
		lost, err = collect(rows, func(s scanner) (store.LostCall, error) {
			var l store.LostCall
			err := s.Scan(&l.CallID, &l.From)
			return l, mapErr(err)
		})
		if err != nil || len(lost) == 0 {
			return err
		}
		n, err := tx.exec(ctx, `UPDATE calls SET state = 'lost', finished_at = ?, outcome_reason = ? `+where,
			at, nstr(reason), deviceID, keepBootID)
		if err == nil && n != len(lost) {
			err = fmt.Errorf("sqlite: LoseCalls selected %d calls but updated %d", len(lost), n)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return lost, nil
}

// Jobs.

const jobCols = `device_id, boot_id, job_id, user_id, session_id, call_id, state, exit_code, last_seq,
	started_at, ended_at, updated_at`

func scanJob(s scanner) (store.Job, error) {
	var j store.Job
	var state string
	err := s.Scan(&j.DeviceID, &j.BootID, &j.JobID, &j.UserID, nullStr{&j.SessionID}, nullStr{&j.CallID},
		&state, nullExit{&j.ExitCode}, &j.LastSeq, nullInt{&j.StartedAt}, nullInt{&j.EndedAt}, nullInt{&j.UpdatedAt})
	j.State = store.JobState(state)
	return j, mapErr(err)
}

func (o *ops) PutJob(ctx context.Context, j store.Job) (bool, error) {
	if j.DeviceID == "" || j.BootID == "" || j.JobID == "" || j.UserID == "" || !j.State.Valid() {
		return false, invalid("job needs device, boot, job and user ids and a valid state")
	}
	n, err := o.exec(ctx, `INSERT INTO jobs (`+jobCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT (device_id, boot_id, job_id) DO UPDATE SET
			session_id = COALESCE(excluded.session_id, jobs.session_id),
			call_id    = COALESCE(excluded.call_id, jobs.call_id),
			state      = excluded.state,
			exit_code  = COALESCE(excluded.exit_code, jobs.exit_code),
			last_seq   = excluded.last_seq,
			started_at = COALESCE(excluded.started_at, jobs.started_at),
			ended_at   = COALESCE(excluded.ended_at, jobs.ended_at),
			updated_at = COALESCE(excluded.updated_at, jobs.updated_at)
		WHERE excluded.last_seq > jobs.last_seq AND jobs.state = 'running'`,
		j.DeviceID, j.BootID, j.JobID, j.UserID, nstr(j.SessionID), nstr(j.CallID), string(j.State),
		nexit(j.ExitCode), j.LastSeq, nint(j.StartedAt), nint(j.EndedAt), nint(j.UpdatedAt))
	return n > 0, err
}

func (o *ops) ListJobs(ctx context.Context, f store.JobFilter) ([]store.Job, error) {
	var where []string
	var args []any
	for _, c := range []struct{ col, v string }{
		{"device_id", f.DeviceID}, {"boot_id", f.BootID}, {"session_id", f.SessionID},
	} {
		if c.v != "" {
			where, args = append(where, c.col+" = ?"), append(args, c.v)
		}
	}
	if len(f.States) > 0 {
		for _, s := range f.States {
			if !s.Valid() {
				return nil, invalid("unknown job state %q", s)
			}
		}
		where, args = append(where, "state IN ("+placeholders(len(f.States))+")"), append(args, anys(f.States)...)
	}
	q := `SELECT ` + jobCols + ` FROM jobs`
	if len(where) > 0 {
		q += ` WHERE ` + strings.Join(where, " AND ")
	}
	rows, err := o.q.QueryContext(ctx, q+` ORDER BY device_id, boot_id, job_id`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, scanJob)
}

func (o *ops) LoseJobs(ctx context.Context, deviceID, keepBootID string, at int64) (int, error) {
	if deviceID == "" || keepBootID == "" || at == 0 {
		return 0, invalid("LoseJobs needs a device id, the boot to keep and a time")
	}
	return o.exec(ctx, `UPDATE jobs SET state = 'lost', ended_at = COALESCE(ended_at, ?), updated_at = ?
		WHERE device_id = ? AND boot_id <> ? AND state = 'running'`, at, at, deviceID, keepBootID)
}

// Timers.

const timerCols = `kind, subject_id, due_at, claimed_by, claim_until, attempts, claim_token`

func scanTimer(s scanner) (store.Timer, error) {
	var t store.Timer
	var attempts int64
	err := s.Scan(&t.Kind, &t.SubjectID, &t.DueAt, nullStr{&t.ClaimedBy}, nullInt{&t.ClaimUntil}, &attempts, nullStr{&t.Token})
	t.Attempts = int(attempts)
	return t, mapErr(err)
}

func (o *ops) SetTimer(ctx context.Context, kind, subjectID string, dueAt int64) error {
	if kind == "" || subjectID == "" {
		return invalid("timer needs kind and subject id")
	}
	_, err := o.exec(ctx, `INSERT INTO timers (kind, subject_id, due_at) VALUES (?, ?, ?)
		ON CONFLICT (kind, subject_id) DO UPDATE SET
			due_at = excluded.due_at, claimed_by = NULL, claim_until = NULL, attempts = 0, claim_token = NULL`,
		kind, subjectID, dueAt)
	return err
}

func (o *ops) CancelTimer(ctx context.Context, kind, subjectID string) error {
	_, err := o.exec(ctx, `DELETE FROM timers WHERE kind = ? AND subject_id = ?`, kind, subjectID)
	return err
}

func (o *ops) ClaimDueTimers(ctx context.Context, now int64, instanceID string, lease int64, limit int) ([]store.Timer, error) {
	if instanceID == "" || lease < 1 || limit <= 0 {
		return nil, invalid("ClaimDueTimers needs an instance id, a lease of at least 1ms and a positive limit")
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	// One token per claim batch: rows differ by key, so (key, token) still
	// names exactly one claim.
	token := hex.EncodeToString(b[:])
	// A single UPDATE … RETURNING: the write lock makes selecting and
	// claiming one atomic step, so two claimants never hold the same timer.
	rows, err := o.q.QueryContext(ctx, `UPDATE timers
		SET claimed_by = ?, claim_until = ?, attempts = attempts + 1, claim_token = ?
		WHERE rowid IN (
			SELECT rowid FROM timers
			WHERE due_at <= ? AND (claim_until IS NULL OR claim_until <= ?)
			ORDER BY due_at, kind, subject_id
			LIMIT ?)
		RETURNING `+timerCols, instanceID, now+lease, token, now, now, limit)
	if err != nil {
		return nil, mapErr(err)
	}
	ts, err := collect(rows, scanTimer)
	if err != nil {
		return nil, err
	}
	// RETURNING order is unspecified.
	slices.SortFunc(ts, compareTimers)
	return ts, nil
}

func compareTimers(a, b store.Timer) int {
	if a.DueAt != b.DueAt {
		if a.DueAt < b.DueAt {
			return -1
		}
		return 1
	}
	if c := strings.Compare(a.Kind, b.Kind); c != 0 {
		return c
	}
	return strings.Compare(a.SubjectID, b.SubjectID)
}

func (o *ops) CompleteTimer(ctx context.Context, t store.Timer) (bool, error) {
	if t.Token == "" {
		return false, invalid("CompleteTimer needs a claimed timer")
	}
	n, err := o.exec(ctx, `DELETE FROM timers WHERE kind = ? AND subject_id = ? AND claim_token = ?`,
		t.Kind, t.SubjectID, t.Token)
	return n > 0, err
}

func (o *ops) ListTimers(ctx context.Context) ([]store.Timer, error) {
	rows, err := o.q.QueryContext(ctx, `SELECT `+timerCols+` FROM timers ORDER BY due_at, kind, subject_id`)
	if err != nil {
		return nil, mapErr(err)
	}
	return collect(rows, scanTimer)
}

func collect[T any](rows *sql.Rows, scan func(scanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, mapErr(rows.Err())
}
