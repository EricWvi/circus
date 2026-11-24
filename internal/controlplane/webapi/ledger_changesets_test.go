package webapi_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptrace"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

func withWorkers(cfg *webapi.Config) { cfg.Workers = auth.NewAuthenticator(cfg.Store, auth.Options{}) }

// credential creates an identity of the user with a credential and
// returns its token.
func (h *harness) credential(t *testing.T, kind store.PrincipalKind, userID, id, name string) string {
	t.Helper()
	ctx := t.Context()
	if err := h.st.CreateIdentity(ctx, store.Identity{Kind: kind, ID: id, UserID: userID, Name: name, MachineID: "m-" + id, CreatedAt: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}
	token, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateCredential(ctx, store.Credential{ID: "c-" + id, Kind: kind, PrincipalID: id, TokenHash: auth.HashToken(token), CreatedAt: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}
	return token
}

func bearer(token string) connect.ClientOption {
	return connect.WithInterceptors(connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			if token != "" {
				req.Header().Set("Authorization", "Bearer "+token)
			}
			return next(ctx, req)
		}
	}))
}

func (h *harness) changesets(t *testing.T, token string, options ...connect.ClientOption) circusv1connect.LedgerChangesetServiceClient {
	t.Helper()
	c := &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(c.CloseIdleConnections)
	return circusv1connect.NewLedgerChangesetServiceClient(c, h.server.URL, append(options, bearer(token))...)
}

// worker creates a worker of the user and a client with its credential.
func (h *harness) worker(t *testing.T, userID, id string, integrator bool) circusv1connect.LedgerChangesetServiceClient {
	t.Helper()
	token := h.credential(t, store.KindWorker, userID, id, id)
	if integrator {
		if _, err := h.st.SetWorkerIntegrator(t.Context(), userID, id, true); err != nil {
			t.Fatal(err)
		}
	}
	return h.changesets(t, token)
}

func wantIntegratorRequired(t *testing.T, err error) {
	t.Helper()
	wantCode(t, err, connect.CodePermissionDenied)
	var ce *connect.Error
	errors.As(err, &ce)
	for _, d := range ce.Details() {
		if v, e := d.Value(); e == nil {
			if info, ok := v.(*errdetails.ErrorInfo); ok && info.Reason == "INTEGRATOR_REQUIRED" {
				return
			}
		}
	}
	t.Fatalf("error %v carries no INTEGRATOR_REQUIRED", err)
}

func preconditionSubjects(t *testing.T, err error) []string {
	t.Helper()
	wantCode(t, err, connect.CodeFailedPrecondition)
	var ce *connect.Error
	errors.As(err, &ce)
	var out []string
	for _, d := range ce.Details() {
		if v, e := d.Value(); e == nil {
			if f, ok := v.(*errdetails.PreconditionFailure); ok {
				for _, violation := range f.Violations {
					out = append(out, violation.Type+":"+violation.Subject)
				}
			}
		}
	}
	return out
}

func content(p, body string) *circusv1.FileChange {
	return &circusv1.FileChange{Path: p, Change: &circusv1.FileChange_Content{Content: body}}
}

func deleted(p string) *circusv1.FileChange {
	return &circusv1.FileChange{Path: p, Change: &circusv1.FileChange_Deleted{Deleted: true}}
}

func decisionFile(number, title, body string) string {
	return "---\nnumber: " + number + "\ntitle: " + title + "\n---\n\n" + body
}

func TestChangesetServiceAuthentication(t *testing.T) {
	h, owner := newHarness(t, withWorkers)
	ctx := t.Context()
	project := createLedgerProject(t, owner)
	device := h.credential(t, store.KindDevice, h.owner.ID, "d1", "desk")
	revoked := h.credential(t, store.KindWorker, h.owner.ID, "w-revoked", "gone")
	if _, _, err := auth.RevokeByName(ctx, h.st, store.KindWorker, h.owner.ID, "gone", time.UnixMilli(h.clock.Load())); err != nil {
		t.Fatal(err)
	}
	cookie := circusv1connect.NewLedgerChangesetServiceClient(owner.http, h.server.URL)
	for name, api := range map[string]circusv1connect.LedgerChangesetServiceClient{
		"none": h.changesets(t, ""), "malformed": h.changesets(t, "x"), "daemon": h.changesets(t, device),
		"revoked": h.changesets(t, revoked), "cookie": cookie, "json": h.changesets(t, device, connect.WithProtoJSON()),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := api.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: project}))
			wantCode(t, err, connect.CodeUnauthenticated)
			_, err = api.ListChangesets(ctx, connect.NewRequest(&circusv1.ListChangesetsRequest{Project: project}))
			wantCode(t, err, connect.CodeUnauthenticated)
		})
	}
}

