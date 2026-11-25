package webapi

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/uuid"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

const (
	// A changeset request without files carries ids, a project name, a
	// discard reason of at most 1 KiB or a cursor: 16 KiB even when JSON
	// escapes every byte.
	maxChangesetSmallRequestBytes = 16 << 10
	// A check or merge carries at most 8 MiB of content in at most
	// MaxChanges files. Twice its binary size leaves room for JSON's
	// escaping of ordinary text; a client that escapes far more must send
	// binary.
	maxChangesetFilesRequestBytes = 2 * (ledger.MaxChangesetBytes + ledger.MaxChanges*(ledger.MaxChangePathBytes+16))
	// A base page holds files up to basePageBytes of encoded content; a
	// larger file comes alone, and only the generated index can be larger:
	// a row per decision with a title of at most five encoded bytes per
	// byte ('<' becomes "&#60;") and a directory that needs no escaping.
	basePageBytes        = 8 << 20
	maxIndexEncodedBytes = 1024 + store.MaxLedgerDecisions*(5*store.MaxLedgerTitleBytes+store.MaxLedgerDirBytes+64)
	maxBaseResponseBytes = max(basePageBytes, maxIndexEncodedBytes) + 64<<10
	// A conflicts page holds conflicts up to basePageBytes of encoded
	// contents; a larger conflict comes alone: two main-line versions of
	// at most the index's size and a changeset version of at most
	// MaxChangesetBytes, six encoded bytes per byte at worst.
	maxConflictsResponseBytes = max(basePageBytes, 2*maxIndexEncodedBytes+6*ledger.MaxChangesetBytes) + 64<<10
	// A repair failure is reported in at most this many bytes.
	maxRepairFailureBytes     = 256
	maxProblemEncodedBytes    = 2*ledger.MaxChangePathBytes + 6*600 + 64
	maxCheckResponseBytes     = ledger.MaxConflictBytes + ledger.MaxChanges*(2*ledger.MaxChangePathBytes+64) + (ledger.MaxProblems+ledger.MaxWarnings)*maxProblemEncodedBytes + 64<<10
	maxMergeResponseBytes     = store.MaxLedgerDecisions*32 + 64<<10
	maxChangesetListBytes     = store.MaxLedgerChangesetPage*(6*(store.MaxLedgerDiscardReasonBytes+maxRepairFailureBytes)+2048) + 4<<10
	maxChangesetSmallResponse = 6*(store.MaxLedgerDiscardReasonBytes+maxRepairFailureBytes) + 8<<10
	// Requests in flight, admitted before their bodies are read, in total
	// and per user; of them, checks, merges and conflict reads, which use
	// the most memory, also take one of the server's ledger write slots
	// (ledgerslots), which sessions' collections share. A user's share is
	// smaller than the whole, so one user's clients that stop sending or
	// reading cannot keep other users out; it takes the clients of two
	// users to hold every request slot. See the README's ledger section.
	maxChangesetRequests        = 4
	maxChangesetRequestsPerUser = 2
	changesetRequestTimeout     = time.Minute
	// A project name has at most 64 characters of up to 4 bytes; an id is
	// shorter.
	maxProjectRefBytes = 256
	// Reason of the PERMISSION_DENIED refusals that the grant would allow.
	integratorRequired = "INTEGRATOR_REQUIRED"
	// Reason of the RESOURCE_EXHAUSTED refusals that a retry may pass.
	ledgerBusy  = "LEDGER_BUSY"
	errorDomain = "circus"
)

type workerKey struct{}

// changesetService implements LedgerChangesetService for worker
// credentials.
type changesetService struct {
	*Service
	workers *auth.Authenticator
}

var _ circusv1connect.LedgerChangesetServiceHandler = (*changesetService)(nil)

