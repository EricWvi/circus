package ledgersnaps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/ledger/snapshot"
	"github.com/EricWvi/circus/internal/store"
	"github.com/google/uuid"
)

const (
	// A preparation sends the base's files in chunks of at most this much
	// content and this many files.
	chunkBytes = 8 << 20
	chunkFiles = 4096
	// maxEditBytes bounds the content of one rebase's edits.
	maxEditBytes = 8 << 20
	// maxRebaseAttempts bounds the collections one rebase step makes (a
	// rebase found stale, a base adopted, known changes dropped) before
	// it waits for its backoff.
	maxRebaseAttempts = 3
	// maxNotesWithoutUser bounds the notes in a row, each of which starts
	// a turn, that the agent gets without an input of the user between
	// them. An agent that cannot fix its snapshot would otherwise be
	// told of new problems after every turn its last note started,
	// forever; past the bound it hears of them after the user's next
	// input. Notes of merges and discards, one per integrator action, are
	// always given but start no turn (they are deferred: they go with the
	// next input that starts one), so they do not count towards the
	// bound.
	maxNotesWithoutUser = 3
	// maxDeviceFailures is how many attempts in a row a rebase or a reset
	// may fail for a reason a retry alone does not fix (the device refuses
	// it: permissions, a full disk, a file in the way; or the snapshot
	// keeps changing while it is rebased) before the failure is recorded
	// as a problem of the snapshot. With the backoff of 5 s and 10 s
	// between them that is about 15 s: long enough for a passing
	// condition to clear, short enough that a session is not held without
	// a word. The problem ends the hold, tells the agent and shows in the
	// status; the step is tried again after the agent's next turn or its
	// backoff.
	maxDeviceFailures = 3
	// emptyTree is the base commit a snapshot of an empty ledger records:
	// the id of Git's empty tree, as the ledger has no commit to name.
	emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"
)

// Events of KIND_LEDGER: "prepared" for changes of the preparation,
// "failed" when it fails, "collected" for everything about collections,
// rebases and resets, and a turn's end that makes a collection due. The
// store records "merged" and "discarded" itself, durably.
const (
	eventPrepared  = "prepared"
	eventFailed    = "failed"
	eventCollected = "collected"
)

// deviceCommit is the base commit a device records for commit.
func deviceCommit(commit string) string {
	if commit == "" {
		return emptyTree
	}
	return commit
}

// ledgerCommit is the base commit of the row for a device's base commit.
func ledgerCommit(commit string) string {
	if commit == emptyTree {
		return ""
	}
	return commit
}

// note is a note for the agent that a change of a snapshot row gives: an
// input of kind note, which starts a turn when the session is idle, or
// when deferred waits for the next input that starts one and goes before
// it in that turn (store.Input.Deferred). It is written in the change's
// transaction, so it is given exactly when the change is.
type note struct {
	category, text string
	deferred       bool
	// refused, when set, undoes what the change records of the note when
	// the session takes none (stopped, or too many inputs open).
	refused func(l *store.SessionLedger)
}

var errRaced = errors.New("ledgersnaps: the snapshot row changed")

// change applies fn to the session's snapshot row in a transaction and
// writes it, with the note fn returns, when fn returns true; it announces
// the change with a KIND_LEDGER event and wakes the session for a new
// note that is not deferred.
func (m *Manager) change(ctx context.Context, sessionID, event string,
	fn func(ctx context.Context, tx store.Ops, l *store.SessionLedger) (bool, *note, error)) (store.SessionLedger, bool, error) {
	for range 8 {
		var out store.SessionLedger
		var wrote, noted bool
		err := m.cfg.Store.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			wrote, noted = false, false
			l, err := tx.GetSessionLedger(ctx, sessionID)
			if err != nil {
				return err
			}
			out = l
			prev := l.UpdatedAt
			ok, n, err := fn(ctx, tx, &l)
			if err != nil || !ok {
				return err
			}
			if n != nil {
				created, err := m.submitNote(ctx, tx, sessionID, l.Path, n)
				switch {
				case errors.Is(err, store.ErrStopped) || errors.Is(err, store.ErrLimit):
					if n.refused != nil {
						n.refused(&l)
					}
				case err != nil:
					return err
				default:
					noted = created && !n.deferred
				}
			}
			l.UpdatedAt = max(m.now(), prev+1)
			ok, err = tx.UpdateSessionLedger(ctx, l, prev)
			if err != nil {
				return err
			}
			if !ok {
				return errRaced
			}
			out, wrote = l, true
			return nil
		})
		if errors.Is(err, errRaced) {
			continue
		}
		if err != nil {
			return out, false, err
		}
		if wrote {
			m.publish(out, event)
			if noted && m.cfg.Waker != nil {
				m.cfg.Waker.Wake(sessionID, circusv1.WorkerEvent_Nudge_REASON_INPUT)
			}
		}
		return out, wrote, nil
	}
	return store.SessionLedger{}, false, fmt.Errorf("%w: the snapshot of session %s keeps changing", store.ErrBusy, sessionID)
}

// update is change without a note.
func (m *Manager) update(ctx context.Context, sessionID, event string, fn func(l *store.SessionLedger) bool) (store.SessionLedger, bool, error) {
	return m.change(ctx, sessionID, event, func(_ context.Context, _ store.Ops, l *store.SessionLedger) (bool, *note, error) {
		return fn(l), nil, nil
	})
}