func TestChangesetServicePermissions(t *testing.T) {
	h, owner := newHarness(t, withWorkers)
	ctx := t.Context()
	createLedgerProject(t, owner) // named "Ledger"
	opener := h.worker(t, h.owner.ID, "w-opener", false)
	other := h.worker(t, h.owner.ID, "w-other", false)
	integrator := h.worker(t, h.owner.ID, "w-integrator", true)
	h.signIn(t, "stranger", "stranger@example.com")
	stranger := h.worker(t, "stranger", "w-stranger", true)

	opened, err := opener.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: "ledger"}))
	if err != nil {
		t.Fatal(err)
	}
	c := opened.Msg.GetChangeset()
	if c.State != circusv1.Changeset_STATE_OPEN || c.OpenerWorkerId != "w-opener" || c.BaseCommit != "" {
		t.Fatalf("opened = %v", c)
	}
	id := c.ChangesetId
	_, err = stranger.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: c.ProjectId}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = stranger.GetChangeset(ctx, connect.NewRequest(&circusv1.GetChangesetRequest{ChangesetId: id}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = stranger.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = other.GetChangeset(ctx, connect.NewRequest(&circusv1.GetChangesetRequest{ChangesetId: id}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.ReadChangesetBase(ctx, connect.NewRequest(&circusv1.ReadChangesetBaseRequest{ChangesetId: id}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = other.GetChangeset(ctx, connect.NewRequest(&circusv1.GetChangesetRequest{ChangesetId: "../x"}))
	wantField(t, err, connect.CodeInvalidArgument, "changeset_id")

	changes := []*circusv1.FileChange{content("D?a-First.md", decisionFile("D?a", "First", "first, before D?b\n")),
		content("core/D?b-Second.md", decisionFile("D?b", "Second", "second\n"))}
	_, err = other.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id, Changes: changes}))
	wantIntegratorRequired(t, err)
	_, err = other.DiscardChangeset(ctx, connect.NewRequest(&circusv1.DiscardChangesetRequest{ChangesetId: id, Reason: "no"}))
	wantIntegratorRequired(t, err)
	_, err = other.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: id}))
	wantIntegratorRequired(t, err)
	if _, err := integrator.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id, Changes: changes})); err != nil {
		t.Fatal(err)
	}
	checked, err := opener.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id, Changes: changes}))
	if err != nil || !checked.Msg.FastForward || len(checked.Msg.Problems) != 0 {
		t.Fatalf("check = %v, %v", checked, err)
	}
	// A second changeset from the same base is behind main after the first
	// merges.
	behind, err := opener.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: c.ProjectId}))
	if err != nil {
		t.Fatal(err)
	}
	behindID := behind.Msg.GetChangeset().GetChangesetId()
	code := strings.Repeat("e", 40)
	merged, err := opener.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: id, CodeCommit: code}))
	if err != nil {
		t.Fatal(err)
	}
	if m := merged.Msg; m.Changeset.State != circusv1.Changeset_STATE_MERGED || m.Changeset.CodeCommit != code || m.Numbers["D?a"] != 0 || m.Numbers["D?b"] != 1 {
		t.Fatalf("merge = %v", m)
	}
	again, err := integrator.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: id}))
	if err != nil || again.Msg.Changeset.MergedCommit != merged.Msg.Changeset.MergedCommit || again.Msg.Numbers["D?a"] != 0 {
		t.Fatalf("second merge = %v, %v", again, err)
	}

	edit := []*circusv1.FileChange{content("D0-First.md", decisionFile("D0", "First", "edited\n"))}
	res, err := opener.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: behindID, Changes: edit}))
	if err != nil || res.Msg.FastForward || res.Msg.MainCommit != merged.Msg.Changeset.MergedCommit {
		t.Fatalf("check behind main = %v, %v", res, err)
	}
	// The base had no D0, so the edit is a new file with a number of its
	// own: a validation problem, not a conflict.
	if len(res.Msg.Problems) != 1 || len(res.Msg.Conflicts) != 1 || res.Msg.Conflicts[0].Path != "D0-First.md" || res.Msg.Conflicts[0].BaseExists {
		t.Fatalf("check behind main = %v", res.Msg)
	}
	_, err = opener.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: behindID}))
	wantIntegratorRequired(t, err)
	if _, err := h.st.SetWorkerIntegrator(ctx, h.owner.ID, "w-integrator", false); err != nil {
		t.Fatal(err)
	}
	_, err = integrator.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: behindID}))
	wantIntegratorRequired(t, err)
	if _, err := h.st.SetWorkerIntegrator(ctx, h.owner.ID, "w-integrator", true); err != nil {
		t.Fatal(err)
	}
	_, err = integrator.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: behindID}))
	// Its content is not final until it is resolved, so only the conflict
	// is reported.
	if got := preconditionSubjects(t, err); len(got) != 1 || got[0] != "UNRESOLVED_CONFLICT:D0-First.md" {
		t.Fatalf("violations = %v", got)
	}
	_, err = integrator.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: behindID, CodeCommit: "HEAD"}))
	wantField(t, err, connect.CodeInvalidArgument, "code_commit")
	_, err = integrator.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: behindID,
		Resolved: []*circusv1.FileChange{{Path: "D0-First.md"}}}))
	wantField(t, err, connect.CodeInvalidArgument, "resolved[0]")
	// A resolution is validated like any change: here its title no longer
	// matches its path.
	_, err = integrator.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: behindID,
		Resolved: []*circusv1.FileChange{content("D0-First.md", decisionFile("D0", "Renamed", "resolved\n"))}}))
	if got := preconditionSubjects(t, err); len(got) != 1 || got[0] != "LEDGER_PROBLEM:D0-First.md" {
		t.Fatalf("violations of an invalid resolution = %v", got)
	}
	mergedBehind, err := integrator.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: behindID,
		Resolved: []*circusv1.FileChange{content("D0-First.md", decisionFile("D0", "First", "resolved\n"))}}))
	if err != nil || mergedBehind.Msg.Changeset.State != circusv1.Changeset_STATE_MERGED || len(mergedBehind.Msg.Numbers) != 0 {
		t.Fatalf("integrator merge = %v, %v", mergedBehind, err)
	}

	// Discards: the reason is required; a merged changeset stays merged.
	third, err := opener.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: c.ProjectId}))
	if err != nil {
		t.Fatal(err)
	}
	thirdID := third.Msg.GetChangeset().GetChangesetId()
	_, err = opener.DiscardChangeset(ctx, connect.NewRequest(&circusv1.DiscardChangesetRequest{ChangesetId: thirdID, Reason: "  "}))
	wantField(t, err, connect.CodeInvalidArgument, "reason")
	_, err = opener.DiscardChangeset(ctx, connect.NewRequest(&circusv1.DiscardChangesetRequest{ChangesetId: thirdID, Reason: strings.Repeat("x", 1025)}))
	wantField(t, err, connect.CodeInvalidArgument, "reason")
	discarded, err := integrator.DiscardChangeset(ctx, connect.NewRequest(&circusv1.DiscardChangesetRequest{ChangesetId: thirdID, Reason: "not needed"}))
	if err != nil || discarded.Msg.Changeset.State != circusv1.Changeset_STATE_DISCARDED || discarded.Msg.Changeset.DiscardReason != "not needed" {
		t.Fatalf("discard = %v, %v", discarded, err)
	}
	_, err = opener.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: thirdID}))
	if got := preconditionSubjects(t, err); len(got) != 1 || got[0] != "CHANGESET_STATE:"+thirdID {
		t.Fatalf("merge of a discarded changeset = %v", got)
	}
	_, err = opener.DiscardChangeset(ctx, connect.NewRequest(&circusv1.DiscardChangesetRequest{ChangesetId: id, Reason: "late"}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	_, err = opener.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: thirdID}))
	wantCode(t, err, connect.CodeFailedPrecondition)

	// Listing pages newest first, by state.
	var listed []string
	token := ""
	for {
		page, err := other.ListChangesets(ctx, connect.NewRequest(&circusv1.ListChangesetsRequest{Project: "Ledger", PageSize: 1, PageToken: token}))
		if err != nil {
			t.Fatal(err)
		}
		for _, cs := range page.Msg.Changesets {
			listed = append(listed, cs.ChangesetId)
		}
		if token = page.Msg.NextPageToken; token == "" {
			break
		}
	}
	if strings.Join(listed, ",") != strings.Join([]string{thirdID, behindID, id}, ",") {
		t.Fatalf("listed %v", listed)
	}
	page, err := other.ListChangesets(ctx, connect.NewRequest(&circusv1.ListChangesetsRequest{Project: "Ledger", State: circusv1.Changeset_STATE_DISCARDED}))
	if err != nil || len(page.Msg.Changesets) != 1 || page.Msg.Changesets[0].ChangesetId != thirdID {
		t.Fatalf("discarded page = %v, %v", page, err)
	}
	_, err = other.ListChangesets(ctx, connect.NewRequest(&circusv1.ListChangesetsRequest{Project: "Ledger", PageToken: "bad"}))
	wantField(t, err, connect.CodeInvalidArgument, "page_token")
	_, err = other.ListChangesets(ctx, connect.NewRequest(&circusv1.ListChangesetsRequest{Project: "Ledger", PageSize: 101}))
	wantField(t, err, connect.CodeInvalidArgument, "page_size")
	_, err = other.ListChangesets(ctx, connect.NewRequest(&circusv1.ListChangesetsRequest{Project: "Elsewhere"}))
	wantCode(t, err, connect.CodeNotFound)

	// The base of the last merge holds both decisions and the index.
	fourth, err := other.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: "Ledger"}))
	if err != nil {
		t.Fatal(err)
	}
	base, err := other.ReadChangesetBase(ctx, connect.NewRequest(&circusv1.ReadChangesetBaseRequest{ChangesetId: fourth.Msg.Changeset.ChangesetId}))
	if err != nil || len(base.Msg.Files) != 3 || base.Msg.NextPageToken != "" || base.Msg.Files[0].Path != "D0-First.md" ||
		!strings.HasSuffix(base.Msg.Files[0].Content, "first, before D1\n") && !strings.HasSuffix(base.Msg.Files[0].Content, "resolved\n") {
		t.Fatalf("base = %v, %v", base, err)
	}
}