func (s *Service) registerChangesets(mux *http.ServeMux) {
	if s.workers == nil || s.ledger == nil {
		return
	}
	cs := &changesetService{Service: s, workers: s.workers}
	slots := &ledgerSlots{byUser: make(map[string]int), limit: maxChangesetRequests, perUser: maxChangesetRequestsPerUser}
	writes := s.writes
	errs := connect.NewErrorWriter()
	for _, p := range []struct {
		path            string
		request, sendTo int
		// heavy requests also take a write slot.
		heavy bool
	}{
		{circusv1connect.LedgerChangesetServiceOpenChangesetProcedure, maxChangesetSmallRequestBytes, maxChangesetSmallResponse, false},
		{circusv1connect.LedgerChangesetServiceReadChangesetBaseProcedure, maxChangesetSmallRequestBytes, maxBaseResponseBytes, false},
		{circusv1connect.LedgerChangesetServiceCheckChangesetProcedure, maxChangesetFilesRequestBytes, maxCheckResponseBytes, true},
		{circusv1connect.LedgerChangesetServiceReadChangesetConflictsProcedure, maxChangesetSmallRequestBytes, maxConflictsResponseBytes, true},
		{circusv1connect.LedgerChangesetServiceMergeChangesetProcedure, maxChangesetFilesRequestBytes, maxMergeResponseBytes, true},
		{circusv1connect.LedgerChangesetServiceDiscardChangesetProcedure, maxChangesetSmallRequestBytes, maxChangesetSmallResponse, false},
		{circusv1connect.LedgerChangesetServiceListChangesetsProcedure, maxChangesetSmallRequestBytes, maxChangesetListBytes, false},
		{circusv1connect.LedgerChangesetServiceGetChangesetProcedure, maxChangesetSmallRequestBytes, maxChangesetSmallResponse, false},
	} {
		_, h := circusv1connect.NewLedgerChangesetServiceHandler(cs, connect.WithReadMaxBytes(p.request),
			connect.WithSendMaxBytes(p.sendTo), connect.WithCompression("gzip", nil, nil))
		mux.Handle(p.path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), changesetRequestTimeout)
			defer cancel()
			defer boundRequest(ctx, w)()
			// Authenticate and admit before Connect reads the body. A
			// refusal answers at once and closes the connection.
			principal, err := cs.workers.Authenticate(ctx, r)
			switch {
			case errors.Is(err, auth.ErrUnauthenticated), errors.Is(err, auth.ErrRevoked), err == nil && principal.Kind != store.KindWorker:
				refuse(errs, w, r, connect.NewError(connect.CodeUnauthenticated, errors.New("a worker credential is required")))
				return
			case err != nil:
				refuse(errs, w, r, s.internal(ctx, "authenticate worker", fmt.Errorf("%w: %w", store.ErrBusy, err)))
				return
			}
			if !slots.take(principal.UserID) {
				refuse(errs, w, r, busyError())
				return
			}
			defer slots.release(principal.UserID)
			held := &heldWrite{}
			if p.heavy {
				if !writes.TryTake(principal.UserID) {
					refuse(errs, w, r, busyError())
					return
				}
				held.release = func() { writes.Release(principal.UserID) }
				defer held.done()
			}
			limitBody(w, r, p.request)
			ctx = context.WithValue(context.WithValue(ctx, workerKey{}, principal), heldWriteKey{}, held)
			h.ServeHTTP(w, r.WithContext(ctx))
		}))
	}
}

// heldWrite is the write slot a request took at its admission. The
// operations on a session's changeset release it early: they wait for a
// slot themselves, after the session's lock (see ledgersnaps), and a slot
// held while waiting for that lock could hold up the session's collection
// that holds it.
type heldWrite struct {
	once    sync.Once
	release func()
}

type heldWriteKey struct{}

func (h *heldWrite) done() {
	if h.release != nil {
		h.once.Do(h.release)
	}
}

// releaseWrite releases the request's write slot, if it holds one.
func releaseWrite(ctx context.Context) {
	if h, ok := ctx.Value(heldWriteKey{}).(*heldWrite); ok {
		h.done()
	}
}

func worker(ctx context.Context) (auth.Principal, error) {
	p, ok := ctx.Value(workerKey{}).(auth.Principal)
	if !ok {
		return auth.Principal{}, connect.NewError(connect.CodeUnauthenticated, errors.New("a worker credential is required"))
	}
	return p, nil
}

// busyError refuses a request for want of a slot. Its ErrorInfo reason,
// "LEDGER_BUSY", tells clients to try again shortly, unlike the other
// RESOURCE_EXHAUSTED refusals, which a retry does not help.
func busyError() error {
	err := connect.NewError(connect.CodeResourceExhausted, errors.New("ledger is busy"))
	if detail, detailErr := connect.NewErrorDetail(&errdetails.ErrorInfo{Reason: ledgerBusy, Domain: errorDomain}); detailErr == nil {
		err.AddDetail(detail)
	}
	return err
}

