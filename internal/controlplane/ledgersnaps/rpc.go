package ledgersnaps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
)

// Types of a Refusal: the google.rpc.PreconditionFailure types of
// MergeSessionLedger and DeleteSession.
const (
	RefuseTurnRunning      = "TURN_RUNNING"
	RefuseNotFastForward   = "NOT_FAST_FORWARD"
	RefuseDeviceOffline    = "DEVICE_OFFLINE"
	RefuseProblems         = "PROBLEMS"
	RefuseNoChanges        = "NO_CHANGES"
	RefuseChangesetChanged = "CHANGESET_CHANGED"
	RefuseLedgerChanges    = "LEDGER_CHANGES"
)

// Refusal refuses a session operation for a precondition.
type Refusal struct {
	Type, Subject, Description string
}

func (r *Refusal) Error() string { return "ledgersnaps: " + r.Type + ": " + r.Description }

// SessionDeleted is the discard reason of a deleted session's changeset.
const SessionDeleted = "session deleted"

// NewSessionLedger is the snapshot row of a new session: waiting to be
// prepared from head, the main line's head (empty for an empty ledger).
func NewSessionLedger(sess store.Session, head string) store.SessionLedger {
	return store.SessionLedger{SessionID: sess.ID, UserID: sess.UserID, ProjectID: sess.ProjectID, DeviceID: sess.DefaultDeviceID,
		State: store.SessionLedgerPending, BaseCommit: head, ViewCommit: head, CreatedAt: sess.CreatedAt, UpdatedAt: sess.CreatedAt}
}

// forced is a forced collection: of the device when it answered, else the
// last one with a warning.
type forced struct {
	Ledger  store.SessionLedger
	Check   *ledger.SessionCheckResult
	Offline bool
}

// offlineWarning says that a check used an older collection.
func offlineWarning(l store.SessionLedger) string {
	if l.CollectedAt == 0 {
		return "the session's device is offline, or could not collect its snapshot, and the snapshot was never collected; the changeset is as it was opened"
	}
	return "the session's device is offline, or could not collect its snapshot; using the snapshot as collected at " +
		time.UnixMilli(l.CollectedAt).UTC().Format("2006-01-02 15:04:05 UTC")
}

// force collects the session's snapshot now, with the session's lock held.
func (m *Manager) force(ctx context.Context, sessionID string) (forced, error) {
	l, err := m.cfg.Store.GetSessionLedger(ctx, sessionID)
	if err != nil {
		return forced{}, err
	}
	if l.State != store.SessionLedgerReady || l.Pending != "" {
		return forced{Ledger: l, Offline: true}, nil
	}
	release, err := m.waitDevice(ctx, l.DeviceID)
	if err != nil {
		return forced{}, err
	}
	defer release()
	c, err := m.collect(ctx, l)
	switch {
	case err == nil:
		return forced{Ledger: c.Ledger, Check: c.Check}, nil
	case ctx.Err() != nil:
		return forced{}, ctx.Err()
	}
	// The device is offline, or could not collect (the collection is
	// retried in the background): the last collection stands.
	if !errors.Is(err, errOffline) {
		m.logError("forced collection", sessionID, err)
	}
	if l, err = m.cfg.Store.GetSessionLedger(ctx, sessionID); err != nil {
		return forced{}, err
	}
	return forced{Ledger: l, Offline: true}, nil
}

// recheck checks changeset c with the session's last collection, read
// again now. It refuses when c is no longer the session's open changeset
// (merged or discarded, the snapshot owing its rebase or reset): the last
// collection is not c's then.
func (m *Manager) recheck(ctx context.Context, sessionID string, c store.LedgerChangeset) (ledger.SessionCheckResult, error) {
	l, err := m.cfg.Store.GetSessionLedger(ctx, sessionID)
	if err != nil {
		return ledger.SessionCheckResult{}, err
	}
	if l.ChangesetID != c.ID {
		return ledger.SessionCheckResult{}, changesetChanged(c.ID)
	}
	changes := make([]ledger.SnapshotChange, 0, len(l.Known))
	for _, k := range l.Known {
		changes = append(changes, ledger.SnapshotChange{Path: k.Path, Deleted: k.Deleted, Known: !k.Deleted, SHA256: k.SHA256})
	}
	return m.checkSession(ctx, l, changes)
}