func TestChangesetServiceLimits(t *testing.T) {
	h, owner := newHarness(t, withWorkers)
	ctx := t.Context()
	project := createLedgerProject(t, owner)
	w := h.worker(t, h.owner.ID, "w1", false)
	var id string
	for i := range store.MaxOpenLedgerChangesets {
		res, err := w.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: project}))
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		id = res.Msg.Changeset.ChangesetId
	}
	_, err := w.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: project}))
	wantCode(t, err, connect.CodeResourceExhausted)
	big := strings.Repeat("x", 200<<10)
	var changes []*circusv1.FileChange
	for i := range 42 {
		changes = append(changes, content("f"+strings.Repeat("y", i), big))
	}
	_, err = w.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id, Changes: changes}))
	wantCode(t, err, connect.CodeResourceExhausted)
	_, err = w.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id,
		Changes: []*circusv1.FileChange{content("a", ""), deleted("a")}}))
	wantField(t, err, connect.CodeInvalidArgument, "changes[1].path")
	_, err = w.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id,
		Changes: []*circusv1.FileChange{deleted("missing.md")}}))
	wantField(t, err, connect.CodeInvalidArgument, "changes[0].path")
}

func TestSetWorkerGrant(t *testing.T) {
	h, owner := newHarness(t, withWorkers)
	ctx := t.Context()
	h.credential(t, store.KindWorker, h.owner.ID, "w1", "builder")
	res, err := owner.machines.SetWorkerGrant(ctx, connect.NewRequest(&circusv1.SetWorkerGrantRequest{WorkerId: "w1", Integrator: true}))
	if err != nil || !res.Msg.Worker.Integrator || res.Msg.Worker.Name != "builder" {
		t.Fatalf("grant = %v, %v", res, err)
	}
	list, err := owner.machines.ListWorkers(ctx, connect.NewRequest(&circusv1.ListWorkersRequest{}))
	if err != nil || len(list.Msg.Workers) != 1 || !list.Msg.Workers[0].Integrator {
		t.Fatalf("workers = %v, %v", list, err)
	}
	stranger := h.signIn(t, "stranger", "stranger@example.com")
	_, err = stranger.machines.SetWorkerGrant(ctx, connect.NewRequest(&circusv1.SetWorkerGrantRequest{WorkerId: "w1"}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = owner.machines.SetWorkerGrant(ctx, connect.NewRequest(&circusv1.SetWorkerGrantRequest{WorkerId: "missing", Integrator: true}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = owner.machines.SetWorkerGrant(ctx, connect.NewRequest(&circusv1.SetWorkerGrantRequest{}))
	wantField(t, err, connect.CodeInvalidArgument, "worker_id")
	_, err = h.browser(t).machines.SetWorkerGrant(ctx, connect.NewRequest(&circusv1.SetWorkerGrantRequest{WorkerId: "w1"}))
	wantCode(t, err, connect.CodeUnauthenticated)
	res, err = owner.machines.SetWorkerGrant(ctx, connect.NewRequest(&circusv1.SetWorkerGrantRequest{WorkerId: "w1"}))
	if err != nil || res.Msg.Worker.Integrator {
		t.Fatalf("revoke grant = %v, %v", res, err)
	}
}

// TestChangesetWriteSlotsPerUser: a user holding its share of the check
// and merge slots, with a request whose body never comes, keeps no other
// user out.
func TestChangesetWriteSlotsPerUser(t *testing.T) {
	h, _ := newHarness(t, withWorkers)
	ctx := t.Context()
	tokenA := h.credential(t, store.KindWorker, h.owner.ID, "w-a", "a")
	a := h.changesets(t, tokenA)
	stranger := h.signIn(t, "stranger", "stranger@example.com")
	if _, err := stranger.projects.CreateProject(ctx, connect.NewRequest(&circusv1.CreateProjectRequest{Name: "Other"})); err != nil {
		t.Fatal(err)
	}
	b := h.worker(t, "stranger", "w-b", false)

	h.holdWriteSlot(t, a, tokenA)
	opened, err := b.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: "Other"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: opened.Msg.Changeset.ChangesetId,
		Changes: []*circusv1.FileChange{content("D?a-New.md", decisionFile("D?a", "New", "x\n"))}})); err != nil {
		t.Fatalf("another user's check while one user holds its share: %v", err)
	}
}

// finishFails cannot record a merge as merged, as a crash right after the
// main line moved leaves it.
type finishFails struct{ store.LedgerOps }

func (finishFails) FinishLedgerMerge(context.Context, string, string, int64) (bool, error) {
	return false, errors.New("crash")
}

// TestChangesetReadsRepairInterruptedMerges: a merge whose commit reached
// the main line reads as merged, not open, before anything else repairs it.
func TestChangesetReadsRepairInterruptedMerges(t *testing.T) {
	h, owner := newHarness(t, withWorkers)
	ctx := t.Context()
	createLedgerProject(t, owner)
	w := h.worker(t, h.owner.ID, "w1", false)
	crashing, err := ledger.New(filepath.Join(h.dir, "web.db"), finishFails{h.st}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = crashing.Close(context.Background()) })
	interrupt := func() string {
		t.Helper()
		opened, err := w.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: "Ledger"}))
		if err != nil {
			t.Fatal(err)
		}
		id := opened.Msg.Changeset.ChangesetId
		if _, err := w.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id,
			Changes: []*circusv1.FileChange{content("D?a-New.md", decisionFile("D?a", "New", id+"\n"))}})); err != nil {
			t.Fatal(err)
		}
		_, err = crashing.MergeChangeset(ctx, id, ledger.MergeOptions{Author: "w1", Authorize: func(context.Context, bool) error { return nil }})
		if err == nil {
			t.Fatal("the merge was recorded")
		}
		if c, err := h.st.GetLedgerChangeset(ctx, id); err != nil || !c.Pending() {
			t.Fatalf("interrupted merge = %+v, %v", c, err)
		}
		return id
	}
	first := interrupt()
	got, err := w.GetChangeset(ctx, connect.NewRequest(&circusv1.GetChangesetRequest{ChangesetId: first}))
	if err != nil || got.Msg.Changeset.State != circusv1.Changeset_STATE_MERGED || got.Msg.Changeset.MergedCommit == "" {
		t.Fatalf("get = %v, %v", got, err)
	}
	second := interrupt()
	page, err := w.ListChangesets(ctx, connect.NewRequest(&circusv1.ListChangesetsRequest{Project: "Ledger"}))
	if err != nil || len(page.Msg.Changesets) != 2 || page.Msg.Changesets[0].ChangesetId != second ||
		page.Msg.Changesets[0].State != circusv1.Changeset_STATE_MERGED || page.Msg.Changesets[1].State != circusv1.Changeset_STATE_MERGED {
		t.Fatalf("list = %v, %v", page, err)
	}
}

