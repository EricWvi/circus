package webapi

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
)

const (
	// A request carries a project ID and either a commit or a URL-safe
	// cursor. Even escaping every byte of the ID and the cursor in JSON
	// takes less than 3 KiB with the numeric fields and framing.
	maxLedgerRequestBytes = 4 << 10
	// A full GetLedger has at most 4096 entries with three strings.
	// Charge six JSON bytes per byte, plus fields and a quoted time.
	maxLedgerListingBytes = store.MaxLedgerDecisions*(6*(store.MaxLedgerTitleBytes+store.MaxLedgerDirBytes+store.MaxLedgerPathBytes)+128) + 256
	// Decision bodies fit one file. History supplies at most 100 versions,
	// each with bounded author, summary, path, title and two commit IDs.
	maxLedgerDecisionBytes = 6*(ledger.MaxDecisionFileBytes+store.MaxLedgerDirBytes+store.MaxLedgerPathBytes) + 256
	maxLedgerHistoryBytes  = store.MaxLedgerHistoryPage*(6*(store.MaxLedgerAuthorBytes+store.MaxLedgerSummaryBytes+
		store.MaxLedgerPathBytes+store.MaxLedgerTitleBytes)+256) + 512
	// Responses in flight, each held until it is written, bound memory.
	// One user may hold only some of them, so one user's clients that stop
	// reading their responses cannot keep other users out; it takes the
	// clients of two users together.
	maxLedgerRequests        = 4
	maxLedgerRequestsPerUser = 2
	ledgerRequestTimeout     = 30 * time.Second
)

type ledgerAdmissionKey struct{}

// ledgerSlots counts the ledger requests in flight, in total and per user,
// up to limit and perUser.
type ledgerSlots struct {
	mu             sync.Mutex
	limit, perUser int
	total          int
	byUser         map[string]int
}

func (l *ledgerSlots) take(userID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.total == l.limit || l.byUser[userID] == l.perUser {
		return false
	}
	l.total++
	l.byUser[userID]++
	return true
}

func (l *ledgerSlots) release(userID string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.total--
	if l.byUser[userID]--; l.byUser[userID] == 0 {
		delete(l.byUser, userID)
	}
}

// unreadBodyGrace bounds how long the server waits, after the handler, for
// the part of a request body the handler left unread.
const unreadBodyGrace = time.Second

// boundRequest bounds the request's reads and writes by ctx's deadline
// and returns what to defer. After the handler the read deadline stays
// near: net/http reads up to 256 KiB of a body the handler left unread
// before it answers, and a client that stopped sending must not hold the
// response and the connection for that. The next request on the
// connection gets its own read deadline from the server.
func boundRequest(ctx context.Context, w http.ResponseWriter) func() {
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(deadline)
	_ = controller.SetWriteDeadline(deadline)
	return func() {
		_ = controller.SetReadDeadline(time.Now().Add(unreadBodyGrace))
		_ = controller.SetWriteDeadline(time.Time{})
	}
}

// limitBody stops the request's body a little past limit, the largest
// message the procedure reads (the rest covers a gRPC envelope). Connect
// reads the rest of a body over its limit to keep the connection; a body
// that keeps coming, or stops, would hold the request until its deadline.
// Past this, reads fail at once and the connection closes after the
// answer.
//
// Connect also closes the body in the handler, and closing a body not
// read to its end reads up to 256 KiB more of it, under the request's
// deadline. The server closes the body itself once the answer is sent,
// under the short deadline boundRequest leaves, so the handler's close
// does nothing.
func limitBody(w http.ResponseWriter, r *http.Request, limit int) {
	r.Body = serverClosedBody{http.MaxBytesReader(w, r.Body, int64(limit)+16)}
}

// serverClosedBody leaves closing a request body to the server.
type serverClosedBody struct{ io.Reader }

func (serverClosedBody) Close() error { return nil }

// refuse answers a request before its body is read and closes the
// connection afterwards, so the answer does not wait for the body.
func refuse(errs *connect.ErrorWriter, w http.ResponseWriter, r *http.Request, err error) {
	w.Header().Set("Connection", "close")
	_ = errs.Write(w, r, err)
}

