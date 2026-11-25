package webapi

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/ledgersnaps"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
)

const (
	// maxMergedListed bounds the merged changesets GetSessionLedger lists.
	maxMergedListed = 64
	// maxSessionDiffBytes is how much of each side GetSessionLedgerDiff
	// returns; one byte more marks a longer file.
	maxSessionDiffBytes = 256 << 10
	// A GetSessionLedger response: the status and changeset (a 4 KiB path,
	// 1 KiB texts), 4096 entries of two paths, two titles and a number,
	// the problems (256 of a 4 KiB path and a 1 KiB message, listed once
	// and again on their entries) and as many warnings, and 64 merged
	// changesets with up to 4096 numbers each; six JSON bytes per byte of
	// text at worst.
	maxProblemJSONBytes       = 6*(store.MaxSessionLedgerPathBytes+store.MaxSessionLedgerMessageBytes) + 64
	maxSessionLedgerBytes     = 64<<10 + store.MaxLedgerDecisions*(6*(2*ledger.MaxChangePathBytes+2*store.MaxLedgerTitleBytes+16)+128) + 4*store.MaxSessionLedgerProblems*maxProblemJSONBytes + maxMergedListed*(store.MaxLedgerDecisions*32+8<<10)
	maxSessionLedgerDiffBytes = 2*6*(maxSessionDiffBytes+1) + 4<<10
	maxMergeSessionBytes      = store.MaxLedgerDecisions*32 + 8<<10
)

// sessionLedger reads the snapshot of one of the user's sessions.
func (s *Service) sessionLedger(ctx context.Context, sessionID string) (store.Session, store.SessionLedger, error) {
	u, err := user(ctx)
	if err != nil {
		return store.Session{}, store.SessionLedger{}, err
	}
	if err := sessionIDArg(sessionID); err != nil {
		return store.Session{}, store.SessionLedger{}, err
	}
	sess, err := ownSession(ctx, s.st, u, sessionID, s.now().UnixMilli())
	if err != nil {
		return store.Session{}, store.SessionLedger{}, s.sessionError(ctx, "read session ledger", err)
	}
	l, err := s.st.GetSessionLedger(ctx, sessionID)
	if errors.Is(err, store.ErrNotFound) {
		return sess, store.SessionLedger{}, nil
	}
	if err != nil {
		return store.Session{}, store.SessionLedger{}, s.internal(ctx, "read session ledger", err)
	}
	return sess, l, nil
}

func problemMessages(list []store.LedgerProblem) []*circusv1.LedgerProblem {
	out := make([]*circusv1.LedgerProblem, 0, len(list))
	for _, p := range list {
		out = append(out, &circusv1.LedgerProblem{Path: p.Path, Message: p.Message})
	}
	return out
}

func numbersMessage(numbers map[string]uint32) map[string]uint32 {
	out := make(map[string]uint32, len(numbers))
	for letters, n := range numbers {
		out["D?"+letters] = n
	}
	return out
}

func (s *Service) GetSessionLedger(ctx context.Context, req *connect.Request[circusv1.GetSessionLedgerRequest]) (*connect.Response[circusv1.GetSessionLedgerResponse], error) {
	sess, l, err := s.sessionLedger(ctx, req.Msg.GetSessionId())
	if err != nil {
		return nil, err
	}
	out := &circusv1.GetSessionLedgerResponse{}
	if l.SessionID == "" || s.ledger == nil {
		return connect.NewResponse(out), nil
	}
	out.Status = ledgersnaps.Status(l)
	out.Problems = problemMessages(l.Problems)
	out.Warnings = problemMessages(l.Warnings)
	if l.ChangesetID != "" {
		cs, err := s.ledger.ReadSessionChangeset(ctx, l.ChangesetID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return nil, s.internal(ctx, "read session changeset", err)
		}
		if err == nil {
			out.Changeset = changesetMessage(cs.Changeset)
			out.FastForward = cs.FastForward
			byPath := make(map[string][]string)
			for _, p := range l.Problems {
				byPath[p.Path] = append(byPath[p.Path], p.Message)
			}
			for _, e := range cs.Entries {
				problems := byPath[e.Path]
				if e.BasePath != "" {
					problems = append(problems, byPath[e.BasePath]...)
				}
				out.Entries = append(out.Entries, &circusv1.SessionLedgerEntry{Path: e.Path, BasePath: e.BasePath, Number: e.Number,
					Title: entryTitle(e.Title), BaseTitle: entryTitle(e.BaseTitle), Added: e.Added, ContentChanged: e.ContentChanged, Moved: e.Moved,
					Retitled: e.Retitled, Deleted: e.Deleted, Problems: problems})
			}
		}
	}
	merged, _, err := s.st.ListLedgerChangesets(ctx, store.LedgerChangesetFilter{ProjectID: sess.ProjectID, SessionID: sess.ID,
		State: store.LedgerChangesetMerged, Limit: maxMergedListed})
	if err != nil {
		return nil, s.internal(ctx, "list merged changesets", err)
	}
	for _, c := range merged {
		out.Merged = append(out.Merged, &circusv1.MergedSessionChangeset{Changeset: changesetMessage(c), Numbers: numbersMessage(c.Merge.Numbers)})
	}
	return connect.NewResponse(out), nil
}