// stalledCall is a request whose body stops after its first byte until
// the test ends.
type stalledCall struct {
	done   chan struct{}
	status int
	err    error
}

// stall sends a POST whose body stops after its first byte and returns
// once its headers are on the way.
func (h *harness) stall(t *testing.T, path, authorization string, first []byte) *stalledCall {
	t.Helper()
	return h.stallAs(t, path, authorization, "application/proto", first)
}

// stallAs is stall with another Content-Type.
func (h *harness) stallAs(t *testing.T, path, authorization, contentType string, first []byte) *stalledCall {
	t.Helper()
	body, upload := io.Pipe()
	wrote := make(chan struct{})
	c := &stalledCall{done: make(chan struct{})}
	go func() {
		defer close(c.done)
		req, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			WroteHeaders: func() { close(wrote) },
		}), http.MethodPost, h.server.URL+path, body)
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", contentType)
		if authorization != "" {
			req.Header.Set("Authorization", authorization)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			c.err = err
			return
		}
		c.status = res.StatusCode
		_, _ = io.Copy(io.Discard, res.Body)
		_ = res.Body.Close()
	}()
	go func() { _, _ = upload.Write(first) }()
	t.Cleanup(func() {
		_ = upload.CloseWithError(io.ErrUnexpectedEOF)
		<-c.done
	})
	select {
	case <-wrote:
	case <-c.done:
	}
	return c
}