func (s *Service) registerLedger(mux *http.ServeMux) {
	slots := &ledgerSlots{byUser: make(map[string]int), limit: maxLedgerRequests, perUser: maxLedgerRequestsPerUser}
	admit := connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
		return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
			// Connect decodes the body before interceptors run. The browser
			// interceptor has already resolved the cookie and checked Origin.
			u, err := user(ctx)
			if err != nil {
				return nil, err
			}
			if !slots.take(u.ID) {
				return nil, connect.NewError(connect.CodeResourceExhausted, errors.New("ledger is busy"))
			}
			*ctx.Value(ledgerAdmissionKey{}).(*string) = u.ID
			return next(ctx, req)
		}
	})
	for _, procedure := range []struct {
		path  string
		limit int
	}{
		{circusv1connect.LedgerServiceGetLedgerProcedure, maxLedgerListingBytes},
		{circusv1connect.LedgerServiceGetDecisionProcedure, maxLedgerDecisionBytes},
		{circusv1connect.LedgerServiceListDecisionHistoryProcedure, maxLedgerHistoryBytes},
	} {
		_, h := circusv1connect.NewLedgerServiceHandler(s, connect.WithReadMaxBytes(maxLedgerRequestBytes),
			connect.WithSendMaxBytes(procedure.limit), connect.WithInterceptors(s.interceptor, admit),
			connect.WithCompression("gzip", nil, nil))
		mux.Handle(procedure.path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), ledgerRequestTimeout)
			defer cancel()
			defer boundRequest(ctx, w)()
			limitBody(w, r, maxLedgerRequestBytes)
			// Release only after Connect has encoded and sent the response.
			var admitted string
			defer func() {
				if admitted != "" {
					slots.release(admitted)
				}
			}()
			h.ServeHTTP(w, r.WithContext(context.WithValue(ctx, ledgerAdmissionKey{}, &admitted)))
		}))
	}
}

func (s *Service) ledgerProject(ctx context.Context, id string) error {
	u, err := user(ctx)
	if err != nil {
		return err
	}
	if !ledger.ValidProjectID(id) {
		return invalidField("project_id", "invalid project id")
	}
	_, err = s.st.GetProject(ctx, u.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		return connect.NewError(connect.CodeNotFound, errProjectNotFound)
	}
	if err != nil {
		return s.internal(ctx, "read ledger project", err)
	}
	if s.ledger != nil {
		if err := s.ledger.CheckProject(ctx, id); err != nil {
			return s.internal(ctx, "read ledger project", err)
		}
	}
	return nil
}

func (s *Service) GetLedger(ctx context.Context, req *connect.Request[circusv1.GetLedgerRequest]) (*connect.Response[circusv1.GetLedgerResponse], error) {
	id := req.Msg.GetProjectId()
	if err := s.ledgerProject(ctx, id); err != nil {
		return nil, err
	}
	index, err := s.st.GetLedgerIndex(ctx, id)
	if err != nil {
		return nil, s.internal(ctx, "read ledger index", err)
	}
	out := &circusv1.GetLedgerResponse{HeadCommit: index.Head, NextNumber: index.NextNumber}
	out.Entries = make([]*circusv1.LedgerEntry, 0, len(index.Entries))
	for _, e := range index.Entries {
		out.Entries = append(out.Entries, &circusv1.LedgerEntry{
			Number: e.Number, Title: e.Title, Dir: e.Dir, Path: e.Path, UpdatedAtUnixMs: e.CommittedAt,
		})
	}
	return connect.NewResponse(out), nil
}