func integratorError(message string) error {
	err := connect.NewError(connect.CodePermissionDenied, errors.New(message))
	if detail, detailErr := connect.NewErrorDetail(&errdetails.ErrorInfo{Reason: integratorRequired, Domain: errorDomain}); detailErr == nil {
		err.AddDetail(detail)
	}
	return err
}

// fail maps the ledger's and the store's errors.
func (cs *changesetService) fail(ctx context.Context, op string, err error) error {
	var connectErr *connect.Error
	var request *ledger.RequestError
	var limit *ledger.LimitError
	var precondition *ledger.PreconditionError
	var repair *ledger.RepairError
	switch {
	case errors.As(err, &connectErr):
		return connectErr
	case errors.As(err, &request):
		return invalidField(request.Field, request.Message)
	case errors.As(err, &limit):
		return connect.NewError(connect.CodeResourceExhausted, errors.New(limit.Message))
	case errors.As(err, &precondition):
		out := connect.NewError(connect.CodeFailedPrecondition, errors.New(precondition.Error()))
		failure := &errdetails.PreconditionFailure{}
		for _, v := range precondition.Violations {
			failure.Violations = append(failure.Violations, &errdetails.PreconditionFailure_Violation{Type: v.Type, Subject: v.Subject, Description: v.Description})
		}
		if detail, detailErr := connect.NewErrorDetail(failure); detailErr == nil {
			out.AddDetail(detail)
		}
		return out
	case errors.As(err, &repair):
		return repairRefusal(repair)
	case errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, errors.New("changeset or project not found"))
	case errors.Is(err, store.ErrLimit):
		return connect.NewError(connect.CodeResourceExhausted, fmt.Errorf("a project has at most %d open changesets", store.MaxOpenLedgerChangesets))
	}
	return cs.internal(ctx, op, err)
}

// project resolves a project id or one of the user's project names.
func (cs *changesetService) project(ctx context.Context, userID, ref string) (string, error) {
	if ref == "" || len(ref) > maxProjectRefBytes {
		return "", invalidField("project", "project must be a project id or name")
	}
	if ledger.ValidProjectID(ref) {
		_, err := cs.st.GetProject(ctx, userID, ref)
		if err == nil {
			return ref, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", cs.internal(ctx, "read ledger project", err)
		}
	}
	projects, err := cs.st.ListProjects(ctx, userID)
	if err != nil {
		return "", cs.internal(ctx, "list projects", err)
	}
	key := store.ProjectNameKey(ref)
	for _, p := range projects {
		if store.ProjectNameKey(p.Name) == key {
			return p.ID, nil
		}
	}
	return "", connect.NewError(connect.CodeNotFound, errProjectNotFound)
}

// changeset reads one of the user's changesets.
func (cs *changesetService) changeset(ctx context.Context, userID, id string) (store.LedgerChangeset, error) {
	if !ledger.ValidChangesetID(id) {
		return store.LedgerChangeset{}, invalidField("changeset_id", "invalid changeset id")
	}
	c, err := cs.st.GetLedgerChangeset(ctx, id)
	if err == nil {
		_, err = cs.st.GetProject(ctx, userID, c.ProjectID)
	}
	if errors.Is(err, store.ErrNotFound) {
		return store.LedgerChangeset{}, connect.NewError(connect.CodeNotFound, errors.New("changeset not found"))
	}
	if err != nil {
		return store.LedgerChangeset{}, cs.internal(ctx, "read changeset", err)
	}
	return c, nil
}

// integrator reads the worker's grant now.
func (cs *changesetService) integrator(ctx context.Context, p auth.Principal) (bool, error) {
	w, err := cs.st.GetIdentity(ctx, store.KindWorker, p.ID)
	if err != nil {
		return false, cs.internal(ctx, "read worker grant", err)
	}
	return w.Integrator && w.DeletedAt == 0, nil
}

// mayChange lets the opener or an integrator check, discard or merge; a
// session's changeset has no opener.
func (cs *changesetService) mayChange(ctx context.Context, p auth.Principal, c store.LedgerChangeset) error {
	if c.SessionID == "" && c.OpenerWorkerID == p.ID {
		return nil
	}
	ok, err := cs.integrator(ctx, p)
	if err != nil || ok {
		return err
	}
	if c.SessionID != "" {
		return integratorError("a session's changeset may be changed only by a worker with the integrator grant")
	}
	return integratorError("only the worker that opened the changeset, or a worker with the integrator grant, may change it")
}