// finished reports whether the call got its answer within d.
func (c *stalledCall) finished(d time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(d):
		return false
	}
}

// holdWriteSlot holds the user's check and merge slot with a check whose
// body never comes: the slot is held once a check through api, with the
// same credential, is refused. A held check that arrived while that
// check held the slot was refused at once, and is sent again.
func (h *harness) holdWriteSlot(t *testing.T, api circusv1connect.LedgerChangesetServiceClient, token string) {
	t.Helper()
	holder := h.holdCheck(t, token)
	unknown := &circusv1.CheckChangesetRequest{ChangesetId: "0190a0a0-0000-7000-8000-000000000000"}
	for deadline := time.Now().Add(30 * time.Second); ; {
		_, err := api.CheckChangeset(t.Context(), connect.NewRequest(unknown))
		if busyRefusal(err) {
			return
		}
		wantCode(t, err, connect.CodeNotFound)
		if time.Now().After(deadline) {
			t.Fatal("the held check never took its slot")
		}
		if holder.finished(100 * time.Millisecond) {
			holder = h.holdCheck(t, token)
		}
	}
}

// busyRefusal reports a refusal for want of a slot, which says that a
// retry may pass.
func busyRefusal(err error) bool {
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
		return false
	}
	for _, d := range ce.Details() {
		if v, e := d.Value(); e == nil {
			if info, ok := v.(*errdetails.ErrorInfo); ok && info.Reason == "LEDGER_BUSY" {
				return true
			}
		}
	}
	return false
}

func (h *harness) holdCheck(t *testing.T, token string) *stalledCall {
	t.Helper()
	return h.stall(t, circusv1connect.LedgerChangesetServiceCheckChangesetProcedure, "Bearer "+token, []byte{0x0a})
}

// TestChangesetRefusalsAnswerAtOnce: a request refused before its body is
// read is answered at once, however long its body takes.
func TestChangesetRefusalsAnswerAtOnce(t *testing.T) {
	h, _ := newHarness(t, withWorkers)
	token := h.credential(t, store.KindWorker, h.owner.ID, "w-a", "a")
	a := h.changesets(t, token)

	stranger := h.holdCheck(t, "bad-token")
	if !stranger.finished(time.Second) || stranger.status != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stalled check: answered %d", stranger.status)
	}
	h.holdWriteSlot(t, a, token)
	over := h.holdCheck(t, token)
	if !over.finished(time.Second) || over.status != http.StatusTooManyRequests {
		t.Fatalf("stalled check over the user's share: answered %d", over.status)
	}
	// LedgerService reads its small bodies first: one over its limit is
	// answered without waiting for the rest, here less than the 256 KiB
	// that closing an unfinished body would read.
	big := h.stall(t, circusv1connect.LedgerServiceGetLedgerProcedure, "", make([]byte, 64<<10))
	if !big.finished(time.Second) || big.status != http.StatusTooManyRequests {
		t.Fatalf("stalled ledger request over its limit: answered %d, %v", big.status, big.err)
	}
}