func (s *Service) GetDecision(ctx context.Context, req *connect.Request[circusv1.GetDecisionRequest]) (*connect.Response[circusv1.GetDecisionResponse], error) {
	id, number, commit := req.Msg.GetProjectId(), req.Msg.GetNumber(), req.Msg.GetCommit()
	if err := s.ledgerProject(ctx, id); err != nil {
		return nil, err
	}
	if number > store.MaxLedgerNumber {
		return nil, invalidField("number", "number is out of range")
	}
	if commit != "" && !store.IsLedgerCommitID(commit) {
		return nil, invalidField("commit", "commit must be 40 lowercase hexadecimal characters")
	}
	if commit == "" {
		var err error
		commit, err = s.st.GetLedgerHead(ctx, id)
		if err != nil {
			return nil, s.internal(ctx, "read ledger head", err)
		}
	}
	missing := connect.NewError(connect.CodeNotFound, errors.New("decision or main-line commit not found"))
	if commit == "" || s.ledger == nil {
		return nil, missing
	}
	d, err := s.ledger.GetDecision(ctx, id, number, commit)
	if errors.Is(err, store.ErrNotFound) {
		return nil, missing
	}
	if err != nil {
		return nil, s.internal(ctx, "read ledger decision", fmt.Errorf("project %q at %s: %w", id, commit, err))
	}
	return connect.NewResponse(&circusv1.GetDecisionResponse{Decision: &circusv1.Decision{
		Number: d.Number, Title: d.Title, Dir: d.Dir, Path: d.Path, Body: string(d.Body),
		Commit: d.Commit, CommittedAtUnixMs: d.CommittedAt,
	}}), nil
}

func (s *Service) ListDecisionHistory(ctx context.Context, req *connect.Request[circusv1.ListDecisionHistoryRequest]) (*connect.Response[circusv1.ListDecisionHistoryResponse], error) {
	id, number := req.Msg.GetProjectId(), req.Msg.GetNumber()
	if err := s.ledgerProject(ctx, id); err != nil {
		return nil, err
	}
	if number > store.MaxLedgerNumber {
		return nil, invalidField("number", "number is out of range")
	}
	size := req.Msg.GetPageSize()
	if size > store.MaxLedgerHistoryPage {
		return nil, invalidField("page_size", "page size must be at most 100")
	}
	if size == 0 {
		size = 50
	}
	after, ok := decodeLedgerCursor(req.Msg.GetPageToken(), id, number)
	if !ok {
		return nil, invalidField("page_token", "invalid ledger history cursor")
	}
	versions, more, err := s.st.ListLedgerHistory(ctx, id, number, after, int(size))
	if errors.Is(err, store.ErrLedgerCursor) {
		return nil, invalidField("page_token", "unknown ledger history cursor")
	}
	if err != nil {
		return nil, s.internal(ctx, "read ledger history", err)
	}
	out := &circusv1.ListDecisionHistoryResponse{}
	for _, v := range versions {
		out.Versions = append(out.Versions, &circusv1.DecisionVersion{
			Commit: v.Commit, CommittedAtUnixMs: v.CommittedAt, Author: v.Author, Summary: v.Summary,
			Path: v.Path, Title: v.Title, Added: v.Added, ContentChanged: v.ContentChanged, Moved: v.Moved, Retitled: v.Retitled,
			CodeCommit: v.CodeCommit,
		})
	}
	if more {
		out.NextPageToken = encodeLedgerCursor(id, number, versions[len(versions)-1].Commit)
	}
	return connect.NewResponse(out), nil
}

const ledgerCursorHeader = 1 + 4 + 40 // format version, number, commit

func encodeLedgerCursor(id string, number uint32, commit string) string {
	b := binary.BigEndian.AppendUint32([]byte{1}, number)
	b = append(b, commit...)
	b = append(b, id...)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeLedgerCursor(token, id string, number uint32) (string, bool) {
	if token == "" {
		return "", true
	}
	if len(token) != base64.RawURLEncoding.EncodedLen(ledgerCursorHeader+len(id)) {
		return "", false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(token)
	if err != nil || len(b) != ledgerCursorHeader+len(id) || b[0] != 1 ||
		binary.BigEndian.Uint32(b[1:5]) != number || string(b[ledgerCursorHeader:]) != id {
		return "", false
	}
	commit := string(b[5:ledgerCursorHeader])
	return commit, store.IsLedgerCommitID(commit)
}
