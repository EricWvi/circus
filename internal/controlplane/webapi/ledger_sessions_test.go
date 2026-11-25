package webapi_test

import (
	"strings"
	"testing"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/controlplane/ledgerslots"
	"github.com/EricWvi/circus/internal/controlplane/ledgersnaps"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
)

// withSnapshots serves session ledgers with every device offline: forced
// collections use the last one.
func withSnapshots(cfg *webapi.Config) {
	slots := ledgerslots.New(ledgerslots.Writes, ledgerslots.WritesPerUser, ledgerslots.MaxWaiters)
	cfg.LedgerWrites = slots
	cfg.Snapshots = ledgersnaps.New(ledgersnaps.Config{Store: cfg.Store, Ledger: cfg.Ledger, Slots: slots, Bus: cfg.Bus, Waker: cfg.Waker,
		Online: func(string) bool { return false }})
}

// readySession creates a session whose snapshot is ready, with a
// collection that opened its changeset with a new decision.
func readySession(t *testing.T, h *harness, super *client) (*circusv1.Session, store.SessionLedger) {
	t.Helper()
	ctx := t.Context()
	_, sess := sessionSetup(t, h, super)
	if sess.GetLedger().GetState() != circusv1.SessionLedgerStatus_STATE_PENDING {
		t.Fatalf("new session's ledger = %v", sess.GetLedger())
	}
	l, err := h.st.GetSessionLedger(ctx, sess.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	newDoc := []byte(decisionFile("D?a", "Backups", "Nightly.\n"))
	res, err := h.ledger.CheckSessionChangeset(ctx, ledger.SessionCheck{ProjectID: l.ProjectID, SessionID: l.SessionID, NewChangesetID: "scs-1",
		ViewBase: l.ViewCommit, SnapshotBase: l.BaseCommit, Changes: []ledger.SnapshotChange{{Path: "D?a-Backups.md", Content: newDoc}}})
	if err != nil {
		t.Fatal(err)
	}
	prev := l.UpdatedAt
	l.State, l.Path, l.Known, l.ChangeCount, l.CollectedAt, l.UpdatedAt = store.SessionLedgerReady, "/state/circus/ledger/"+l.SessionID,
		res.Known, uint32(len(res.Entries)), h.clock.Load(), prev+1
	if ok, err := h.st.UpdateSessionLedger(ctx, l, prev); err != nil || !ok {
		t.Fatalf("update: %v %v", ok, err)
	}
	l, err = h.st.GetSessionLedger(ctx, sess.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	return sess, l
}

func preconditionType(t *testing.T, err error) string {
	t.Helper()
	subjects := preconditionSubjects(t, err)
	if len(subjects) == 0 {
		t.Fatalf("no precondition failure in %v", err)
	}
	typ, _, _ := strings.Cut(subjects[0], ":")
	return typ
}

// TestSessionLedgerService reads a session's snapshot and refuses its
// merge for each precondition.
func TestSessionLedgerService(t *testing.T) {
	h, super := newHarness(t, withWorkers, withSnapshots)
	ctx := t.Context()
	sess, l := readySession(t, h, super)
	api := circusv1connect.NewLedgerServiceClient(super.http, h.server.URL)
	got, err := api.GetSessionLedger(ctx, connect.NewRequest(&circusv1.GetSessionLedgerRequest{SessionId: sess.SessionId}))
	if err != nil {
		t.Fatal(err)
	}
	m := got.Msg
	if m.GetStatus().GetState() != circusv1.SessionLedgerStatus_STATE_READY || m.GetStatus().GetChangesetId() != l.ChangesetID ||
		m.GetChangeset().GetSessionId() != sess.SessionId || !m.GetFastForward() || len(m.GetEntries()) != 1 ||
		m.GetEntries()[0].GetNumber() != "D?a" || !m.GetEntries()[0].GetAdded() {
		t.Fatalf("session ledger = %v", m)
	}
	session, err := super.sessions.GetSession(ctx, connect.NewRequest(&circusv1.GetSessionRequest{SessionId: sess.SessionId}))
	if err != nil {
		t.Fatal(err)
	}
	if session.Msg.GetLedger().GetChangesetId() != l.ChangesetID || session.Msg.GetLedger().GetUpdatedAtUnixMs() != l.UpdatedAt {
		t.Fatalf("Session.ledger = %v", session.Msg.GetLedger())
	}
	listed, err := super.sessions.ListSessions(ctx, connect.NewRequest(&circusv1.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Msg.GetSessions()) != 1 || listed.Msg.GetSessions()[0].GetLedger().GetChangeCount() != 1 {
		t.Fatalf("listed = %v", listed.Msg.GetSessions())
	}
	diff, err := api.GetSessionLedgerDiff(ctx, connect.NewRequest(&circusv1.GetSessionLedgerDiffRequest{SessionId: sess.SessionId, Path: "D?a-Backups.md"}))
	if err != nil {
		t.Fatal(err)
	}
	if diff.Msg.GetBaseExists() || !diff.Msg.GetChangesetExists() || !strings.Contains(diff.Msg.GetChangeset(), "Nightly.") {
		t.Fatalf("diff = %v", diff.Msg)
	}
	_, err = api.GetSessionLedgerDiff(ctx, connect.NewRequest(&circusv1.GetSessionLedgerDiffRequest{SessionId: sess.SessionId, Path: "D0-x.md"}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = api.GetSessionLedgerDiff(ctx, connect.NewRequest(&circusv1.GetSessionLedgerDiffRequest{SessionId: sess.SessionId}))
	wantCode(t, err, connect.CodeInvalidArgument)

	// Another user's session does not exist for the caller.
	other := h.signIn(t, "u2", "u2@example.com")
	otherAPI := circusv1connect.NewLedgerServiceClient(other.http, h.server.URL)
	_, err = otherAPI.GetSessionLedger(ctx, connect.NewRequest(&circusv1.GetSessionLedgerRequest{SessionId: sess.SessionId}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = otherAPI.MergeSessionLedger(ctx, connect.NewRequest(&circusv1.MergeSessionLedgerRequest{SessionId: sess.SessionId, ExpectedChangesetId: l.ChangesetID}))
	wantCode(t, err, connect.CodeNotFound)

	// The merge refuses another changeset, and a due collection with the
	// device offline.
	_, err = api.MergeSessionLedger(ctx, connect.NewRequest(&circusv1.MergeSessionLedgerRequest{SessionId: sess.SessionId, ExpectedChangesetId: "other"}))
	if got := preconditionType(t, err); got != ledgersnaps.RefuseChangesetChanged {
		t.Fatalf("merge of another changeset refused with %s", got)
	}
	prev := l.UpdatedAt
	l.DueAt, l.UpdatedAt = h.clock.Load(), prev+1
	if ok, err := h.st.UpdateSessionLedger(ctx, l, prev); err != nil || !ok {
		t.Fatalf("update: %v %v", ok, err)
	}
	_, err = api.MergeSessionLedger(ctx, connect.NewRequest(&circusv1.MergeSessionLedgerRequest{SessionId: sess.SessionId, ExpectedChangesetId: l.ChangesetID}))
	if got := preconditionType(t, err); got != ledgersnaps.RefuseDeviceOffline {
		t.Fatalf("merge with a due collection refused with %s", got)
	}
	// Deleting it is refused while it has changes.
	if _, err := super.sessions.StopSession(ctx, connect.NewRequest(&circusv1.StopSessionRequest{SessionId: sess.SessionId})); err != nil {
		t.Fatal(err)
	}
	_, err = super.sessions.DeleteSession(ctx, connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: sess.SessionId}))
	if got := preconditionType(t, err); got != ledgersnaps.RefuseLedgerChanges {
		t.Fatalf("delete refused with %s", got)
	}
	if _, err := super.sessions.DeleteSession(ctx, connect.NewRequest(&circusv1.DeleteSessionRequest{SessionId: sess.SessionId, DiscardLedgerChanges: true})); err != nil {
		t.Fatal(err)
	}
	c, err := h.st.GetLedgerChangeset(ctx, l.ChangesetID)
	if err != nil || c.State != store.LedgerChangesetDiscarded || c.DiscardReason != ledgersnaps.SessionDeleted {
		t.Fatalf("changeset after the delete = %+v, %v", c, err)
	}
	if r, err := h.st.GetLedgerSnapshotRemoval(ctx, sess.SessionId); err != nil || r.DeviceID != "d1" {
		t.Fatalf("removal = %+v, %v", r, err)
	}
}

// TestSessionChangesetService: a session's changeset is an integrator's,
// takes no changes, is checked with the session's last collection while
// the device is offline, and is listed by session.
func TestSessionChangesetService(t *testing.T) {
	h, super := newHarness(t, withWorkers, withSnapshots)
	ctx := t.Context()
	sess, l := readySession(t, h, super)
	agent := h.worker(t, h.owner.ID, "w1", false)
	integrator := h.worker(t, h.owner.ID, "w2", true)
	_, err := agent.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: l.ChangesetID}))
	wantIntegratorRequired(t, err)
	_, err = agent.DiscardChangeset(ctx, connect.NewRequest(&circusv1.DiscardChangesetRequest{ChangesetId: l.ChangesetID, Reason: "no"}))
	wantIntegratorRequired(t, err)
	_, err = integrator.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: l.ChangesetID,
		Changes: []*circusv1.FileChange{content("D?a-Backups.md", "x")}}))
	wantCode(t, err, connect.CodeInvalidArgument)
	checked, err := integrator.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: l.ChangesetID}))
	if err != nil {
		t.Fatal(err)
	}
	m := checked.Msg
	if !m.GetFastForward() || len(m.GetProblems()) != 0 || len(m.GetWarnings()) != 1 || !strings.Contains(m.GetWarnings()[0].GetMessage(), "offline") {
		t.Fatalf("check = %v", m)
	}
	for _, c := range []struct {
		session string
		want    int
	}{{sess.SessionId, 1}, {"other", 0}} {
		listed, err := agent.ListChangesets(ctx, connect.NewRequest(&circusv1.ListChangesetsRequest{Project: sess.ProjectId, SessionId: c.session}))
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Msg.GetChangesets()) != c.want {
			t.Fatalf("changesets of %s = %v", c.session, listed.Msg.GetChangesets())
		}
	}
	merged, err := integrator.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: l.ChangesetID}))
	if err != nil {
		t.Fatal(err)
	}
	if merged.Msg.GetNumbers()["D?a"] != 0 || merged.Msg.GetChangeset().GetSessionId() != sess.SessionId {
		t.Fatalf("merge = %v", merged.Msg)
	}
	after, err := h.st.GetSessionLedger(ctx, sess.SessionId)
	if err != nil {
		t.Fatal(err)
	}
	if after.Pending != store.SessionLedgerRebase || after.ChangesetID != "" {
		t.Fatalf("after the merge = %+v", after)
	}
}