// TestReadChangesetConflictsService: the opener or an integrator reads a
// check's conflicts with their contents, without creating changesets,
// until the main line moves.
func TestReadChangesetConflictsService(t *testing.T) {
	h, owner := newHarness(t, withWorkers)
	ctx := t.Context()
	createLedgerProject(t, owner)
	w := h.worker(t, h.owner.ID, "w1", false)
	other := h.worker(t, h.owner.ID, "w2", false)
	open := func() string {
		t.Helper()
		res, err := w.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: "Ledger"}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Changeset.ChangesetId
	}
	check := func(id string, changes ...*circusv1.FileChange) *circusv1.CheckChangesetResponse {
		t.Helper()
		res, err := w.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id, Changes: changes}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	merge := func(id string) {
		t.Helper()
		if _, err := w.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: id})); err != nil {
			t.Fatal(err)
		}
	}
	seed := open()
	check(seed, content("D?a-One.md", decisionFile("D?a", "One", "one\n")))
	merge(seed)
	a, b := open(), open()
	check(a, content("D0-One.md", decisionFile("D0", "One", "a\n")))
	merge(a)
	checked := check(b, content("D0-One.md", decisionFile("D0", "One", "b\n")))
	if len(checked.Conflicts) != 1 {
		t.Fatalf("check = %v", checked)
	}
	list := func() int {
		t.Helper()
		res, err := w.ListChangesets(ctx, connect.NewRequest(&circusv1.ListChangesetsRequest{Project: "Ledger"}))
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Msg.Changesets)
	}
	before := list()
	read := &circusv1.ReadChangesetConflictsRequest{ChangesetId: b, MainCommit: checked.MainCommit}
	_, err := other.ReadChangesetConflicts(ctx, connect.NewRequest(read))
	wantIntegratorRequired(t, err)
	res, err := w.ReadChangesetConflicts(ctx, connect.NewRequest(read))
	if err != nil {
		t.Fatal(err)
	}
	if cs := res.Msg.Conflicts; len(cs) != 1 || res.Msg.NextPageToken != "" || cs[0].Path != "D0-One.md" ||
		!strings.HasSuffix(cs[0].Base, "\none\n") || cs[0].Changeset != decisionFile("D0", "One", "b\n") ||
		!strings.HasSuffix(cs[0].Main, "\na\n") || cs[0].ContentOmitted {
		t.Fatalf("conflicts = %v", res.Msg)
	}
	if after := list(); after != before {
		t.Fatalf("reading conflicts changed the changesets: %d, then %d", before, after)
	}
	_, err = w.ReadChangesetConflicts(ctx, connect.NewRequest(&circusv1.ReadChangesetConflictsRequest{ChangesetId: b, MainCommit: "HEAD"}))
	wantField(t, err, connect.CodeInvalidArgument, "main_commit")
	_, err = w.ReadChangesetConflicts(ctx, connect.NewRequest(&circusv1.ReadChangesetConflictsRequest{ChangesetId: b, MainCommit: checked.MainCommit, PageToken: "x"}))
	wantField(t, err, connect.CodeInvalidArgument, "page_token")
	c := open()
	check(c, content("D?a-Two.md", decisionFile("D?a", "Two", "two\n")))
	merge(c)
	_, err = w.ReadChangesetConflicts(ctx, connect.NewRequest(read))
	if got := preconditionSubjects(t, err); len(got) != 1 || !strings.HasPrefix(got[0], "MAIN_LINE_MOVED:") {
		t.Fatalf("read after the main line moved = %v", got)
	}
}