// sessionChangeset reports a session's changeset, which the snapshot
// manager handles; without one (no ledger snapshots) it is refused.
func (cs *changesetService) sessionChangeset(c store.LedgerChangeset) (bool, error) {
	if c.SessionID == "" {
		return false, nil
	}
	if cs.snapshots == nil {
		return true, connect.NewError(connect.CodeFailedPrecondition, errors.New("this server keeps no session ledger snapshots"))
	}
	return true, nil
}

func (cs *changesetService) workerName(ctx context.Context, id string) string {
	if w, err := cs.st.GetIdentity(ctx, store.KindWorker, id); err == nil {
		return w.Name
	}
	return "worker " + id
}

// repairFailure says briefly why a changeset's repair failed.
func repairFailure(err error) string {
	msg := "its interrupted merge or archive could not be repaired: " + err.Error()
	if len(msg) > maxRepairFailureBytes {
		msg = strings.ToValidUTF8(msg[:maxRepairFailureBytes-3], "") + "..."
	}
	return msg
}

// needsRepair reports a changeset with a merge in progress, or discarded
// with its branch still to archive.
func needsRepair(c store.LedgerChangeset) bool {
	return c.Pending() || c.State == store.LedgerChangesetDiscarded && !c.Archived
}

// withRepairFailure sets why the changeset's repair failed, if it did.
func withRepairFailure(m *circusv1.Changeset, err error) *circusv1.Changeset {
	if err != nil {
		var repair *ledger.RepairError
		if errors.As(err, &repair) {
			err = repair.Err
		}
		m.RepairFailure = repairFailure(err)
	}
	return m
}

// repairRefusal refuses an operation on a changeset whose repair failed.
func repairRefusal(e *ledger.RepairError) error {
	reason := repairFailure(e.Err)
	out := connect.NewError(connect.CodeFailedPrecondition, fmt.Errorf("changeset %s cannot be used: %s", e.ID, reason))
	failure := &errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{{
		Type: "CHANGESET_REPAIR", Subject: e.ID, Description: reason}}}
	if detail, err := connect.NewErrorDetail(failure); err == nil {
		out.AddDetail(detail)
	}
	return out
}

func changesetMessage(c store.LedgerChangeset) *circusv1.Changeset {
	out := &circusv1.Changeset{ChangesetId: c.ID, ProjectId: c.ProjectID, BaseCommit: c.BaseCommit, OpenerWorkerId: c.OpenerWorkerID,
		CreatedAtUnixMs: c.CreatedAt, UpdatedAtUnixMs: c.UpdatedAt, SessionId: c.SessionID}
	switch c.State {
	case store.LedgerChangesetOpen:
		out.State = circusv1.Changeset_STATE_OPEN
	case store.LedgerChangesetMerged:
		out.State = circusv1.Changeset_STATE_MERGED
		out.MergedCommit, out.CodeCommit = c.Merge.Commit, c.Merge.CodeCommit
	case store.LedgerChangesetDiscarded:
		out.State = circusv1.Changeset_STATE_DISCARDED
		out.DiscardReason = c.DiscardReason
	}
	return out
}

func (cs *changesetService) OpenChangeset(ctx context.Context, req *connect.Request[circusv1.OpenChangesetRequest]) (*connect.Response[circusv1.OpenChangesetResponse], error) {
	p, err := worker(ctx)
	if err != nil {
		return nil, err
	}
	projectID, err := cs.project(ctx, p.UserID, req.Msg.GetProject())
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, cs.internal(ctx, "changeset id", err)
	}
	c, err := cs.ledger.OpenChangeset(ctx, projectID, id.String(), p.ID)
	if err != nil {
		return nil, cs.fail(ctx, "open changeset", err)
	}
	return connect.NewResponse(&circusv1.OpenChangesetResponse{Changeset: changesetMessage(c)}), nil
}