// submitNote gives the agent n in tx. A deferred note at the bound of
// the session's held notes first folds them into one (foldedText), so
// none is dropped without a word.
func (m *Manager) submitNote(ctx context.Context, tx store.Ops, sessionID, snapshotPath string, n *note) (bool, error) {
	input := func(category, text string) (store.Input, error) {
		id, err := uuid.NewV7()
		return store.Input{InputID: id.String(), SessionID: sessionID, ClientMsgID: "ledger-note:" + id.String(), Kind: store.InputNote,
			Note: category, Text: clipText(text, store.MaxInputTextBytes), Priority: store.InputNormal, CreatedAt: m.now(), Deferred: n.deferred}, err
	}
	if n.deferred {
		folded, err := input(noteMerged, foldedText(snapshotPath))
		if err != nil {
			return false, err
		}
		if _, err := tx.FoldDeferredNotes(ctx, folded); err != nil {
			return false, err
		}
	}
	in, err := input(n.category, n.text)
	if err != nil {
		return false, err
	}
	res, err := tx.SubmitInput(ctx, in)
	return res.Created, err
}

// problemsNote is the note telling the agent of the problems all of
// snapshot l, recorded in l: none when it was told of them already, or
// when it got maxNotesWithoutUser notes since the user's last input (it
// hears of them after the user's next one).
func problemsNote(ctx context.Context, tx store.Ops, l *store.SessionLedger, all []store.LedgerProblem) (*note, error) {
	hash := problemHash(all)
	switch {
	case hash == "":
		l.NotedHash = ""
		return nil, nil
	case l.NotedHash == hash:
		return nil, nil
	}
	n, err := tx.CountNotesSinceUserInput(ctx, l.SessionID)
	if err != nil || n >= maxNotesWithoutUser {
		return nil, err
	}
	prev := l.NotedHash
	l.NotedHash = hash
	return &note{category: noteProblems, text: problemsText(l.Path, all), refused: func(x *store.SessionLedger) { x.NotedHash = prev }}, nil
}

// publish announces a change of the session's snapshot to its watchers.
func (m *Manager) publish(l store.SessionLedger, event string) {
	if m.cfg.Bus == nil {
		return
	}
	payload := fmt.Sprintf(`{"event":%q,"changeset_id":%q}`, event, l.ChangesetID)
	m.cfg.Bus.Ephemeral(l.SessionID, &circusv1.SessionEvent{Kind: circusv1.SessionEvent_KIND_LEDGER, TsUnixMs: max(l.UpdatedAt, m.now()),
		PayloadJson: payload, Ephemeral: true})
}

// publishDurable announces the durable events the store recorded for the
// session, as a merge or a discard of its changeset does.
func (m *Manager) publishDurable(ctx context.Context, sessionID string) {
	if m.cfg.Bus == nil {
		return
	}
	sess, err := m.cfg.Store.GetSession(ctx, sessionID, m.now())
	if err == nil && sess.LastEventSeq > 0 {
		m.cfg.Bus.Durable(sessionID, uint64(sess.LastEventSeq))
	}
}

// later schedules the session's next attempt after a backoff.
func (m *Manager) later(ctx context.Context, sessionID string, cause error) {
	m.logError("snapshot step", sessionID, cause)
	l, ok, err := m.update(ctx, sessionID, eventCollected, func(l *store.SessionLedger) bool {
		l.Attempts++
		l.RetryAt = m.now() + backoff(l.Attempts-1).Milliseconds()
		return true
	})
	if err != nil {
		m.logError("schedule retry", sessionID, err)
		return
	}
	if ok {
		m.retryAt(sessionID, l.RetryAt)
	}
}

// fail ends a snapshot: the session runs without a ledger.
func (m *Manager) fail(ctx context.Context, sessionID string, code circusv1.LedgerSnapshotError, detail string) {
	_, ok, err := m.update(ctx, sessionID, eventFailed, func(l *store.SessionLedger) bool {
		if l.State == store.SessionLedgerFailed {
			return false
		}
		l.State, l.Path, l.ErrorCode, l.ErrorDetail = store.SessionLedgerFailed, "", int32(code), clipText(detail, store.MaxSessionLedgerMessageBytes)
		l.Attempts, l.RetryAt, l.DueAt, l.Pending, l.PendingChangeset, l.PendingReason, l.RebaseTarget = 0, 0, 0, "", "", "", ""
		return true
	})
	m.logError("fail snapshot", sessionID, err)
	if ok && m.cfg.Waker != nil {
		m.cfg.Waker.Wake(sessionID, circusv1.WorkerEvent_Nudge_REASON_INPUT)
	}
}

// failUnusable fails the snapshot of a device that cannot take its calls.
func (m *Manager) failUnusable(ctx context.Context, sessionID string, err error) {
	m.fail(ctx, sessionID, circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_UNSPECIFIED, cmpErr(err, errRevoked).Error())
}

// wait records that a preparation waits for its offline device; it holds
// the session's turns, as for a workspace.
func (m *Manager) wait(ctx context.Context, sessionID string) {
	_, _, err := m.update(ctx, sessionID, eventPrepared, func(l *store.SessionLedger) bool {
		if !l.State.Unprepared() || l.State == store.SessionLedgerPending && l.ErrorCode == 0 {
			return false
		}
		l.State, l.ErrorCode, l.ErrorDetail, l.Attempts, l.RetryAt = store.SessionLedgerPending, 0, "", 0, 0
		return true
	})
	m.logError("wait for device", sessionID, err)
}

