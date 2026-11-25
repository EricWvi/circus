//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/fakeworker"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

// ledgerSessionTimeout bounds each wait for the snapshot work (a
// collection, a rebase, a reset), which runs after a turn ends.
const ledgerSessionTimeout = 30 * time.Second

// sessionLedgerHarness drives one session's ledger snapshot through the
// browser API, the fake worker and the real daemon.
type sessionLedgerHarness struct {
	t        *testing.T
	h        *harness
	ctx      context.Context
	sessions circusv1connect.SessionServiceClient
	ledger   circusv1connect.LedgerServiceClient
	id       string
}

func (s *sessionLedgerHarness) waitFor(what string, p func() bool) {
	s.t.Helper()
	deadline := time.Now().Add(ledgerSessionTimeout)
	for !p() {
		if time.Now().After(deadline) {
			s.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (s *sessionLedgerHarness) session() *pb.Session {
	s.t.Helper()
	res, err := s.sessions.GetSession(s.ctx, connect.NewRequest(&pb.GetSessionRequest{SessionId: s.id}))
	must(s.t, err)
	return res.Msg
}

func (s *sessionLedgerHarness) get() *pb.GetSessionLedgerResponse {
	s.t.Helper()
	res, err := s.ledger.GetSessionLedger(s.ctx, connect.NewRequest(&pb.GetSessionLedgerRequest{SessionId: s.id}))
	must(s.t, err)
	return res.Msg
}

// waitLedger polls GetSessionLedger until p holds.
func (s *sessionLedgerHarness) waitLedger(what string, p func(*pb.GetSessionLedgerResponse) bool) *pb.GetSessionLedgerResponse {
	s.t.Helper()
	var got *pb.GetSessionLedgerResponse
	s.waitFor(what, func() bool {
		got = s.get()
		return p(got)
	})
	return got
}

// settled is a snapshot with no collection due and no work left from a
// merge or a discard, collected after the given time.
func settled(after int64) func(*pb.GetSessionLedgerResponse) bool {
	return func(r *pb.GetSessionLedgerResponse) bool {
		st := r.GetStatus()
		return st.GetState() == pb.SessionLedgerStatus_STATE_READY && !st.GetCollectionDue() && st.GetCollectedAtUnixMs() > after
	}
}

func (s *sessionLedgerHarness) send(text string) {
	s.t.Helper()
	_, err := s.sessions.SendMessage(s.ctx, connect.NewRequest(&pb.SendMessageRequest{SessionId: s.id, ClientMsgId: uuid.NewString(), Text: text}))
	must(s.t, err)
}

// turn sends text and waits until a turn after it ended and the session is
// idle.
func (s *sessionLedgerHarness) turn(text string) {
	s.t.Helper()
	before := s.session().GetTurnCount()
	s.send(text)
	s.waitFor("the turn of "+text, func() bool {
		sess := s.session()
		return sess.GetTurnCount() > before && sess.GetState() == pb.Session_STATE_IDLE
	})
}

// edit runs a turn whose agent changes the snapshot with the fake worker's
// ledger operations, and waits for its collection.
func (s *sessionLedgerHarness) edit(ops ...map[string]string) *pb.GetSessionLedgerResponse {
	s.t.Helper()
	b, err := json.Marshal(ops)
	must(s.t, err)
	start := time.Now().UnixMilli()
	s.turn("/fake ledger " + string(b))
	return s.waitLedger("the collection after the edit", settled(start))
}

// events reads the session's stored view events.
func (s *sessionLedgerHarness) events() []store.SessionEvent {
	s.t.Helper()
	var out []store.SessionEvent
	for after := int64(0); ; {
		page, err := s.h.st.ReadEvents(s.ctx, store.EventFilter{SessionID: s.id, AfterSeq: after, Limit: store.MaxPageSize, MaxBytes: 8 << 20})
		must(s.t, err)
		out = append(out, page.Events...)
		if len(page.Events) < store.MaxPageSize {
			return out
		}
		after = page.Events[len(page.Events)-1].Seq
	}
}

// notes returns the notes of a category the worker published.
func (s *sessionLedgerHarness) notes(category string) []store.SessionEvent {
	s.t.Helper()
	var out []store.SessionEvent
	for _, e := range s.events() {
		if e.Kind != "USER" {
			continue
		}
		var p struct {
			Note        string `json:"circus_note"`
			IsSynthetic bool   `json:"isSynthetic"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.Note == category {
			if !p.IsSynthetic {
				s.t.Fatalf("note %s is not synthetic: %s", category, e.Payload)
			}
			out = append(out, e)
		}
	}
	return out
}

// noteTurn waits until the count-th note of category was published, the
// turn it started ended and the session is idle; it returns the note.
func (s *sessionLedgerHarness) noteTurn(category string, count int) store.SessionEvent {
	s.t.Helper()
	var note store.SessionEvent
	s.waitFor("note "+category, func() bool {
		notes := s.notes(category)
		if len(notes) < count {
			return false
		}
		note = notes[count-1]
		ended := false
		for _, e := range s.events() {
			if e.Seq > note.Seq && e.Kind == "RESULT" {
				ended = true
			}
		}
		return ended && s.session().GetState() == pb.Session_STATE_IDLE
	})
	return note
}

func (s *sessionLedgerHarness) path() string { return s.get().GetStatus().GetPath() }

func (s *sessionLedgerHarness) file(name string) (string, bool) {
	s.t.Helper()
	data, err := os.ReadFile(filepath.Join(s.path(), filepath.FromSlash(name)))
	if errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	must(s.t, err)
	return string(data), true
}

func entryPaths(r *pb.GetSessionLedgerResponse) []string {
	var out []string
	for _, e := range r.GetEntries() {
		out = append(out, e.GetPath())
	}
	return out
}

// precondition returns the type of a FAILED_PRECONDITION's violation.
func preconditionType(t *testing.T, err error) string {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeFailedPrecondition {
		t.Fatalf("want FAILED_PRECONDITION, got %v", err)
	}
	for _, d := range ce.Details() {
		if v, e := d.Value(); e == nil {
			if f, ok := v.(*errdetails.PreconditionFailure); ok && len(f.GetViolations()) > 0 {
				return f.GetViolations()[0].GetType()
			}
		}
	}
	t.Fatalf("no precondition failure in %v", err)
	return ""
}

func (h *harness) stopDaemon() {
	h.t.Helper()
	h.daemon.signal(h.t, syscall.SIGTERM)
	_ = h.daemon.wait(h.t, 10*time.Second)
	h.offline()
}

func (h *harness) restartDaemon() {
	h.t.Helper()
	h.startDaemon()
	h.readyDaemon()
}

func write(path, content string) map[string]string {
	return map[string]string{"op": "write", "path": path, "content": content}
}

func edit(path, old, new string) map[string]string {
	return map[string]string{"op": "edit", "path": path, "old": old, "new": new}
}

// TestSessionLedgerSnapshots drives a session's ledger snapshot end to end
// with the real daemon and the fake worker: preparation, collections into
// the session's changeset, notes, merges from the session page that rename
// new decisions on the device, an integrator's merge and discard, a device
// offline at a turn's end and during a rebase, and deletions.
func TestSessionLedgerSnapshots(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	jar, err := cookiejar.New(nil)
	must(t, err)
	browser := &http.Client{Jar: jar, Transport: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(browser.CloseIdleConnections)
	_, err = circusv1connect.NewAuthServiceClient(browser, h.base).Login(ctx, connect.NewRequest(&pb.LoginRequest{
		Email: enrollmentEmail, Password: enrollmentPassword,
	}))
	must(t, err)
	projects := circusv1connect.NewProjectServiceClient(browser, h.base)
	project, err := projects.CreateProject(ctx, connect.NewRequest(&pb.CreateProjectRequest{
		Name: "Plans", Locations: []*pb.Project_Location{{Device: "desk", Path: h.dir}},
	}))
	must(t, err)
	projectID := project.Msg.GetProject().GetProjectId()

	// An integrator, on its own credential.
	integratorConfig := filepath.Join(h.dir, "integrator-config")
	integratorEnv := []string{"XDG_CONFIG_HOME=" + integratorConfig}
	link := h.run("", 0, "server", "enroll-token", "create", "--user", enrollmentEmail, "--config", filepath.Join(filepath.Dir(h.db), "server.yaml"), "--role", "worker", "--url", h.base)
	p := h.start(integratorEnv, link, "join", "--role", "worker", "--name", "integrator")
	if code := h.waitCode(p, 10*time.Second); code != 0 {
		t.Fatalf("join: exit %d\n%s", code, p.stderr.String())
	}
	integrator, err := credfile.Load(filepath.Join(integratorConfig, "circus", "worker.json"))
	must(t, err)
	_, err = circusv1connect.NewMachineServiceClient(browser, h.base).SetWorkerGrant(ctx, connect.NewRequest(&pb.SetWorkerGrantRequest{
		WorkerId: integrator.PrincipalID, Integrator: true}))
	must(t, err)

	// The main line holds D0.
	seed, seedDir := h.ledgerOpen(nil, "Plans")
	writeLedgerFile(t, filepath.Join(seedDir, "D?a-Storage.md"), ledgerDecision("D?a", "Storage", "Keep data in SQLite.\n"))
	h.ledgerCLI(nil, 0, "merge", seed)

	workerCtx, stopWorker := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- fakeworker.Run(workerCtx, fakeworker.Options{Credential: h.worker, MaxSessions: 1, StreamDelay: 5 * time.Millisecond,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	}()
	t.Cleanup(func() {
		stopWorker()
		if err := testwait.Recv(t, done); !errors.Is(err, context.Canceled) {
			t.Errorf("fake worker exited: %v", err)
		}
	})
	sessions := circusv1connect.NewSessionServiceClient(browser, h.base)
	ledgerAPI := circusv1connect.NewLedgerServiceClient(browser, h.base)
	create := func() *sessionLedgerHarness {
		res, err := sessions.CreateSession(ctx, connect.NewRequest(&pb.CreateSessionRequest{ClientRequestId: uuid.NewString(), ProjectId: projectID}))
		must(t, err)
		if res.Msg.GetSession().GetLedger() == nil {
			t.Fatalf("a new session has no ledger status: %v", res.Msg.GetSession())
		}
		return &sessionLedgerHarness{t: t, h: h, ctx: ctx, sessions: sessions, ledger: ledgerAPI, id: res.Msg.GetSession().GetSessionId()}
	}

	// Creating a session prepares its snapshot on the device; the agent
	// learns its path from the lease.
	s := create()
	s.waitFor("the snapshot", func() bool {
		return s.session().GetLedger().GetState() == pb.SessionLedgerStatus_STATE_READY
	})
	snapshot := filepath.Join(h.dir, "state", "circus", "ledger", s.id)
	if got := s.path(); got != snapshot {
		t.Fatalf("snapshot at %s, want %s", got, snapshot)
	}
	if body, ok := s.file("D0-Storage.md"); !ok || !strings.Contains(body, "Keep data in SQLite.") {
		t.Fatalf("snapshot D0 = %q", body)
	}
	s.turn("/fake ledger-path")
	found := false
	for _, e := range s.events() {
		if e.Kind == "ASSISTANT" && strings.Contains(string(e.Payload), "ledger path: "+snapshot) {
			found = true
		}
	}
	if !found {
		t.Fatal("the worker did not get the snapshot's path in its lease")
	}

	// A turn adds a decision and revises D0: both are collected into the
	// session's changeset.
	r := s.edit(
		write("D?a-Backups.md", ledgerDecision("D?a", "Backups", "Back up nightly; storage per D0.\n")),
		edit("D0-Storage.md", "SQLite.", "SQLite in WAL mode."))
	if strings.Join(entryPaths(r), ",") != "D0-Storage.md,D?a-Backups.md" || len(r.GetProblems()) != 0 {
		t.Fatalf("entries %v, problems %v", r.GetEntries(), r.GetProblems())
	}
	if e := r.GetEntries()[0]; !e.GetContentChanged() || e.GetNumber() != "D0" || e.GetAdded() {
		t.Fatalf("revised entry %v", e)
	}
	if e := r.GetEntries()[1]; !e.GetAdded() || e.GetNumber() != "D?a" || e.GetTitle() != "Backups" {
		t.Fatalf("new entry %v", e)
	}
	if st := r.GetStatus(); st.GetChangesetId() == "" || st.GetChangeCount() != 2 || st.GetProblemCount() != 0 || !r.GetFastForward() {
		t.Fatalf("status %v, fast-forward %v", st, r.GetFastForward())
	}
	diff, err := ledgerAPI.GetSessionLedgerDiff(ctx, connect.NewRequest(&pb.GetSessionLedgerDiffRequest{SessionId: s.id, Path: "D0-Storage.md"}))
	must(t, err)
	if !diff.Msg.GetBaseExists() || !strings.Contains(diff.Msg.GetBase(), "SQLite.") || !strings.Contains(diff.Msg.GetChangeset(), "WAL mode") {
		t.Fatalf("diff %v", diff.Msg)
	}

	// A file that is not a decision is a problem: the agent hears of it
	// once, in a note that starts a turn, and fixing it clears it.
	r = s.edit(write("notes.txt", "scratch\n"))
	if r.GetStatus().GetProblemCount() == 0 || len(r.GetProblems()) == 0 || r.GetProblems()[0].GetPath() != "notes.txt" {
		t.Fatalf("problems %v", r.GetProblems())
	}
	note := s.noteTurn("ledger_problems", 1)
	if !strings.Contains(string(note.Payload), "notes.txt") || !strings.Contains(string(note.Payload), snapshot) {
		t.Fatalf("problems note %s", note.Payload)
	}
	s.waitLedger("the collection after the note's turn", settled(note.At))
	if n := len(s.notes("ledger_problems")); n != 1 {
		t.Fatalf("%d problem notes for one set of problems", n)
	}
	r = s.edit(map[string]string{"op": "delete", "path": "notes.txt"})
	if r.GetStatus().GetProblemCount() != 0 || len(r.GetEntries()) != 2 {
		t.Fatalf("after the fix: %v", r)
	}

	// The owner merges from the session page: it fast-forwards, D?a
	// becomes D1, and the snapshot follows.
	merged, err := ledgerAPI.MergeSessionLedger(ctx, connect.NewRequest(&pb.MergeSessionLedgerRequest{SessionId: s.id,
		ExpectedChangesetId: r.GetStatus().GetChangesetId()}))
	must(t, err)
	if merged.Msg.GetNumbers()["D?a"] != 1 || merged.Msg.GetChangeset().GetState() != pb.Changeset_STATE_MERGED {
		t.Fatalf("merge %v", merged.Msg)
	}
	note = s.noteTurn("ledger_merged", 1)
	if !strings.Contains(string(note.Payload), "D?a is now D1") {
		t.Fatalf("merged note %s", note.Payload)
	}
	if body, ok := s.file("D1-Backups.md"); !ok || !strings.Contains(body, "number: D1") || !strings.Contains(body, "storage per D0") {
		t.Fatalf("renamed decision %q", body)
	}
	if _, ok := s.file("D?a-Backups.md"); ok {
		t.Fatal("the temporary file is still in the snapshot")
	}
	if index, _ := s.file("INDEX.md"); !strings.Contains(index, "| D1 |") {
		t.Fatalf("index %q", index)
	}
	r = s.waitLedger("no changes after the rebase", settled(note.At))
	if r.GetStatus().GetChangesetId() != "" || len(r.GetMerged()) != 1 || r.GetMerged()[0].GetNumbers()["D?a"] != 1 {
		t.Fatalf("after the merge: %v", r)
	}
	ledgerEvents := 0
	for _, e := range s.events() {
		if e.Kind == "LEDGER" && strings.Contains(string(e.Payload), `"event":"merged"`) {
			ledgerEvents++
		}
	}
	if ledgerEvents != 1 {
		t.Fatalf("%d durable merge events", ledgerEvents)
	}

	// The next changeset starts from the session's view and fast-forwards.
	r = s.edit(edit("D1-Backups.md", "nightly", "hourly"))
	if !r.GetFastForward() || len(r.GetEntries()) != 1 {
		t.Fatalf("second changeset %v", r)
	}
	_, err = ledgerAPI.MergeSessionLedger(ctx, connect.NewRequest(&pb.MergeSessionLedgerRequest{SessionId: s.id,
		ExpectedChangesetId: r.GetStatus().GetChangesetId()}))
	must(t, err)
	s.noteTurn("ledger_merged", 2)

	// Someone else merges: the session's next changeset no longer
	// fast-forwards, and only an integrator merges it.
	other, otherDir := h.ledgerOpen(nil, "Plans")
	editInPlace(t, filepath.Join(otherDir, "D0-Storage.md"), "WAL mode.", "WAL mode, checkpointed hourly.")
	h.ledgerCLI(nil, 0, "merge", other)
	r = s.edit(edit("D1-Backups.md", "hourly", "every hour"))
	if r.GetFastForward() {
		t.Fatalf("fast-forward after another merge: %v", r)
	}
	changeset := r.GetStatus().GetChangesetId()
	_, err = ledgerAPI.MergeSessionLedger(ctx, connect.NewRequest(&pb.MergeSessionLedgerRequest{SessionId: s.id, ExpectedChangesetId: changeset}))
	if got := preconditionType(t, err); got != "NOT_FAST_FORWARD" {
		t.Fatalf("merge refused with %s", got)
	}
	out, _ := h.ledgerCLI(nil, 0, "list", "Plans", "--session", s.id, "--state", "open")
	if !strings.HasPrefix(out, changeset+"\topen") || !strings.Contains(out, "session "+s.id) {
		t.Fatalf("list --session:\n%s", out)
	}
	_, errOut := h.ledgerCLI(nil, 1, "check", changeset)
	if !strings.Contains(errOut, "integrator grant") {
		t.Fatalf("check without the grant:\n%s", errOut)
	}
	out, _ = h.ledgerCLI(integratorEnv, 0, "check", changeset)
	if !strings.Contains(out, "session: "+s.id) || !strings.Contains(out, "fast-forward: no") || strings.Contains(out, "conflict") {
		t.Fatalf("integrator check:\n%s", out)
	}
	h.ledgerCLI(integratorEnv, 0, "merge", changeset)
	s.noteTurn("ledger_merged", 3)
	if body, _ := s.file("D1-Backups.md"); !strings.Contains(body, "every hour") {
		t.Fatalf("D1 after the integrator's merge %q", body)
	}
	if body, _ := s.file("D0-Storage.md"); strings.Contains(body, "checkpointed") {
		t.Fatalf("the snapshot took in another merge: %q", body)
	}

	// The integrator discards a later changeset: the snapshot is written
	// again and the agent told why.
	r = s.edit(write("ops/D?b-Later.md", ledgerDecision("D?b", "Later", "Maybe.\n")))
	discarded := r.GetStatus().GetChangesetId()
	h.ledgerCLI(integratorEnv, 0, "discard", discarded, "--reason", "not now")
	note = s.noteTurn("ledger_discarded", 1)
	if !strings.Contains(string(note.Payload), "not now") {
		t.Fatalf("discarded note %s", note.Payload)
	}
	if _, ok := s.file("ops/D?b-Later.md"); ok {
		t.Fatal("the discarded decision is still in the snapshot")
	}
	r = s.waitLedger("no changes after the reset", settled(note.At))
	if r.GetStatus().GetChangesetId() != "" {
		t.Fatalf("after the reset: %v", r)
	}

	// A device offline when a turn ends leaves its collection due; the
	// merge from the session page waits for it. An integrator merges the
	// last collection meanwhile; the agent changes the snapshot before
	// the device is back, so the rebase keeps the change and the next
	// collection takes it into the next changeset.
	r = s.edit(write("D?c-Cache.md", ledgerDecision("D?c", "Cache", "Cache reads.\n")))
	pending := r.GetStatus().GetChangesetId()
	h.stopDaemon()
	s.turn("while the device is away")
	r = s.waitLedger("the collection due", func(r *pb.GetSessionLedgerResponse) bool { return r.GetStatus().GetCollectionDue() })
	_, err = ledgerAPI.MergeSessionLedger(ctx, connect.NewRequest(&pb.MergeSessionLedgerRequest{SessionId: s.id, ExpectedChangesetId: pending}))
	if got := preconditionType(t, err); got != "DEVICE_OFFLINE" {
		t.Fatalf("merge refused with %s", got)
	}
	out, _ = h.ledgerCLI(integratorEnv, 0, "merge", pending)
	if !strings.Contains(out, "device is offline") || !strings.Contains(out, "D?c -> D2") {
		t.Fatalf("integrator merge while offline:\n%s", out)
	}
	editInPlace(t, filepath.Join(snapshot, "D0-Storage.md"), "WAL mode.", "WAL mode, with a later change.")
	restarted := time.Now().UnixMilli()
	h.restartDaemon()
	note = s.noteTurn("ledger_merged", 4)
	if !strings.Contains(string(note.Payload), "D?c is now D2") {
		t.Fatalf("merged note %s", note.Payload)
	}
	if _, ok := s.file("D2-Cache.md"); !ok {
		t.Fatal("D?c was not renamed")
	}
	if body, _ := s.file("D0-Storage.md"); !strings.Contains(body, "with a later change") {
		t.Fatalf("the agent's later change was lost: %q", body)
	}
	r = s.waitLedger("the later change collected", func(r *pb.GetSessionLedgerResponse) bool {
		return settled(restarted)(r) && strings.Join(entryPaths(r), ",") == "D0-Storage.md"
	})
	if !r.GetEntries()[0].GetContentChanged() {
		t.Fatalf("later change %v", r.GetEntries())
	}

	// A session whose changeset has changes is deleted only with its
	// changes discarded; its snapshot is removed from the device.
	_, err = sessions.StopSession(ctx, connect.NewRequest(&pb.StopSessionRequest{SessionId: s.id}))
	must(t, err)
	s.waitFor("the stop", func() bool { return s.session().GetState() == pb.Session_STATE_STOPPED })
	_, err = sessions.DeleteSession(ctx, connect.NewRequest(&pb.DeleteSessionRequest{SessionId: s.id}))
	if got := preconditionType(t, err); got != "LEDGER_CHANGES" {
		t.Fatalf("delete refused with %s", got)
	}
	last := r.GetStatus().GetChangesetId()
	_, err = sessions.DeleteSession(ctx, connect.NewRequest(&pb.DeleteSessionRequest{SessionId: s.id, DiscardLedgerChanges: true}))
	must(t, err)
	cs, err := h.st.GetLedgerChangeset(ctx, last)
	must(t, err)
	if cs.State != store.LedgerChangesetDiscarded || cs.DiscardReason != "session deleted" {
		t.Fatalf("the deleted session's changeset %v", cs)
	}
	s.waitFor("the snapshot's removal", func() bool {
		_, err := os.Stat(snapshot)
		return errors.Is(err, os.ErrNotExist)
	})

	// A turn ends while the device is offline: its collection is due, and
	// a deletion without discarding the changes is refused although no
	// changeset is open yet. The collection runs when the device returns.
	s2 := create()
	s2.waitFor("the second snapshot", func() bool {
		return s2.session().GetLedger().GetState() == pb.SessionLedgerStatus_STATE_READY
	})
	second := s2.path()
	h.stopDaemon()
	// The agent's tools reach the device through its daemon: the file is
	// written beside the turn, as the agent would.
	writeLedgerFile(t, filepath.Join(second, "D?d-Offline.md"), ledgerDecision("D?d", "Offline", "Written offline.\n"))
	s2.turn("while the device is away")
	s2.waitLedger("the collection due", func(r *pb.GetSessionLedgerResponse) bool {
		return r.GetStatus().GetCollectionDue() && r.GetStatus().GetChangesetId() == ""
	})
	_, err = sessions.StopSession(ctx, connect.NewRequest(&pb.StopSessionRequest{SessionId: s2.id}))
	must(t, err)
	s2.waitFor("the stop", func() bool { return s2.session().GetState() == pb.Session_STATE_STOPPED })
	_, err = sessions.DeleteSession(ctx, connect.NewRequest(&pb.DeleteSessionRequest{SessionId: s2.id}))
	if got := preconditionType(t, err); got != "LEDGER_CHANGES" {
		t.Fatalf("offline delete with uncollected changes refused with %s", got)
	}
	restarted = time.Now().UnixMilli()
	h.restartDaemon()
	r = s2.waitLedger("the collection on reconnect", settled(restarted))
	if got := strings.Join(entryPaths(r), ","); got != "D?d-Offline.md" {
		t.Fatalf("collected on reconnect: %s", got)
	}
	offline := r.GetStatus().GetChangesetId()

	// Deleted while its device is offline, with its changes discarded, a
	// session's snapshot is removed when the device returns.
	h.stopDaemon()
	_, err = sessions.DeleteSession(ctx, connect.NewRequest(&pb.DeleteSessionRequest{SessionId: s2.id, DiscardLedgerChanges: true}))
	must(t, err)
	if cs, err := h.st.GetLedgerChangeset(ctx, offline); err != nil || cs.State != store.LedgerChangesetDiscarded {
		t.Fatalf("the deleted session's changeset %v, %v", cs, err)
	}
	if _, err := h.st.GetLedgerSnapshotRemoval(ctx, s2.id); err != nil {
		t.Fatalf("no pending removal: %v", err)
	}
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("the snapshot went while its device was offline: %v", err)
	}
	h.restartDaemon()
	s2.waitFor("the pending removal", func() bool {
		_, statErr := os.Stat(second)
		_, err := h.st.GetLedgerSnapshotRemoval(ctx, s2.id)
		return errors.Is(statErr, os.ErrNotExist) && errors.Is(err, store.ErrNotFound)
	})
}