// entryTitle is a title as an entry shows it: a title the changeset has
// not validated yet may be long or hold control characters, which are
// cut after MaxLedgerTitleBytes and replaced.
func entryTitle(title string) string {
	title = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return unicode.ReplacementChar
		}
		return r
	}, title)
	if len(title) <= store.MaxLedgerTitleBytes {
		return title
	}
	n := store.MaxLedgerTitleBytes - len("…")
	for n > 0 && !utf8.RuneStart(title[n]) {
		n--
	}
	return title[:n] + "…"
}

// clipDiff cuts a side of a diff after maxSessionDiffBytes and one more
// byte, keeping valid UTF-8.
func clipDiff(b []byte) (string, bool) {
	if len(b) <= maxSessionDiffBytes {
		return string(b), false
	}
	return strings.ToValidUTF8(string(b[:maxSessionDiffBytes+1]), ""), true
}

func (s *Service) GetSessionLedgerDiff(ctx context.Context, req *connect.Request[circusv1.GetSessionLedgerDiffRequest]) (*connect.Response[circusv1.GetSessionLedgerDiffResponse], error) {
	p := req.Msg.GetPath()
	if p == "" || len(p) > store.MaxSessionLedgerPathBytes || !utf8.ValidString(p) {
		return nil, invalidField("path", "path must be an entry's path")
	}
	_, l, err := s.sessionLedger(ctx, req.Msg.GetSessionId())
	if err != nil {
		return nil, err
	}
	notFound := connect.NewError(connect.CodeNotFound, errors.New("the session's open changeset does not change that path"))
	if l.ChangesetID == "" || s.ledger == nil {
		return nil, notFound
	}
	d, err := s.ledger.ReadSessionDiff(ctx, l.ChangesetID, p)
	if errors.Is(err, store.ErrNotFound) {
		return nil, notFound
	}
	if err != nil {
		return nil, s.internal(ctx, "read session diff", err)
	}
	out := &circusv1.GetSessionLedgerDiffResponse{BaseExists: d.Base.Exists, ChangesetExists: d.Changeset.Exists}
	out.Base, out.BaseTruncated = clipDiff(d.Base.Content)
	out.Changeset, out.ChangesetTruncated = clipDiff(d.Changeset.Content)
	return connect.NewResponse(out), nil
}

// refusalError maps a snapshot manager's refusal.
func refusalError(r *ledgersnaps.Refusal) error {
	err := connect.NewError(connect.CodeFailedPrecondition, errors.New(r.Description))
	failure := &errdetails.PreconditionFailure{Violations: []*errdetails.PreconditionFailure_Violation{{
		Type: r.Type, Subject: r.Subject, Description: r.Description}}}
	if detail, detailErr := connect.NewErrorDetail(failure); detailErr == nil {
		err.AddDetail(detail)
	}
	return err
}

func (s *Service) MergeSessionLedger(ctx context.Context, req *connect.Request[circusv1.MergeSessionLedgerRequest]) (*connect.Response[circusv1.MergeSessionLedgerResponse], error) {
	sess, l, err := s.sessionLedger(ctx, req.Msg.GetSessionId())
	if err != nil {
		return nil, err
	}
	if len(req.Msg.GetExpectedChangesetId()) > store.MaxIDBytes {
		return nil, invalidField("expected_changeset_id", "changeset id is too long")
	}
	if l.SessionID == "" || s.snapshots == nil {
		return nil, refusalError(&ledgersnaps.Refusal{Type: ledgersnaps.RefuseNoChanges, Subject: sess.ID, Description: "the session has no ledger snapshot"})
	}
	merged, err := s.snapshots.MergeSession(ctx, sess.ID, req.Msg.GetExpectedChangesetId())
	var refusal *ledgersnaps.Refusal
	var repair *ledger.RepairError
	switch {
	case errors.As(err, &refusal):
		return nil, refusalError(refusal)
	case errors.As(err, &repair):
		return nil, repairRefusal(repair)
	case err != nil:
		return nil, s.internal(ctx, "merge session ledger", err)
	}
	return connect.NewResponse(&circusv1.MergeSessionLedgerResponse{Changeset: changesetMessage(merged), Numbers: numbersMessage(merged.Merge.Numbers)}), nil
}