func clipText(s string, n int) string {
	s = strings.ToValidUTF8(s, "")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// send writes commit's files to the session's snapshot (a preparation, or
// with reset a replacement) and returns its path, or the outcome that
// stopped it. failed holds a daemon's error.
func (m *Manager) send(ctx context.Context, l store.SessionLedger, commit string, reset bool) (snapshotPath string, o outcome, failed *circusv1.PrepareLedgerResult, err error) {
	after, total := "", uint32(0)
	for chunk := uint32(0); ; chunk++ {
		page, err := m.cfg.Ledger.ReadSnapshotFiles(ctx, l.ProjectID, commit, after, min(chunkBytes, m.cfg.MaxMessageBytes/2), chunkFiles)
		if err != nil {
			return "", transient, nil, err
		}
		req := &circusv1.PrepareLedgerRequest{SnapshotId: l.SessionID, BaseCommit: deviceCommit(commit), ChunkIndex: chunk, Reset_: reset}
		for _, f := range page.Files {
			if len(f.Content) > snapshot.MaxBytes(f.Path) {
				return "", gotResult, &circusv1.PrepareLedgerResult{Outcome: circusv1.PrepareLedgerResult_OUTCOME_FAILED,
					Error:       circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_INVALID_REQUEST,
					ErrorDetail: clipText(fmt.Sprintf("%s exceeds the %d bytes a snapshot file may hold", f.Path, snapshot.MaxBytes(f.Path)), 1024)}, nil
			}
			req.Files = append(req.Files, &circusv1.SnapshotFile{Path: f.Path, Content: string(f.Content)})
			after = f.Path
		}
		total += uint32(len(page.Files))
		req.Last = !page.More
		if req.Last {
			req.TotalFiles = total
		}
		res, o, err := m.call(ctx, l.UserID, l.DeviceID, l.SessionID, prepareTimeout,
			&circusv1.DeviceCall{Tool: &circusv1.DeviceCall_PrepareLedger{PrepareLedger: req}})
		if o != gotResult {
			return "", o, nil, err
		}
		got := res.GetPrepareLedger()
		switch got.GetOutcome() {
		case circusv1.PrepareLedgerResult_OUTCOME_CHUNK_ACCEPTED:
			if req.Last {
				return "", transient, nil, errors.New("ledgersnaps: the last chunk was only accepted")
			}
			continue
		case circusv1.PrepareLedgerResult_OUTCOME_READY:
			p := got.GetPath()
			if !path.IsAbs(p) || path.Clean(p) != p || len(p) > 4096 || !utf8.ValidString(p) || strings.ContainsRune(p, 0) {
				return "", transient, nil, errors.New("ledgersnaps: invalid snapshot path")
			}
			return p, gotResult, nil, nil
		case circusv1.PrepareLedgerResult_OUTCOME_FAILED:
			return "", gotResult, got, nil
		}
		return "", transient, nil, errors.New("ledgersnaps: invalid prepare result")
	}
}

// prepare writes the session's snapshot on its device, from the main
// line's head when the preparation starts.
func (m *Manager) prepare(ctx context.Context, l store.SessionLedger) {
	sid := l.SessionID
	if _, deleted, err := m.deviceName(ctx, l.DeviceID); err != nil {
		m.logError("read device", sid, err)
		return
	} else if deleted {
		m.failUnusable(ctx, sid, errRevoked)
		return
	}
	if !m.cfg.Online(l.DeviceID) {
		m.wait(ctx, sid)
		return
	}
	// A preparation that starts late (its device was offline) starts from
	// the head of now: the session's first changeset is based on it.
	head, err := m.cfg.Store.GetLedgerHead(ctx, l.ProjectID)
	if err != nil {
		m.later(ctx, sid, err)
		return
	}
	l, _, err = m.update(ctx, sid, eventPrepared, func(x *store.SessionLedger) bool {
		if !x.State.Unprepared() {
			return false
		}
		moved := x.ChangesetID == "" && x.BaseCommit != head
		if x.State == store.SessionLedgerPreparing && !moved {
			return false
		}
		if moved {
			x.BaseCommit, x.ViewCommit = head, head
		}
		x.State, x.ErrorCode, x.ErrorDetail = store.SessionLedgerPreparing, 0, ""
		return true
	})
	if err != nil || !l.State.Unprepared() {
		m.logError("start preparation", sid, err)
		return
	}
	p, o, failed, err := m.send(ctx, l, l.BaseCommit, false)
	switch o {
	case offline:
		m.wait(ctx, sid)
		return
	case unusable:
		m.failUnusable(ctx, sid, err)
		return
	case transient:
		m.later(ctx, sid, err)
		return
	}
	if failed != nil {
		code := failed.GetError()
		switch {
		case code == circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_BUSY:
			m.later(ctx, sid, errors.New("ledgersnaps: snapshot busy"))
		case code > 0 && code < 100 && circusv1.LedgerSnapshotError_name[int32(code)] != "":
			m.fail(ctx, sid, code, failed.GetErrorDetail())
		default:
			m.later(ctx, sid, errors.New("ledgersnaps: invalid prepare error"))
		}
		return
	}
	_, ok, err := m.update(ctx, sid, eventPrepared, func(x *store.SessionLedger) bool {
		if !x.State.Unprepared() || x.BaseCommit != l.BaseCommit {
			return false
		}
		x.State, x.Path, x.ErrorCode, x.ErrorDetail, x.Attempts, x.RetryAt = store.SessionLedgerReady, p, 0, "", 0, 0
		return true
	})
	m.logError("finish preparation", sid, err)
	if ok && m.cfg.Waker != nil {
		m.cfg.Waker.Wake(sid, circusv1.WorkerEvent_Nudge_REASON_INPUT)
	}
}

// reset writes the snapshot again from its view after its changeset was
// discarded, and tells the agent in the same change.
func (m *Manager) reset(ctx context.Context, l store.SessionLedger) {
	sid := l.SessionID
	_, o, failed, err := m.send(ctx, l, l.ViewCommit, true)
	switch {
	case o == offline:
		return
	case o == unusable:
		m.failUnusable(ctx, sid, err)
		return
	case o != gotResult:
		m.later(ctx, sid, err)
		return
	case failed != nil && transientError(failed.GetError()):
		m.later(ctx, sid, fmt.Errorf("ledgersnaps: reset failed: %s", failed.GetError()))
		return
	case failed != nil:
		m.failedStep(ctx, l, fmt.Sprintf("the snapshot could not be written again after the discard of changeset %s: the device answered %s. "+
			"Until it is, the snapshot still holds the discarded changes, and what you change in it is discarded with them when it is written again. "+
			"Fix what stops it (the snapshot directory's permissions, a full disk, a file in the way)",
			l.PendingChangeset, deviceAnswer(failed.GetError(), failed.GetErrorDetail())))
		return
	}
	_, ok, err := m.change(ctx, sid, eventCollected, func(_ context.Context, _ store.Ops, x *store.SessionLedger) (bool, *note, error) {
		if x.Pending != store.SessionLedgerReset || x.PendingChangeset != l.PendingChangeset {
			return false, nil, nil
		}
		n := &note{category: noteDiscarded, text: discardedText(x.Path, x.PendingChangeset, x.PendingReason), deferred: true}
		x.BaseCommit, x.Known, x.Problems, x.Warnings, x.ChangeCount, x.ProblemCount = x.ViewCommit, nil, nil, nil, 0, 0
		x.Pending, x.PendingChangeset, x.PendingReason, x.RebaseTarget, x.NotedHash, x.Attempts, x.RetryAt = "", "", "", "", "", 0, 0
		return true, n, nil
	})
	if err != nil || !ok {
		m.logError("finish reset", sid, err)
		return
	}
	m.wake(sid)
}

// wake tells the session that its turns are no longer held, with its
// pending inputs.
func (m *Manager) wake(sessionID string) {
	if m.cfg.Waker != nil {
		m.cfg.Waker.Wake(sessionID, circusv1.WorkerEvent_Nudge_REASON_INPUT)
	}
}

// transientError reports a device's error that a retry alone may fix.
func transientError(code circusv1.LedgerSnapshotError) bool {
	return code == circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_BUSY || code == circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_TIMEOUT
}

// deviceAnswer names a device's error and its detail for a problem.
func deviceAnswer(code circusv1.LedgerSnapshotError, detail string) string {
	out := strings.TrimPrefix(code.String(), "LEDGER_SNAPSHOT_ERROR_")
	if code == 0 {
		out = "an invalid result"
	}
	if detail != "" {
		out += " (" + clipText(detail, 512) + ")"
	}
	return out
}

// failedStep handles a rebase or reset attempt that failed for a reason a
// retry alone does not fix: it is tried again after its backoff, and the
// failure is recorded as message, a problem of the snapshot, once
// maxDeviceFailures attempts in a row failed (as pendingProblems).
func (m *Manager) failedStep(ctx context.Context, l store.SessionLedger, message string) {
	m.logError("snapshot step", l.SessionID, errors.New("ledgersnaps: the device refused a rebase or reset"))
	m.stepProblems(ctx, l, []store.LedgerProblem{{Message: message}}, maxDeviceFailures)
}

// knownChanges converts known changes for a device call.
func knownChanges(known []store.LedgerKnownChange) []*circusv1.CollectLedgerRequest_KnownChange {
	out := make([]*circusv1.CollectLedgerRequest_KnownChange, 0, len(known))
	for _, k := range known {
		c := &circusv1.CollectLedgerRequest_KnownChange{Path: k.Path}
		if k.Deleted {
			c.Change = &circusv1.CollectLedgerRequest_KnownChange_Deleted{Deleted: true}
		} else {
			c.Change = &circusv1.CollectLedgerRequest_KnownChange_Sha256{Sha256: k.SHA256}
		}
		out = append(out, c)
	}
	return out
}

// collection is one collect_ledger result as the control plane uses it.
type collection struct {
	changes  []ledger.SnapshotChange
	problems []store.LedgerProblem
	// tooLarge: nothing of the collection can be used.
	tooLarge bool
	// base is the base commit the snapshot's manifest records, which the
	// changes are relative to (empty for an empty ledger).
	base string
}

var errCollectFailed = errors.New("ledgersnaps: the device could not collect the snapshot")

// collectDevice runs collect_ledger with known. It returns the device's
// result only when the collection failed: the contents are copied out,
// so the decoded result is not kept while the collection is checked.
func (m *Manager) collectDevice(ctx context.Context, l store.SessionLedger, known []store.LedgerKnownChange) (collection, *circusv1.CollectLedgerResult, outcome, error) {
	res, o, err := m.call(ctx, l.UserID, l.DeviceID, l.SessionID, collectTimeout, &circusv1.DeviceCall{
		Tool: &circusv1.DeviceCall_CollectLedger{CollectLedger: &circusv1.CollectLedgerRequest{SnapshotId: l.SessionID, Known: knownChanges(known)}}})
	switch o {
	case offline:
		return collection{}, nil, o, errOffline
	case unusable:
		return collection{}, nil, o, err
	case transient:
		return collection{}, nil, o, cmpErr(err, errCollectFailed)
	}
	got := res.GetCollectLedger()
	if got.GetOutcome() != circusv1.CollectLedgerResult_OUTCOME_COLLECTED {
		return collection{}, got, transient, errCollectFailed
	}
	out := collection{base: ledgerCommit(got.GetCurrentBaseCommit())}
	if !store.IsLedgerCommitID(out.base) && out.base != "" {
		return collection{}, nil, transient, errors.New("ledgersnaps: the collection names no base commit")
	}
	for _, p := range got.GetProblems() {
		out.problems = append(out.problems, store.LedgerProblem{Path: p.GetPath(), Message: clipText(p.GetMessage(), store.MaxSessionLedgerMessageBytes)})
	}
	if got.GetTooLarge() {
		out.tooLarge = true
		out.problems = append(out.problems, store.LedgerProblem{Message: fmt.Sprintf(
			"the changed files exceed %d MiB, more than a changeset holds; this collection was not used", ledger.MaxChangesetBytes>>20)})
		return out, nil, gotResult, nil
	}
	for _, c := range got.GetChanges() {
		switch v := c.GetChange().(type) {
		case *circusv1.CollectLedgerResult_Change_Content:
			out.changes = append(out.changes, ledger.SnapshotChange{Path: c.GetPath(), Content: []byte(v.Content)})
		case *circusv1.CollectLedgerResult_Change_Deleted:
			out.changes = append(out.changes, ledger.SnapshotChange{Path: c.GetPath(), Deleted: true})
		case *circusv1.CollectLedgerResult_Change_Known:
			out.changes = append(out.changes, ledger.SnapshotChange{Path: c.GetPath(), Known: true, SHA256: c.GetSha256()})
		default:
			return collection{}, got, transient, errors.New("ledgersnaps: a collected change has no content")
		}
	}
	return out, nil, gotResult, nil
}

// collectFailed handles a collection that ended without a usable result:
// a missing snapshot or a device that cannot take calls fails the
// snapshot; anything else but an offline device is tried again later.
func (m *Manager) collectFailed(ctx context.Context, sessionID string, got *circusv1.CollectLedgerResult, o outcome, err error) {
	switch {
	case o == offline:
	case o == unusable:
		m.failUnusable(ctx, sessionID, err)
	case got.GetError() == circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_ABSENT:
		m.fail(ctx, sessionID, got.GetError(), "the snapshot is missing on the device: "+got.GetErrorDetail())
	case got != nil:
		m.later(ctx, sessionID, fmt.Errorf("%w: %s", err, got.GetError()))
	default:
		m.later(ctx, sessionID, err)
	}
}

// adoptBase takes base, the base commit a collection found in the
// snapshot's manifest instead of the row's, as the snapshot's base: the
// collection is not mixed with the row's base and known changes, but the
// snapshot is collected again relative to base. A base the ledger does not
// hold cannot be interpreted and fails the snapshot. It reports whether
// the caller collects again.
func (m *Manager) adoptBase(ctx context.Context, l store.SessionLedger, base string) bool {
	sid := l.SessionID
	if base != "" {
		has, err := m.cfg.Ledger.HasCommit(ctx, l.ProjectID, base)
		if err != nil {
			m.later(ctx, sid, err)
			return false
		}
		if !has {
			m.fail(ctx, sid, circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_UNSPECIFIED,
				"the snapshot's manifest records base commit "+base+", which the project's ledger does not hold")
			return false
		}
	}
	m.cfg.Logger.Warn("ledger snapshot found on another base", "session_id", sid)
	_, ok, err := m.update(ctx, sid, eventCollected, func(x *store.SessionLedger) bool {
		if x.State != store.SessionLedgerReady || x.BaseCommit != l.BaseCommit || x.Pending != l.Pending {
			return false
		}
		x.BaseCommit, x.Known = base, nil
		return true
	})
	if err != nil {
		m.later(ctx, sid, err)
	}
	return ok
}

func cmpErr(err, otherwise error) error {
	if err != nil {
		return err
	}
	return otherwise
}

// asProblem turns an engine error about the collection's content into a
// problem of the collection.
func asProblem(err error) (store.LedgerProblem, bool) {
	var limit *ledger.LimitError
	var request *ledger.RequestError
	switch {
	case errors.As(err, &limit):
		return store.LedgerProblem{Message: limit.Message}, true
	case errors.As(err, &request):
		return store.LedgerProblem{Message: request.Field + ": " + request.Message}, true
	case errors.Is(err, store.ErrLimit):
		return store.LedgerProblem{Message: fmt.Sprintf(
			"the project has %d open changesets, the most it may have; the changes are kept on the device until one of them is merged or discarded",
			store.MaxOpenLedgerChangesets)}, true
	}
	return store.LedgerProblem{}, false
}

// withSlot runs fn, ledger work that holds memory like a check, through a
// write slot of userID.
func (m *Manager) withSlot(ctx context.Context, userID string, fn func() error) error {
	if err := m.cfg.Slots.Take(ctx, userID); err != nil {
		return err
	}
	defer m.cfg.Slots.Release(userID)
	return fn()
}

// checkSession checks the session's changeset with a collection through a
// write slot, waiting for one.
func (m *Manager) checkSession(ctx context.Context, l store.SessionLedger, changes []ledger.SnapshotChange) (ledger.SessionCheckResult, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return ledger.SessionCheckResult{}, err
	}
	var res ledger.SessionCheckResult
	err = m.withSlot(ctx, l.UserID, func() (err error) {
		res, err = m.cfg.Ledger.CheckSessionChangeset(ctx, ledger.SessionCheck{ProjectID: l.ProjectID, SessionID: l.SessionID,
			NewChangesetID: id.String(), ViewBase: l.ViewCommit, SnapshotBase: l.BaseCommit, Changes: changes})
		return err
	})
	return res, err
}