func (cs *changesetService) ReadChangesetBase(ctx context.Context, req *connect.Request[circusv1.ReadChangesetBaseRequest]) (*connect.Response[circusv1.ReadChangesetBaseResponse], error) {
	p, err := worker(ctx)
	if err != nil {
		return nil, err
	}
	c, err := cs.changeset(ctx, p.UserID, req.Msg.GetChangesetId())
	if err != nil {
		return nil, err
	}
	after, ok := decodePathCursor(req.Msg.GetPageToken(), c.ID)
	if !ok {
		return nil, invalidField("page_token", "invalid base page token")
	}
	files, more, err := cs.ledger.ReadChangesetBase(ctx, c, after, basePageBytes)
	if err != nil {
		return nil, cs.fail(ctx, "read changeset base", err)
	}
	out := &circusv1.ReadChangesetBaseResponse{Files: make([]*circusv1.LedgerFile, 0, len(files))}
	for _, f := range files {
		out.Files = append(out.Files, &circusv1.LedgerFile{Path: f.Path, Content: string(f.Content)})
	}
	if more && len(files) > 0 {
		out.NextPageToken = encodePathCursor(c.ID, files[len(files)-1].Path)
	}
	return connect.NewResponse(out), nil
}

func changes(field string, in []*circusv1.FileChange) ([]ledger.Change, error) {
	out := make([]ledger.Change, 0, len(in))
	for i, ch := range in {
		switch v := ch.GetChange().(type) {
		case *circusv1.FileChange_Content:
			out = append(out, ledger.Change{Path: ch.GetPath(), Content: []byte(v.Content)})
		case *circusv1.FileChange_Deleted:
			if !v.Deleted {
				return nil, invalidField(fmt.Sprintf("%s[%d].deleted", field, i), "deleted must be true when set")
			}
			out = append(out, ledger.Change{Path: ch.GetPath(), Deleted: true})
		default:
			return nil, invalidField(fmt.Sprintf("%s[%d]", field, i), "a change needs content or deleted")
		}
	}
	return out, nil
}

func (cs *changesetService) CheckChangeset(ctx context.Context, req *connect.Request[circusv1.CheckChangesetRequest]) (*connect.Response[circusv1.CheckChangesetResponse], error) {
	p, err := worker(ctx)
	if err != nil {
		return nil, err
	}
	c, err := cs.changeset(ctx, p.UserID, req.Msg.GetChangesetId())
	if err != nil {
		return nil, err
	}
	if err := cs.mayChange(ctx, p, c); err != nil {
		return nil, err
	}
	in, err := changes("changes", req.Msg.GetChanges())
	if err != nil {
		return nil, err
	}
	var res ledger.CheckResult
	var problems []*circusv1.LedgerProblem
	var warning string
	if session, err := cs.sessionChangeset(c); err != nil {
		return nil, err
	} else if session {
		if len(in) > 0 {
			return nil, invalidField("changes", "a session's changeset takes its changes from the session's snapshot; send none")
		}
		releaseWrite(ctx)
		check, err := cs.snapshots.CheckChangeset(ctx, c)
		if err != nil {
			return nil, cs.fail(ctx, "check session changeset", err)
		}
		res, problems, warning = check.CheckResult, problemMessages(check.Problems), check.Warning
	} else {
		if res, err = cs.ledger.CheckChangeset(ctx, c.ID, cs.workerName(ctx, c.OpenerWorkerID), in); err != nil {
			return nil, cs.fail(ctx, "check changeset", err)
		}
		for _, problem := range res.Problems {
			problems = append(problems, &circusv1.LedgerProblem{Path: problem.Path, Message: problem.Message})
		}
	}
	out := &circusv1.CheckChangesetResponse{Changeset: changesetMessage(res.Changeset), FastForward: res.FastForward, MainCommit: res.MainCommit,
		Problems: problems}
	if warning != "" {
		out.Warnings = append(out.Warnings, &circusv1.LedgerProblem{Path: ".", Message: warning})
	}
	for _, warning := range res.Warnings {
		out.Warnings = append(out.Warnings, &circusv1.LedgerProblem{Path: warning.Path, Message: warning.Message})
	}
	for _, cf := range res.Conflicts {
		out.Conflicts = append(out.Conflicts, &circusv1.LedgerConflict{Path: cf.Path,
			BaseExists: cf.Base.Exists, Base: string(cf.Base.Content),
			ChangesetExists: cf.Changeset.Exists, Changeset: string(cf.Changeset.Content),
			MainExists: cf.Main.Exists, Main: string(cf.Main.Content), ContentOmitted: cf.Omitted})
	}
	return connect.NewResponse(out), nil
}