// TestChangesetRepairFailureReads: a changeset whose interrupted merge
// cannot be repaired is listed and got as recorded, with why, and its
// operations are refused with that reason; so is a discarded changeset
// whose archive cannot be finished. The project's other changesets read
// as usual.
func TestChangesetRepairFailureReads(t *testing.T) {
	h, owner := newHarness(t, withWorkers)
	ctx := t.Context()
	projectID := createLedgerProject(t, owner)
	w := h.worker(t, h.owner.ID, "w1", false)
	var commits []string
	var ids []string
	for i := range 3 {
		opened, err := w.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: "Ledger"}))
		if err != nil {
			t.Fatal(err)
		}
		id := opened.Msg.Changeset.ChangesetId
		ids = append(ids, id)
		if _, err := w.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id,
			Changes: []*circusv1.FileChange{content("D?a-New.md", decisionFile("D?a", "New", strings.Repeat("x", i)+"\n"))}})); err != nil {
			t.Fatal(err)
		}
		merged, err := w.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: id}))
		if err != nil {
			t.Fatal(err)
		}
		commits = append(commits, merged.Msg.Changeset.MergedCommit)
	}
	// An open changeset with a recorded merge whose commit never reached
	// the main line: finding that walks back to the first commit, past a
	// commit whose object is now missing.
	opened, err := w.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: "Ledger"}))
	if err != nil {
		t.Fatal(err)
	}
	stuck := opened.Msg.Changeset.ChangesetId
	if ok, err := h.st.PrepareLedgerMerge(ctx, stuck, store.LedgerMerge{Commit: strings.Repeat("e", 40), Parent: commits[0]}, h.clock.Load()); err != nil || !ok {
		t.Fatalf("prepare = %v, %v", ok, err)
	}
	objects := filepath.Join(h.dir, "ledger", projectID+".git", "objects")
	if err := os.Remove(filepath.Join(objects, commits[1][:2], commits[1][2:])); err != nil {
		t.Fatal(err)
	}
	// A discarded changeset whose branch is still to archive, while its
	// branch commit's object is missing.
	opened, err = w.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: "Ledger"}))
	if err != nil {
		t.Fatal(err)
	}
	unarchived := opened.Msg.Changeset.ChangesetId
	if _, err := w.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: unarchived,
		Changes: []*circusv1.FileChange{content("D?a-Gone.md", decisionFile("D?a", "Gone", "x\n"))}})); err != nil {
		t.Fatal(err)
	}
	branch, err := os.ReadFile(filepath.Join(h.dir, "ledger", projectID+".git", "refs", "heads", "changesets", unarchived))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := h.st.DiscardLedgerChangeset(ctx, unarchived, "dropped", false, h.clock.Load()); err != nil || !ok {
		t.Fatalf("discard = %v, %v", ok, err)
	}
	hash := strings.TrimSpace(string(branch))
	if err := os.Remove(filepath.Join(objects, hash[:2], hash[2:])); err != nil {
		t.Fatal(err)
	}

	page, err := w.ListChangesets(ctx, connect.NewRequest(&circusv1.ListChangesetsRequest{Project: "Ledger"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Msg.Changesets) != 5 {
		t.Fatalf("list = %v", page.Msg)
	}
	for _, c := range page.Msg.Changesets {
		switch c.ChangesetId {
		case stuck:
			if c.State != circusv1.Changeset_STATE_OPEN || !strings.Contains(c.RepairFailure, "could not be repaired") {
				t.Fatalf("stuck changeset listed as %v", c)
			}
		case unarchived:
			if c.State != circusv1.Changeset_STATE_DISCARDED || c.DiscardReason != "dropped" || !strings.Contains(c.RepairFailure, "could not be repaired") {
				t.Fatalf("unarchived changeset listed as %v", c)
			}
		default:
			if c.State != circusv1.Changeset_STATE_MERGED || c.RepairFailure != "" {
				t.Fatalf("other changeset listed as %v", c)
			}
		}
	}
	for id, state := range map[string]circusv1.Changeset_State{stuck: circusv1.Changeset_STATE_OPEN, unarchived: circusv1.Changeset_STATE_DISCARDED} {
		got, err := w.GetChangeset(ctx, connect.NewRequest(&circusv1.GetChangesetRequest{ChangesetId: id}))
		if err != nil || got.Msg.Changeset.State != state || !strings.Contains(got.Msg.Changeset.RepairFailure, "could not be repaired") {
			t.Fatalf("get of %s = %v, %v", id, got, err)
		}
	}
	if got, err := w.GetChangeset(ctx, connect.NewRequest(&circusv1.GetChangesetRequest{ChangesetId: ids[2]})); err != nil || got.Msg.Changeset.RepairFailure != "" {
		t.Fatalf("get of another changeset = %v, %v", got, err)
	}
	_, err = w.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: stuck}))
	if got := preconditionSubjects(t, err); len(got) != 1 || got[0] != "CHANGESET_REPAIR:"+stuck {
		t.Fatalf("check of the stuck changeset = %v", got)
	}
	_, err = w.DiscardChangeset(ctx, connect.NewRequest(&circusv1.DiscardChangesetRequest{ChangesetId: unarchived, Reason: "again"}))
	if got := preconditionSubjects(t, err); len(got) != 1 || got[0] != "CHANGESET_REPAIR:"+unarchived {
		t.Fatalf("discard of the unarchived changeset = %v", got)
	}
}

// TestChangesetLimitsAfterAdmission: conflict reads take the user's write
// slot like checks and merges, and an admitted request answered without
// reading its body is answered within about a second however long the
// body takes.
func TestChangesetLimitsAfterAdmission(t *testing.T) {
	h, _ := newHarness(t, withWorkers)
	ctx := t.Context()
	token := h.credential(t, store.KindWorker, h.owner.ID, "w-a", "a")
	a := h.changesets(t, token)
	h.holdWriteSlot(t, a, token)
	_, err := a.ReadChangesetConflicts(ctx, connect.NewRequest(&circusv1.ReadChangesetConflictsRequest{ChangesetId: "0190a0a0-0000-7000-8000-000000000000"}))
	if !busyRefusal(err) {
		t.Fatalf("conflict read while the user's write slot is held: %v", err)
	}
	// Admitted, then refused by Connect for its Content-Type before the
	// body is read.
	start := time.Now()
	odd := h.stallAs(t, circusv1connect.LedgerChangesetServiceGetChangesetProcedure, "Bearer "+token, "text/plain", []byte{0x0a})
	if !odd.finished(3*time.Second) || odd.status != http.StatusUnsupportedMediaType || time.Since(start) > 2*time.Second {
		t.Fatalf("stalled request of an unsupported type: answered %d after %v", odd.status, time.Since(start))
	}
}