// collected is a collection's outcome for its callers.
type collected struct {
	Ledger store.SessionLedger
	// Check is the check the collection made; nil when it could not be
	// used.
	Check *ledger.SessionCheckResult
}

// collect collects the snapshot, checks the session's changeset with it,
// records the outcome and tells the agent of new problems. errOffline
// means the device could not be asked; the row is unchanged.
func (m *Manager) collect(ctx context.Context, l store.SessionLedger) (collected, error) {
	sid := l.SessionID
	startedAt := m.now()
	known := l.Known
	var c collection
	var check *ledger.SessionCheckResult
	for attempt := 0; ; attempt++ {
		var got *circusv1.CollectLedgerResult
		var o outcome
		var err error
		c, got, o, err = m.collectDevice(ctx, l, known)
		if err != nil {
			m.collectFailed(ctx, sid, got, o, err)
			return collected{Ledger: l}, err
		}
		if c.base != l.BaseCommit {
			// Not relative to the base the row knows: collected again
			// relative to the manifest's.
			if attempt == maxRebaseAttempts || !m.adoptBase(ctx, l, c.base) {
				return collected{Ledger: l}, errCollectFailed
			}
			if l, err = m.cfg.Store.GetSessionLedger(ctx, sid); err != nil {
				return collected{Ledger: l}, err
			}
			known = nil
			continue
		}
		if c.tooLarge {
			break
		}
		res, err := m.checkSession(ctx, l, c.changes)
		if errors.Is(err, ledger.ErrKnownChanged) && known != nil {
			known = nil
			continue
		}
		if p, ok := asProblem(err); ok {
			c.problems = append(c.problems, p)
			break
		}
		if err != nil {
			m.later(ctx, sid, err)
			return collected{Ledger: l}, err
		}
		check = &res
		break
	}
	var all []store.LedgerProblem
	if check != nil {
		for _, p := range check.Problems {
			all = append(all, store.LedgerProblem{Path: p.Path, Message: p.Message})
		}
	}
	all = append(all, c.problems...)
	slices.SortStableFunc(all, func(a, b store.LedgerProblem) int { return strings.Compare(a.Path, b.Path) })
	l, ok, err := m.change(ctx, sid, eventCollected, func(ctx context.Context, tx store.Ops, x *store.SessionLedger) (bool, *note, error) {
		if x.State != store.SessionLedgerReady || x.Pending != "" || x.BaseCommit != l.BaseCommit {
			return false, nil, nil
		}
		x.CollectedAt = m.now()
		if x.DueAt <= startedAt {
			x.DueAt = 0
		}
		x.Attempts, x.RetryAt = 0, 0
		if check != nil {
			x.Known = check.Known
			x.ChangeCount = uint32(len(check.Entries))
			x.Warnings = boundProblems(problemsOf(check.Warnings))
		}
		x.Problems, x.ProblemCount = boundProblems(all), uint32(len(all))
		n, err := problemsNote(ctx, tx, x, all)
		return err == nil, n, err
	})
	if err != nil {
		return collected{Ledger: l}, err
	}
	if !ok {
		return collected{Ledger: l}, nil
	}
	return collected{Ledger: l, Check: check}, nil
}