func (cs *changesetService) ReadChangesetConflicts(ctx context.Context, req *connect.Request[circusv1.ReadChangesetConflictsRequest]) (*connect.Response[circusv1.ReadChangesetConflictsResponse], error) {
	p, err := worker(ctx)
	if err != nil {
		return nil, err
	}
	c, err := cs.changeset(ctx, p.UserID, req.Msg.GetChangesetId())
	if err != nil {
		return nil, err
	}
	if err := cs.mayChange(ctx, p, c); err != nil {
		return nil, err
	}
	main := req.Msg.GetMainCommit()
	if main != "" && !store.IsLedgerCommitID(main) {
		return nil, invalidField("main_commit", "main commit must be 40 lowercase hexadecimal characters")
	}
	branch, tokenMain, after, ok := decodeConflictCursor(req.Msg.GetPageToken(), c.ID)
	if !ok {
		return nil, invalidField("page_token", "invalid conflicts page token")
	}
	if req.Msg.GetPageToken() != "" && tokenMain != main {
		// The pages before were read against another main line.
		return nil, cs.fail(ctx, "read changeset conflicts", &ledger.PreconditionError{Violations: []ledger.Violation{{
			Type: ledger.ViolationMainMoved, Subject: main,
			Description: "the page token was read against another main-line commit; check again and read from the first page"}}})
	}
	page, err := cs.ledger.ReadChangesetConflicts(ctx, c.ID, main, branch, after, basePageBytes)
	if err != nil {
		return nil, cs.fail(ctx, "read changeset conflicts", err)
	}
	conflicts := page.Conflicts
	out := &circusv1.ReadChangesetConflictsResponse{Conflicts: make([]*circusv1.LedgerConflict, 0, len(conflicts))}
	for _, cf := range conflicts {
		out.Conflicts = append(out.Conflicts, &circusv1.LedgerConflict{Path: cf.Path,
			BaseExists: cf.Base.Exists, Base: string(cf.Base.Content),
			ChangesetExists: cf.Changeset.Exists, Changeset: string(cf.Changeset.Content),
			MainExists: cf.Main.Exists, Main: string(cf.Main.Content)})
	}
	if page.More && len(conflicts) > 0 {
		out.NextPageToken = encodeConflictCursor(c.ID, page.Branch, main, conflicts[len(conflicts)-1].Path)
	}
	return connect.NewResponse(out), nil
}

func (cs *changesetService) MergeChangeset(ctx context.Context, req *connect.Request[circusv1.MergeChangesetRequest]) (*connect.Response[circusv1.MergeChangesetResponse], error) {
	p, err := worker(ctx)
	if err != nil {
		return nil, err
	}
	c, err := cs.changeset(ctx, p.UserID, req.Msg.GetChangesetId())
	if err != nil {
		return nil, err
	}
	if err := cs.mayChange(ctx, p, c); err != nil {
		return nil, err
	}
	code := req.Msg.GetCodeCommit()
	if code != "" && !store.IsLedgerCommitID(code) {
		return nil, invalidField("code_commit", "code commit must be 40 lowercase hexadecimal characters")
	}
	resolved, err := changes("resolved", req.Msg.GetResolved())
	if err != nil {
		return nil, err
	}
	o := ledger.MergeOptions{Author: p.Name, CodeCommit: code, Resolved: resolved,
		Authorize: func(ctx context.Context, fastForward bool) error {
			if fastForward {
				return nil
			}
			ok, err := cs.integrator(ctx, p)
			if err != nil || ok {
				return err
			}
			return integratorError("the changeset does not fast-forward: its base differs from the main line's head, so only a worker with the integrator grant may merge it")
		}}
	var merged store.LedgerChangeset
	if session, err := cs.sessionChangeset(c); err != nil {
		return nil, err
	} else if session {
		releaseWrite(ctx)
		merged, err = cs.snapshots.MergeChangeset(ctx, p.UserID, c, o)
		if err != nil {
			return nil, cs.fail(ctx, "merge session changeset", err)
		}
	} else if merged, err = cs.ledger.MergeChangeset(ctx, c.ID, o); err != nil {
		return nil, cs.fail(ctx, "merge changeset", err)
	}
	out := &circusv1.MergeChangesetResponse{Changeset: changesetMessage(merged), Numbers: make(map[string]uint32, len(merged.Merge.Numbers))}
	for letters, n := range merged.Merge.Numbers {
		out.Numbers["D?"+letters] = n
	}
	return connect.NewResponse(out), nil
}