// TestReadChangesetConflictsLargePage reads a conflict whose contents
// alone exceed a page, encoded as JSON with control characters, and
// refuses to read on once the changeset was checked again.
func TestReadChangesetConflictsLargePage(t *testing.T) {
	h, owner := newHarness(t, withWorkers)
	ctx := t.Context()
	createLedgerProject(t, owner)
	token := h.credential(t, store.KindWorker, h.owner.ID, "w1", "w1")
	w := h.changesets(t, token)
	asJSON := h.changesets(t, token, connect.WithProtoJSON())
	open := func() string {
		t.Helper()
		res, err := w.OpenChangeset(ctx, connect.NewRequest(&circusv1.OpenChangesetRequest{Project: "Ledger"}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg.Changeset.ChangesetId
	}
	check := func(id string, changes ...*circusv1.FileChange) *circusv1.CheckChangesetResponse {
		t.Helper()
		res, err := w.CheckChangeset(ctx, connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: id, Changes: changes}))
		if err != nil {
			t.Fatal(err)
		}
		return res.Msg
	}
	merge := func(id string) {
		t.Helper()
		if _, err := w.MergeChangeset(ctx, connect.NewRequest(&circusv1.MergeChangesetRequest{ChangesetId: id})); err != nil {
			t.Fatal(err)
		}
	}
	seed := open()
	check(seed, content("D?a-One.md", decisionFile("D?a", "One", "one\n")))
	merge(seed)
	a, b := open(), open()
	check(a, content("D0-One.md", decisionFile("D0", "One", "a\n")), content("D?a-Two.md", decisionFile("D?a", "Two", "two\n")))
	merge(a)
	// The index, which no changeset may change, of 2 MiB of control
	// characters: six JSON bytes each, beyond any page by itself.
	index := strings.Repeat("\x01", 2<<20)
	checked := check(b, content("D0-One.md", decisionFile("D0", "One", "b\n")), content("INDEX.md", index))
	if len(checked.Conflicts) != 2 || checked.Conflicts[0].ContentOmitted || !checked.Conflicts[1].ContentOmitted {
		t.Fatalf("check = %d conflicts, omitted %v %v", len(checked.Conflicts), checked.Conflicts[0].ContentOmitted, checked.Conflicts[1].ContentOmitted)
	}
	readAt := func(api circusv1connect.LedgerChangesetServiceClient, id, main, token string) (*circusv1.ReadChangesetConflictsResponse, error) {
		res, err := api.ReadChangesetConflicts(ctx, connect.NewRequest(&circusv1.ReadChangesetConflictsRequest{
			ChangesetId: id, MainCommit: main, PageToken: token}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	read := func(api circusv1connect.LedgerChangesetServiceClient, token string) (*circusv1.ReadChangesetConflictsResponse, error) {
		res, err := api.ReadChangesetConflicts(ctx, connect.NewRequest(&circusv1.ReadChangesetConflictsRequest{
			ChangesetId: b, MainCommit: checked.MainCommit, PageToken: token}))
		if err != nil {
			return nil, err
		}
		return res.Msg, nil
	}
	first, err := read(asJSON, "")
	if err != nil || len(first.Conflicts) != 1 || first.Conflicts[0].Path != "D0-One.md" || first.NextPageToken == "" {
		t.Fatalf("first page = %v, %v", first, err)
	}
	second, err := read(asJSON, first.NextPageToken)
	if err != nil || len(second.Conflicts) != 1 || second.Conflicts[0].Path != "INDEX.md" || second.Conflicts[0].Changeset != index || second.NextPageToken != "" {
		t.Fatalf("second page: %v", err)
	}
	// A token of one changeset does not read another's.
	other := open()
	_, err = readAt(w, other, checked.MainCommit, first.NextPageToken)
	wantField(t, err, connect.CodeInvalidArgument, "page_token")
	// The main line moves; checking the same content again keeps the
	// branch, and the token read against the old main line is refused
	// with the new one.
	c := open()
	check(c, content("D?a-Three.md", decisionFile("D?a", "Three", "three\n")))
	merge(c)
	rechecked := check(b, content("D0-One.md", decisionFile("D0", "One", "b\n")), content("INDEX.md", index))
	if rechecked.MainCommit == checked.MainCommit {
		t.Fatal("the main line did not move")
	}
	_, err = readAt(w, b, rechecked.MainCommit, first.NextPageToken)
	if got := preconditionSubjects(t, err); len(got) != 1 || !strings.HasPrefix(got[0], "MAIN_LINE_MOVED:") {
		t.Fatalf("old token with the new main line = %v", got)
	}
	if again, err := readAt(w, b, rechecked.MainCommit, ""); err != nil || len(again.Conflicts) != 1 || again.NextPageToken == "" {
		t.Fatalf("first page against the new main line = %v, %v", again, err)
	}
	// The changeset is checked again between the pages.
	checked = rechecked
	first, err = read(w, "")
	if err != nil {
		t.Fatal(err)
	}
	check(b, content("D0-One.md", decisionFile("D0", "One", "b again\n")), content("INDEX.md", index))
	_, err = read(w, first.NextPageToken)
	if got := preconditionSubjects(t, err); len(got) != 1 || got[0] != "CHANGESET_CHANGED:"+b {
		t.Fatalf("page after another check = %v", got)
	}
}