// enter takes the session's lock, then an in-flight token: RPCs waiting
// for a session's lock hold no token, which other sessions' work needs.
func (m *Manager) enter(ctx context.Context, sessionID string) (func(), error) {
	unlock, err := m.lock(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	release, err := m.takeFlight(ctx)
	if err != nil {
		unlock()
		return nil, err
	}
	return func() {
		unlock()
		release()
		// Work the session's lock held back runs now.
		m.Kick(sessionID)
	}, nil
}

// ChangesetCheck is CheckChangeset of a session's changeset.
type ChangesetCheck struct {
	ledger.CheckResult
	// Problems are the last collection's: validation and what the device
	// could not collect.
	Problems []store.LedgerProblem
	Warning  string
}

func changesetChanged(id string) error {
	return &ledger.PreconditionError{Violations: []ledger.Violation{{Type: ledger.ViolationChangesetChanged, Subject: id,
		Description: "the session's open changeset changed while it was collected; list the session's changesets again"}}}
}

// CheckChangeset collects the snapshot of the session of changeset c (or,
// with its device offline, takes the last collection) and checks c with
// it, for an integrator. The caller holds no write slot.
func (m *Manager) CheckChangeset(ctx context.Context, c store.LedgerChangeset) (ChangesetCheck, error) {
	leave, err := m.enter(ctx, c.SessionID)
	if err != nil {
		return ChangesetCheck{}, err
	}
	defer leave()
	f, err := m.force(ctx, c.SessionID)
	if err != nil {
		return ChangesetCheck{}, err
	}
	out := ChangesetCheck{Problems: f.Ledger.Problems}
	check := f.Check
	if check == nil {
		if f.Offline {
			out.Warning = offlineWarning(f.Ledger)
		}
		res, err := m.recheck(ctx, c.SessionID, c)
		if err != nil {
			return ChangesetCheck{}, err
		}
		check = &res
	}
	if check.Changeset.ID != c.ID {
		return ChangesetCheck{}, changesetChanged(c.ID)
	}
	out.CheckResult = check.CheckResult
	return out, nil
}

// MergeChangeset collects the snapshot of the session of changeset c (or
// takes the last collection) and merges c for an integrator. The rebase of
// the snapshot follows in the background. The caller holds no write slot.
//
// It is refused (TURN_RUNNING) while a turn of the session runs, so the
// merge's collection, the hold on the session's turns and the rebase
// follow one another with no agent work in between; the refusal is
// checked again under the ledger's lock right before the merge.
func (m *Manager) MergeChangeset(ctx context.Context, userID string, c store.LedgerChangeset, o ledger.MergeOptions) (store.LedgerChangeset, error) {
	leave, err := m.enter(ctx, c.SessionID)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	defer leave()
	if err := m.turnRunning(ctx, c.SessionID); err != nil {
		return store.LedgerChangeset{}, err
	}
	authorize := o.Authorize
	o.Authorize = func(ctx context.Context, fastForward bool) error {
		if err := m.turnRunning(ctx, c.SessionID); err != nil {
			return err
		}
		if authorize == nil {
			return nil
		}
		return authorize(ctx, fastForward)
	}
	if c.State == store.LedgerChangesetOpen {
		f, err := m.force(ctx, c.SessionID)
		if err != nil {
			return store.LedgerChangeset{}, err
		}
		if f.Ledger.ChangesetID != c.ID {
			return store.LedgerChangeset{}, changesetChanged(c.ID)
		}
		if f.Ledger.ProblemCount > 0 {
			return store.LedgerChangeset{}, problemsError(f.Ledger.Problems)
		}
	}
	return m.merge(ctx, c.ID, c.SessionID, userID, o)
}

func problemsError(problems []store.LedgerProblem) error {
	var out []ledger.Violation
	for _, p := range problems {
		if len(out) == ledger.MaxViolations {
			break
		}
		out = append(out, ledger.Violation{Type: ledger.ViolationProblem, Subject: p.Path,
			Description: p.Message})
	}
	return &ledger.PreconditionError{Violations: out}
}

var errNotFastForward = errors.New("ledgersnaps: the changeset does not fast-forward")

// merge merges a session's changeset through a write slot of userID and
// announces it.
func (m *Manager) merge(ctx context.Context, id, sessionID, userID string, o ledger.MergeOptions) (store.LedgerChangeset, error) {
	if err := m.cfg.Slots.Take(ctx, userID); err != nil {
		return store.LedgerChangeset{}, err
	}
	merged, err := m.cfg.Ledger.MergeChangeset(ctx, id, o)
	m.cfg.Slots.Release(userID)
	if err == nil {
		m.publishDurable(context.WithoutCancel(ctx), sessionID)
		m.holdStarts(sessionID)
	}
	return merged, asTurnRunning(err, sessionID)
}

// holdStarts tells the session service that the session's turns are held
// now (a rebase or a reset is owed), so its holder is not nudged to start
// one meanwhile.
func (m *Manager) holdStarts(sessionID string) {
	if m.cfg.Waker != nil {
		m.cfg.Waker.Wake(sessionID)
	}
}

// turnRunning refuses (TURN_RUNNING) an integrator's merge or discard of a
// session's changeset while a turn of the session runs.
func (m *Manager) turnRunning(ctx context.Context, sessionID string) error {
	sess, err := m.cfg.Store.GetSession(ctx, sessionID, m.now())
	if err != nil {
		return err
	}
	if sess.State != store.SessionRunning {
		return nil
	}
	return turnRunningError(sessionID)
}

const turnRunningText = "a turn of the session is running or about to start; merge or discard its changeset after the turn ends"

func turnRunningError(sessionID string) error {
	return &ledger.PreconditionError{Violations: []ledger.Violation{{Type: RefuseTurnRunning, Subject: sessionID, Description: turnRunningText}}}
}

// asTurnRunning maps the store's refusal of a merge or a discard while a
// turn runs or is about to start (inputs taken for it), checked in the
// transaction that records the merge or the discard.
func asTurnRunning(err error, sessionID string) error {
	if errors.Is(err, store.ErrTurnRunning) {
		return turnRunningError(sessionID)
	}
	return err
}

// DiscardChangeset discards a session's changeset; the snapshot is
// written again in the background and the agent told. It is refused
// (TURN_RUNNING) while a turn of the session runs, as MergeChangeset is.
func (m *Manager) DiscardChangeset(ctx context.Context, c store.LedgerChangeset, reason, author string) (store.LedgerChangeset, error) {
	unlock, err := m.lock(ctx, c.SessionID)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	defer func() {
		unlock()
		m.Kick(c.SessionID)
	}()
	if err := m.turnRunning(ctx, c.SessionID); err != nil {
		return store.LedgerChangeset{}, err
	}
	discarded, err := m.cfg.Ledger.DiscardChangeset(ctx, c.ID, reason, author)
	if err == nil {
		m.publishDurable(context.WithoutCancel(ctx), c.SessionID)
		m.holdStarts(c.SessionID)
	}
	return discarded, asTurnRunning(err, c.SessionID)
}

// MergeSession merges the session's open changeset when it fast-forwards,
// for the project's owner (MergeSessionLedger). It collects the snapshot
// first. A precondition that fails is a *Refusal.
func (m *Manager) MergeSession(ctx context.Context, sessionID, expected string) (store.LedgerChangeset, error) {
	leave, err := m.enter(ctx, sessionID)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	defer leave()
	l, err := m.cfg.Store.GetSessionLedger(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return store.LedgerChangeset{}, &Refusal{Type: RefuseNoChanges, Subject: sessionID, Description: "the session has no ledger snapshot"}
	}
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	running := func() error {
		sess, err := m.cfg.Store.GetSession(ctx, sessionID, m.now())
		if err != nil {
			return err
		}
		if sess.State == store.SessionRunning {
			return &Refusal{Type: RefuseTurnRunning, Subject: sessionID, Description: "a turn is running; merge after it ends"}
		}
		return nil
	}
	if err := running(); err != nil {
		return store.LedgerChangeset{}, err
	}
	f, err := m.force(ctx, sessionID)
	if err != nil {
		return store.LedgerChangeset{}, err
	}
	l = f.Ledger
	switch {
	case f.Offline && l.State == store.SessionLedgerReady && l.Pending == "" && l.CollectionDue():
		return store.LedgerChangeset{}, &Refusal{Type: RefuseDeviceOffline, Subject: sessionID,
			Description: "the device is offline (or could not collect the snapshot) and a turn ended after the last collection, so its changes are not known yet"}
	case l.ChangesetID == "" || l.ChangeCount == 0:
		return store.LedgerChangeset{}, &Refusal{Type: RefuseNoChanges, Subject: sessionID, Description: "the session has no open changeset with changes"}
	case expected != l.ChangesetID:
		return store.LedgerChangeset{}, &Refusal{Type: RefuseChangesetChanged, Subject: l.ChangesetID,
			Description: "the session's open changeset is not the one expected"}
	case l.ProblemCount > 0:
		return store.LedgerChangeset{}, &Refusal{Type: RefuseProblems, Subject: l.ChangesetID,
			Description: fmt.Sprintf("the changeset has %d problems", l.ProblemCount)}
	}
	if err := running(); err != nil {
		return store.LedgerChangeset{}, err
	}
	merged, err := m.merge(ctx, l.ChangesetID, sessionID, l.UserID, ledger.MergeOptions{Author: ledger.SessionAuthor(sessionID),
		Authorize: func(_ context.Context, fastForward bool) error {
			if !fastForward {
				return errNotFastForward
			}
			return nil
		}})
	var precondition *ledger.PreconditionError
	switch {
	case errors.As(err, &precondition) && len(precondition.Violations) == 1 && precondition.Violations[0].Type == RefuseTurnRunning:
		return store.LedgerChangeset{}, &Refusal{Type: RefuseTurnRunning, Subject: sessionID, Description: turnRunningText}
	case errors.Is(err, errNotFastForward):
		return store.LedgerChangeset{}, &Refusal{Type: RefuseNotFastForward, Subject: l.ChangesetID,
			Description: "the changeset's base differs from the main line's head; an integrator merges it with `circus ledger merge`"}
	case errors.As(err, &precondition):
		var parts []string
		for _, v := range precondition.Violations {
			parts = append(parts, strings.TrimSpace(v.Subject+" "+v.Description))
		}
		return store.LedgerChangeset{}, &Refusal{Type: RefuseProblems, Subject: l.ChangesetID, Description: strings.Join(parts, "; ")}
	}
	return merged, err
}

// DeleteSession deletes a session with its snapshot: with the session's
// lock held it runs check, refuses a session whose changeset is open with
// changes, or whose collection is due (it collects first when it can),
// unless discard (LEDGER_CHANGES), and runs del (the store's deletion,
// which leaves the snapshot's removal behind). Only once the session is
// deleted is its open changeset discarded and its view forgotten; what a
// failure leaves behind, the periodic sweep of orphaned changesets
// finishes. The device removes the snapshot when it can.
func (m *Manager) DeleteSession(ctx context.Context, sess store.Session, discard bool, check, del func(ctx context.Context) error) error {
	leave, err := m.enter(ctx, sess.ID)
	if err != nil {
		return err
	}
	defer leave()
	if err := check(ctx); err != nil {
		return err
	}
	l, err := m.cfg.Store.GetSessionLedger(ctx, sess.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return del(ctx)
	case err != nil:
		return err
	}
	if l.CollectionDue() && !discard {
		// A stopped session starts no turn: collected now, the snapshot
		// says whether it holds changes.
		f, err := m.force(ctx, sess.ID)
		if err != nil {
			return err
		}
		l = f.Ledger
	}
	changes := l.ChangesetID != "" && l.ChangeCount > 0 || l.CollectionDue()
	if changes && !discard {
		return &Refusal{Type: RefuseLedgerChanges, Subject: sess.ID,
			Description: "the session's ledger changes are not merged; delete it with discard_ledger_changes to discard them"}
	}
	if err := del(ctx); err != nil {
		return err
	}
	m.forget(context.WithoutCancel(ctx), store.LedgerChangeset{ID: l.ChangesetID, ProjectID: l.ProjectID, SessionID: sess.ID})
	m.RemoveSnapshots()
	return nil
}

// forget discards the open changeset c of a deleted session (none when
// c.ID is empty) and forgets the session's view.
func (m *Manager) forget(ctx context.Context, c store.LedgerChangeset) {
	if c.ID != "" {
		if _, err := m.cfg.Ledger.DiscardChangeset(ctx, c.ID, SessionDeleted, ledger.SessionAuthor(c.SessionID)); err != nil {
			// The sweep discards it later.
			m.logError("discard deleted session's changeset", c.SessionID, err)
			return
		}
	}
	m.logError("forget session view", c.SessionID, m.cfg.Ledger.RemoveSession(ctx, c.ProjectID, c.SessionID))
}

// sweep discards the open changesets of sessions that no longer exist,
// which a DeleteSession that failed after the deletion left behind: one
// page per pass, from where the last pass stopped, so orphans whose
// discard keeps failing do not keep the others from their turn.
func (m *Manager) sweep(ctx context.Context) {
	orphans, err := m.cfg.Store.ListOrphanSessionChangesets(ctx, m.sweepAfter, sweepPage)
	if err != nil {
		m.logError("list orphaned changesets", "", err)
		return
	}
	for _, c := range orphans {
		m.forget(ctx, c)
	}
	m.sweepAfter = ""
	if len(orphans) == sweepPage {
		m.sweepAfter = orphans[len(orphans)-1].ID
	}
}

// RemoveSnapshots asks for the removals of deleted sessions' snapshots to
// run, as after a session or project deletion.
func (m *Manager) RemoveSnapshots() {
	select {
	case m.removalHint <- struct{}{}:
	default:
	}
}