func (cs *changesetService) DiscardChangeset(ctx context.Context, req *connect.Request[circusv1.DiscardChangesetRequest]) (*connect.Response[circusv1.DiscardChangesetResponse], error) {
	p, err := worker(ctx)
	if err != nil {
		return nil, err
	}
	c, err := cs.changeset(ctx, p.UserID, req.Msg.GetChangesetId())
	if err != nil {
		return nil, err
	}
	reason := req.Msg.GetReason()
	if strings.TrimSpace(reason) == "" || len(reason) > store.MaxLedgerDiscardReasonBytes || !utf8.ValidString(reason) || strings.ContainsRune(reason, 0) {
		return nil, invalidField("reason", fmt.Sprintf("reason is required: 1 to %d bytes of text", store.MaxLedgerDiscardReasonBytes))
	}
	if err := cs.mayChange(ctx, p, c); err != nil {
		return nil, err
	}
	var discarded store.LedgerChangeset
	if session, err := cs.sessionChangeset(c); err != nil {
		return nil, err
	} else if session {
		discarded, err = cs.snapshots.DiscardChangeset(ctx, c, reason, p.Name)
		if err != nil {
			return nil, cs.fail(ctx, "discard session changeset", err)
		}
	} else if discarded, err = cs.ledger.DiscardChangeset(ctx, c.ID, reason, p.Name); err != nil {
		return nil, cs.fail(ctx, "discard changeset", err)
	}
	return connect.NewResponse(&circusv1.DiscardChangesetResponse{Changeset: changesetMessage(discarded)}), nil
}

func (cs *changesetService) ListChangesets(ctx context.Context, req *connect.Request[circusv1.ListChangesetsRequest]) (*connect.Response[circusv1.ListChangesetsResponse], error) {
	p, err := worker(ctx)
	if err != nil {
		return nil, err
	}
	projectID, err := cs.project(ctx, p.UserID, req.Msg.GetProject())
	if err != nil {
		return nil, err
	}
	f := store.LedgerChangesetFilter{ProjectID: projectID, Limit: int(req.Msg.GetPageSize()), SessionID: req.Msg.GetSessionId()}
	if len(f.SessionID) > store.MaxIDBytes {
		return nil, invalidField("session_id", "session id is too long")
	}
	switch req.Msg.GetState() {
	case circusv1.Changeset_STATE_UNSPECIFIED:
	case circusv1.Changeset_STATE_OPEN:
		f.State = store.LedgerChangesetOpen
	case circusv1.Changeset_STATE_MERGED:
		f.State = store.LedgerChangesetMerged
	case circusv1.Changeset_STATE_DISCARDED:
		f.State = store.LedgerChangesetDiscarded
	default:
		return nil, invalidField("state", "unknown changeset state")
	}
	if f.Limit > store.MaxLedgerChangesetPage {
		return nil, invalidField("page_size", "page size must be at most 100")
	}
	if f.Limit == 0 {
		f.Limit = 50
	}
	if token := req.Msg.GetPageToken(); token != "" {
		after, ok := decodeChangesetCursor(token, projectID)
		if !ok {
			return nil, invalidField("page_token", "invalid changeset page token")
		}
		f.After = &after
	}
	page, more, err := cs.st.ListLedgerChangesets(ctx, f)
	if err != nil {
		return nil, cs.internal(ctx, "list changesets", err)
	}
	// A merge in progress may have reached the main line: such a row is
	// merged or open only once it is repaired, and a discarded one may
	// still have to be archived. One that cannot be repaired is listed as
	// recorded, with why.
	var failed map[string]error
	if slices.ContainsFunc(page, needsRepair) {
		if failed, err = cs.ledger.RepairChangesets(ctx, projectID); err != nil {
			return nil, cs.fail(ctx, "repair changesets", err)
		}
		if page, more, err = cs.st.ListLedgerChangesets(ctx, f); err != nil {
			return nil, cs.internal(ctx, "list changesets", err)
		}
	}
	out := &circusv1.ListChangesetsResponse{Changesets: make([]*circusv1.Changeset, 0, len(page))}
	for _, c := range page {
		out.Changesets = append(out.Changesets, withRepairFailure(changesetMessage(c), failed[c.ID]))
	}
	if more {
		last := page[len(page)-1]
		out.NextPageToken = encodeChangesetCursor(projectID, store.LedgerChangesetCursor{CreatedAt: last.CreatedAt, ID: last.ID})
	}
	return connect.NewResponse(out), nil
}