func problemsOf(list []ledger.Problem) []store.LedgerProblem {
	out := make([]store.LedgerProblem, 0, len(list))
	for _, p := range list {
		out = append(out, store.LedgerProblem{Path: p.Path, Message: p.Message})
	}
	return out
}

// boundProblems keeps what the store holds of problems: at most
// MaxSessionLedgerProblems, with bounded paths and messages.
func boundProblems(list []store.LedgerProblem) []store.LedgerProblem {
	out := make([]store.LedgerProblem, 0, min(len(list), store.MaxSessionLedgerProblems))
	for _, p := range list {
		if len(out) == store.MaxSessionLedgerProblems {
			break
		}
		out = append(out, store.LedgerProblem{Path: clipText(p.Path, store.MaxSessionLedgerPathBytes),
			Message: clipText(p.Message, store.MaxSessionLedgerMessageBytes)})
	}
	return out
}

// problemHash identifies a set of problems.
func problemHash(list []store.LedgerProblem) string {
	if len(list) == 0 {
		return ""
	}
	h := sha256.New()
	for _, p := range list {
		fmt.Fprintf(h, "%d:%s%d:%s", len(p.Path), p.Path, len(p.Message), p.Message)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// rebaseAnswer is how a rebase call ended, for rebase.
type rebaseAnswer int

const (
	// rebasedTo: the manifest records the request's base commit, and the
	// snapshot holds exactly its files.
	rebasedTo rebaseAnswer = iota
	// alreadyOn: a STALE answer that finds the snapshot on the request's
	// base commit already (an earlier attempt whose answer was lost): it
	// changed since, which the next collection takes.
	alreadyOn
	// stale: the snapshot is not what the request expected: collect
	// again (edits before the path that differed may be applied).
	stale
	// stopped: the step ends here; the call was handled (offline, a
	// backoff, a failed snapshot).
	stopped
)

// sendRebase sends the rebase plan describes. Its base commit names the
// exact files the request installs, so a manifest found on it later means
// those files. Before sending, the target is recorded on the row: a lost
// answer leaves the next attempt to find the snapshot on it. The plan's
// edits and known changes are dropped once copied into the request.
func (m *Manager) sendRebase(ctx context.Context, l store.SessionLedger, plan *ledger.RebasePlan) rebaseAnswer {
	sid := l.SessionID
	if _, ok, err := m.update(ctx, sid, eventCollected, func(x *store.SessionLedger) bool {
		if x.Pending != store.SessionLedgerRebase || x.PendingChangeset != l.PendingChangeset || x.BaseCommit != l.BaseCommit {
			return false
		}
		x.RebaseTarget = plan.Target
		return true
	}); err != nil || !ok {
		m.logError("record rebase target", sid, err)
		return stopped
	}
	req := &circusv1.RebaseLedgerRequest{SnapshotId: sid, BaseCommit: deviceCommit(plan.Target), Known: knownChanges(plan.Known)}
	for _, e := range plan.Edits {
		edit := &circusv1.RebaseLedgerRequest_Edit{Path: e.Path, ExpectedSha256: e.ExpectedSHA256}
		if e.Deleted {
			edit.Change = &circusv1.RebaseLedgerRequest_Edit_Deleted{Deleted: true}
		} else {
			edit.Change = &circusv1.RebaseLedgerRequest_Edit_Content{Content: string(e.Content)}
		}
		req.Edits = append(req.Edits, edit)
	}
	plan.Edits, plan.Known = nil, nil
	res, o, err := m.call(ctx, l.UserID, l.DeviceID, sid, rebaseTimeout, &circusv1.DeviceCall{Tool: &circusv1.DeviceCall_RebaseLedger{RebaseLedger: req}})
	switch o {
	case offline:
		return stopped
	case unusable:
		m.failUnusable(ctx, sid, err)
		return stopped
	case transient:
		m.later(ctx, sid, err)
		return stopped
	}
	got := res.GetRebaseLedger()
	on := got.GetCurrentBaseCommit() != "" && ledgerCommit(got.GetCurrentBaseCommit()) == plan.Target
	switch {
	case on && got.GetOutcome() == circusv1.RebaseLedgerResult_OUTCOME_REBASED:
		return rebasedTo
	case on:
		return alreadyOn
	case got.GetError() == circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_STALE:
		return stale
	case got.GetError() == circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_ABSENT:
		m.fail(ctx, sid, got.GetError(), "the snapshot is missing on the device: "+got.GetErrorDetail())
		return stopped
	case transientError(got.GetError()):
		m.later(ctx, sid, fmt.Errorf("ledgersnaps: rebase failed: %s", got.GetError()))
		return stopped
	}
	m.failedStep(ctx, l, rebaseFailure(l.PendingChangeset, "the device answered "+deviceAnswer(got.GetError(), got.GetErrorDetail())))
	return stopped
}

// rebaseFailure is the problem of a rebase that keeps failing for why.
func rebaseFailure(changeset, why string) string {
	return fmt.Sprintf("the snapshot could not be updated with the merge of changeset %s: %s. "+
		"Your turns are no longer held for it; it is tried again after your next turn. "+
		"Fix what stops it (the snapshot directory's permissions, a full disk, a file in the way, a process still writing to it)", changeset, why)
}

// rebase applies the merge of the session's changeset to its snapshot,
// between turns (the session's turns wait for it). It collects what the
// snapshot holds now and installs the plan PlanSessionRebase makes of it:
// the merged files, with the agent's changes since the collection the
// merge used renumbered as the merge did. A collection on the target of an
// earlier attempt finds that attempt finished, its answer lost; one on a
// base the row does not know adopts it and collects again.
func (m *Manager) rebase(ctx context.Context, l store.SessionLedger) {
	sid, changeset := l.SessionID, l.PendingChangeset
	startedAt := m.now()
	known := l.Known
	for range maxRebaseAttempts {
		c, got, o, err := m.collectDevice(ctx, l, known)
		if err != nil {
			if code := got.GetError(); got != nil && code != circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_ABSENT && !transientError(code) {
				m.failedStep(ctx, l, rebaseFailure(changeset, "the device could not collect it: "+deviceAnswer(code, got.GetErrorDetail())))
				return
			}
			m.collectFailed(ctx, sid, got, o, err)
			return
		}
		switch {
		case l.RebaseTarget != "" && c.base == l.RebaseTarget && c.base != l.BaseCommit:
			var view string
			err := m.withSlot(ctx, l.UserID, func() (err error) {
				view, err = m.cfg.Ledger.SessionView(ctx, changeset)
				return err
			})
			if err != nil {
				m.later(ctx, sid, err)
				return
			}
			// What the target renamed is not known any more, and the agent
			// may have changed the snapshot since: collected next.
			m.rebased(ctx, l, ledger.RebasePlan{View: view, Target: l.RebaseTarget}, true, true, startedAt)
			return
		case c.base != l.BaseCommit:
			if !m.adoptBase(ctx, l, c.base) {
				return
			}
			if l, err = m.cfg.Store.GetSessionLedger(ctx, sid); err != nil {
				m.later(ctx, sid, err)
				return
			}
			known = nil
			continue
		case c.tooLarge || len(c.problems) > 0:
			// Nothing can be rebased while the snapshot has files no
			// collection takes; the agent hears of them.
			m.pendingProblems(ctx, l, c.problems)
			return
		}
		var plan ledger.RebasePlan
		err = m.withSlot(ctx, l.UserID, func() (err error) {
			plan, err = m.cfg.Ledger.PlanSessionRebase(ctx, ledger.RebaseInput{ChangesetID: changeset, SnapshotBase: l.BaseCommit, Changes: c.changes})
			return err
		})
		if errors.Is(err, ledger.ErrKnownChanged) && known != nil {
			known = nil
			continue
		}
		if p, ok := asProblem(err); ok {
			m.pendingProblems(ctx, l, []store.LedgerProblem{p})
			return
		}
		if err != nil {
			m.later(ctx, sid, err)
			return
		}
		if err := checkEdits(plan.Edits); err != nil {
			// A snapshot this far from the merge cannot be rebased by
			// edits.
			m.fail(ctx, sid, circusv1.LedgerSnapshotError_LEDGER_SNAPSHOT_ERROR_INVALID_REQUEST, err.Error())
			return
		}
		// Only the request holds contents from here on.
		clear(c.changes)
		switch m.sendRebase(ctx, l, &plan) {
		case rebasedTo:
			m.rebased(ctx, l, plan, false, plan.Target != plan.View, startedAt)
			return
		case alreadyOn:
			m.rebased(ctx, l, plan, false, true, startedAt)
			return
		case stopped:
			return
		}
		// Changed outside a turn (by hand on the device): planned again
		// from what it holds now.
		if l, err = m.cfg.Store.GetSessionLedger(ctx, sid); err != nil || l.Pending != store.SessionLedgerRebase || l.PendingChangeset != changeset {
			m.logError("read snapshot", sid, err)
			return
		}
	}
	m.failedStep(ctx, l, rebaseFailure(changeset, fmt.Sprintf("it kept changing while it was updated (%d attempts)", maxRebaseAttempts)))
}

// pendingProblems records the problems that stop a rebase or a reset and
// tells the agent; the step is tried again after the agent's next turn,
// or its backoff. The snapshot holds no turns meanwhile.
func (m *Manager) pendingProblems(ctx context.Context, l store.SessionLedger, problems []store.LedgerProblem) {
	m.stepProblems(ctx, l, problems, 1)
}

// stepProblems counts a failed attempt of the pending step of l and
// schedules the next after its backoff; from the after-th failed attempt
// in a row on, it records problems as well (pendingProblems).
func (m *Manager) stepProblems(ctx context.Context, l store.SessionLedger, problems []store.LedgerProblem, after int64) {
	recorded := false
	l, ok, err := m.change(ctx, l.SessionID, eventCollected, func(ctx context.Context, tx store.Ops, x *store.SessionLedger) (bool, *note, error) {
		if x.Pending == "" || x.Pending != l.Pending || x.PendingChangeset != l.PendingChangeset {
			return false, nil, nil
		}
		x.Attempts++
		x.RetryAt = m.now() + backoff(x.Attempts-1).Milliseconds()
		if recorded = x.Attempts >= after; !recorded {
			return true, nil, nil
		}
		x.Problems, x.ProblemCount = boundProblems(problems), uint32(len(problems))
		n, err := problemsNote(ctx, tx, x, problems)
		return err == nil, n, err
	})
	if err != nil || !ok {
		m.logError("record snapshot problems", l.SessionID, err)
		return
	}
	m.retryAt(l.SessionID, l.RetryAt)
	if recorded {
		// The session's turns are no longer held.
		m.wake(l.SessionID)
	}
}

// rebased records a finished rebase onto plan.Target and tells the agent
// in the same change. With changed (a target with the agent's later
// changes, not the view itself, or a snapshot that changed after it was
// installed) a collection is due, which takes those changes into the next
// changeset. found: an earlier attempt installed the target, and what it
// renamed is not known. startedAt is when the rebase's collection
// started.
func (m *Manager) rebased(ctx context.Context, l store.SessionLedger, plan ledger.RebasePlan, found, changed bool, startedAt int64) {
	sid, changeset := l.SessionID, l.PendingChangeset
	numbers := plan.Numbers
	if found {
		if c, err := m.cfg.Store.GetLedgerChangeset(ctx, changeset); err == nil {
			numbers = c.Merge.Numbers
		}
	}
	l, ok, err := m.change(ctx, sid, eventCollected, func(_ context.Context, _ store.Ops, x *store.SessionLedger) (bool, *note, error) {
		if x.Pending != store.SessionLedgerRebase || x.PendingChangeset != changeset {
			return false, nil, nil
		}
		n := &note{category: noteMerged, text: mergedText(x.Path, changeset, mergeNote{numbers: numbers, renamed: plan.Renamed,
			restored: plan.Restored, problems: plan.Problems, found: found, kept: plan.Target != plan.View}), deferred: true}
		x.BaseCommit, x.ViewCommit, x.Known, x.Problems, x.Warnings, x.ChangeCount, x.ProblemCount = plan.Target, plan.View, nil, nil, nil, 0, 0
		x.Pending, x.PendingChangeset, x.PendingReason, x.RebaseTarget, x.NotedHash, x.Attempts, x.RetryAt = "", "", "", "", "", 0, 0
		switch {
		case changed:
			x.DueAt = max(x.DueAt, m.now())
		case x.DueAt <= startedAt:
			// The rebase's collection saw every turn's end so far.
			x.DueAt = 0
		}
		return true, n, nil
	})
	if err != nil || !ok {
		m.logError("finish rebase", sid, err)
		return
	}
	m.wake(sid)
	if l.CollectionDue() {
		m.Kick(sid)
	}
}

// checkEdits refuses edits a rebase cannot carry.
func checkEdits(edits []ledger.SnapshotEdit) error {
	if len(edits) > chunkFiles {
		return fmt.Errorf("the merge changes %d files of the snapshot, more than one rebase applies", len(edits))
	}
	total := 0
	for _, e := range edits {
		if len(e.Content) > snapshot.MaxBytes(e.Path) {
			return fmt.Errorf("%s exceeds the %d bytes a snapshot file may hold after the merge", e.Path, snapshot.MaxBytes(e.Path))
		}
		if e.Path != snapshot.IndexPath {
			// The generated index has its own bound.
			total += len(e.Content)
		}
	}
	if total > maxEditBytes {
		return fmt.Errorf("the merge changes %d bytes of the snapshot, more than one rebase applies", total)
	}
	return nil
}

// removal is a queued removal of a deleted session's snapshot.
const removalKey = "rm:"

// removals queues the removals that can run now: of one device, or of all
// online devices.
func (m *Manager) removals(ctx context.Context, deviceID string) {
	for after, read := "", 0; read < scanLimit && ctx.Err() == nil; {
		rows, err := m.cfg.Store.ListLedgerSnapshotRemovals(ctx, deviceID, after, store.MaxPageSize)
		if err != nil {
			m.logError("list removals", "", err)
			return
		}
		for _, r := range rows {
			if m.cfg.Online(r.DeviceID) {
				m.mu.Lock()
				m.pendingRemovals[r.SessionID] = r
				m.mu.Unlock()
				m.enqueue(removalKey + r.SessionID)
			}
		}
		if len(rows) < store.MaxPageSize {
			return
		}
		after = rows[len(rows)-1].SessionID
		read += len(rows)
	}
}

// remove deletes a deleted session's snapshot from its device and forgets
// the removal once it is done.
func (m *Manager) remove(ctx context.Context, r store.LedgerSnapshotRemoval) {
	res, o, err := m.call(ctx, r.UserID, r.DeviceID, r.SessionID, removeTimeout, &circusv1.DeviceCall{
		Tool: &circusv1.DeviceCall_RemoveLedger{RemoveLedger: &circusv1.RemoveLedgerRequest{SnapshotId: r.SessionID}}})
	switch o {
	case unusable:
		// The device is gone: so is its snapshot.
		m.logError("remove snapshot", r.SessionID, m.cfg.Store.DeleteLedgerSnapshotRemoval(ctx, r.SessionID))
		return
	case gotResult:
	default:
		m.logError("remove snapshot", r.SessionID, err)
		return
	}
	switch res.GetRemoveLedger().GetOutcome() {
	case circusv1.RemoveLedgerResult_OUTCOME_REMOVED, circusv1.RemoveLedgerResult_OUTCOME_ABSENT:
		m.logError("forget removal", r.SessionID, m.cfg.Store.DeleteLedgerSnapshotRemoval(ctx, r.SessionID))
	default:
		// Kept for the next connection, until the removal expires.
		m.cfg.Logger.Warn("ledger snapshot removal failed", "session_id", r.SessionID,
			"error", res.GetRemoveLedger().GetError().String())
	}
}