func (cs *changesetService) GetChangeset(ctx context.Context, req *connect.Request[circusv1.GetChangesetRequest]) (*connect.Response[circusv1.GetChangesetResponse], error) {
	p, err := worker(ctx)
	if err != nil {
		return nil, err
	}
	c, err := cs.changeset(ctx, p.UserID, req.Msg.GetChangesetId())
	if err != nil {
		return nil, err
	}
	var failed map[string]error
	if needsRepair(c) {
		// As for List: repaired first, or returned as recorded with why.
		if failed, err = cs.ledger.RepairChangesets(ctx, c.ProjectID); err != nil {
			return nil, cs.fail(ctx, "repair changeset", err)
		}
		if c, err = cs.changeset(ctx, p.UserID, c.ID); err != nil {
			return nil, err
		}
	}
	return connect.NewResponse(&circusv1.GetChangesetResponse{Changeset: withRepairFailure(changesetMessage(c), failed[c.ID])}), nil
}

// A path cursor names the last path of a base page, bound to its
// changeset: version, the id's length, the id, the path.
func encodePathCursor(id, path string) string {
	b := append([]byte{1, byte(len(id))}, id...)
	return base64.RawURLEncoding.EncodeToString(append(b, path...))
}

func decodePathCursor(token, id string) (string, bool) {
	if token == "" {
		return "", true
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(b) < 2+len(id) || b[0] != 1 || int(b[1]) != len(id) || string(b[2:2+len(id)]) != id {
		return "", false
	}
	path := string(b[2+len(id):])
	return path, path != "" && len(path) <= store.MaxLedgerPathBytes+len("INDEX.md") && utf8.ValidString(path)
}

// A conflict cursor names the last path of a conflicts page, bound to its
// changeset, the branch commit the page was read from and the main-line
// commit it was compared with: version, the id's length, the id, the two
// commits' 20 bytes each, the path.
func encodeConflictCursor(id, branch, main, path string) string {
	b := append([]byte{3, byte(len(id))}, id...)
	for _, commit := range []string{branch, main} {
		hash := plumbing.NewHash(commit)
		b = append(b, hash[:]...)
	}
	return base64.RawURLEncoding.EncodeToString(append(b, path...))
}

func decodeConflictCursor(token, id string) (branch, main, path string, ok bool) {
	if token == "" {
		return "", "", "", true
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(token)
	const hashBytes = 20
	if err != nil || len(b) < 2+len(id)+2*hashBytes || b[0] != 3 || int(b[1]) != len(id) || string(b[2:2+len(id)]) != id {
		return "", "", "", false
	}
	rest := b[2+len(id):]
	branch, main, path = hex.EncodeToString(rest[:hashBytes]), hex.EncodeToString(rest[hashBytes:2*hashBytes]), string(rest[2*hashBytes:])
	zero := strings.Repeat("0", 40)
	if main == zero {
		main = ""
	}
	return branch, main, path, path != "" && len(path) <= ledger.MaxChangePathBytes && utf8.ValidString(path) && branch != zero
}

// A changeset cursor holds the last row's creation time and id, bound to
// the project: version, time, the id's length, the id, the project id.
func encodeChangesetCursor(projectID string, c store.LedgerChangesetCursor) string {
	b := binary.BigEndian.AppendUint64([]byte{1}, uint64(c.CreatedAt))
	b = append(append(b, byte(len(c.ID))), c.ID...)
	return base64.RawURLEncoding.EncodeToString(append(b, projectID...))
}

func decodeChangesetCursor(token, projectID string) (store.LedgerChangesetCursor, bool) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(b) < 10 || b[0] != 1 {
		return store.LedgerChangesetCursor{}, false
	}
	n := int(b[9])
	if len(b) != 10+n+len(projectID) || string(b[10+n:]) != projectID {
		return store.LedgerChangesetCursor{}, false
	}
	c := store.LedgerChangesetCursor{CreatedAt: int64(binary.BigEndian.Uint64(b[1:9])), ID: string(b[10 : 10+n])}
	return c, ledger.ValidChangesetID(c.ID)
}
