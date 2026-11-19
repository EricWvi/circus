package sessions

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
)

func ok(dedup bool) *circusv1.SessionReply {
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_Ok_{Ok: &circusv1.SessionReply_Ok{Deduplicated: dedup}}}
}

func fencedReply(epoch int64) *circusv1.SessionReply {
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_Fenced_{Fenced: &circusv1.SessionReply_Fenced{Epoch: uint64(max(epoch, 0))}}}
}

func rejected(code circusv1.SessionReply_Rejected_Code, msg string) *circusv1.SessionReply {
	if len(msg) > replyMessageBytes {
		msg = strings.ToValidUTF8(msg[:replyMessageBytes], "")
	}
	return &circusv1.SessionReply{Result: &circusv1.SessionReply_Rejected_{Rejected: &circusv1.SessionReply_Rejected{Code: code, Message: msg}}}
}

func invalidReply(msg string) *circusv1.SessionReply {
	return rejected(circusv1.SessionReply_Rejected_CODE_INVALID, msg)
}

// failure maps a store error of a session operation to its reply. Only
// argument and count refusals carry the store's text, which names the
// rule broken and never request content beyond lengths and sequences.
func (s *Service) failure(ctx context.Context, op string, err error) *circusv1.SessionReply {
	var fe *store.FencedError
	switch {
	case errors.As(err, &fe):
		return fencedReply(fe.Epoch)
	case errors.Is(err, store.ErrStopped):
		return rejected(circusv1.SessionReply_Rejected_CODE_STOPPED, "session stopped")
	case errors.Is(err, store.ErrBroken):
		return rejected(circusv1.SessionReply_Rejected_CODE_BROKEN, "session transcript is broken; wait for its repair or recovery")
	case errors.Is(err, store.ErrLimit):
		return rejected(circusv1.SessionReply_Rejected_CODE_LIMIT, err.Error())
	case errors.Is(err, store.ErrInvalid):
		return rejected(circusv1.SessionReply_Rejected_CODE_INVALID, err.Error())
	case errors.Is(err, store.ErrNotFound):
		return rejected(circusv1.SessionReply_Rejected_CODE_NOT_FOUND, "not found")
	case errors.Is(err, store.ErrTranscriptFull):
		return rejected(circusv1.SessionReply_Rejected_CODE_TRANSCRIPT_FULL, "transcript full")
	}
	if ctx.Err() == nil {
		s.log.LogAttrs(ctx, slog.LevelWarn, "session operation store failure", slog.String("op", op), slog.Any("error", err))
	}
	return rejected(circusv1.SessionReply_Rejected_CODE_STORE_UNAVAILABLE, "store unavailable; retry")
}

// devices resolves device names to ids and back for one user, inside one
// transaction. Its caches hold at most the devices one request names.
type devices struct {
	tx     store.Ops
	user   string
	names  map[string]string // id → name
	byName map[string]string // name → id
}

func newDevices(tx store.Ops, user string) *devices {
	return &devices{tx: tx, user: user, names: map[string]string{}, byName: map[string]string{}}
}

// id returns the id of the user's live device name; store.ErrNotFound when
// there is none.
func (d *devices) id(ctx context.Context, name string) (string, error) {
	if id, ok := d.byName[name]; ok {
		return id, nil
	}
	if err := store.CheckID("device name", name); err != nil {
		return "", err
	}
	ident, err := d.tx.GetIdentityByName(ctx, store.KindDevice, d.user, name)
	if err != nil {
		return "", err
	}
	d.byName[name], d.names[ident.ID] = ident.ID, name
	return ident.ID, nil
}

// name returns the name of device id, deleted or not; the id itself when
// the device row is gone.
func (d *devices) name(ctx context.Context, id string) (string, error) {
	if id == "" {
		return "", nil
	}
	if n, ok := d.names[id]; ok {
		return n, nil
	}
	ident, err := d.tx.GetIdentity(ctx, store.KindDevice, id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		d.names[id] = id
		return id, nil
	case err != nil:
		return "", err
	}
	d.names[id] = ident.Name
	return ident.Name, nil
}

// eventKind maps a SessionEvent kind to its stored name (the enum name
// without KIND_); "" for UNSPECIFIED or a value this build does not know,
// which the store refuses.
func eventKind(k circusv1.SessionEvent_Kind) string {
	n, ok := circusv1.SessionEvent_Kind_name[int32(k)]
	if !ok || k == circusv1.SessionEvent_KIND_UNSPECIFIED {
		return ""
	}
	return strings.TrimPrefix(n, "KIND_")
}

// controlPlaneOnly reports kinds only the control plane records.
func controlPlaneOnly(kind string) bool {
	return store.ControlPlaneOnlyKind(kind)
}

// usage parses the SDK result's usage JSON; zero usage when it does not
// parse or holds a negative number.
func usage(raw string) store.TurnUsage {
	if raw == "" {
		return store.TurnUsage{}
	}
	var u struct {
		InputTokens              int64   `json:"input_tokens"`
		OutputTokens             int64   `json:"output_tokens"`
		CacheReadInputTokens     int64   `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64   `json:"cache_creation_input_tokens"`
		TotalCostUSD             float64 `json:"total_cost_usd"`
		DurationMs               int64   `json:"duration_ms"`
	}
	if json.Unmarshal([]byte(raw), &u) != nil {
		return store.TurnUsage{}
	}
	cost := u.TotalCostUSD * 1e6
	if u.InputTokens < 0 || u.OutputTokens < 0 || u.CacheReadInputTokens < 0 || u.CacheCreationInputTokens < 0 ||
		u.DurationMs < 0 || cost < 0 || cost > math.MaxInt64/2 || math.IsNaN(cost) {
		return store.TurnUsage{}
	}
	return store.TurnUsage{InputTokens: u.InputTokens, OutputTokens: u.OutputTokens, CacheReadInputTokens: u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens, CostMicroUSD: int64(math.Round(cost)), DurationMs: u.DurationMs}
}

func turnOrigin(o circusv1.TurnOrigin) store.TurnOrigin {
	switch o {
	case circusv1.TurnOrigin_TURN_ORIGIN_INPUT:
		return store.TurnFromInput
	case circusv1.TurnOrigin_TURN_ORIGIN_NOTIFICATION:
		return store.TurnFromNotification
	case circusv1.TurnOrigin_TURN_ORIGIN_CLI:
		return store.TurnFromCLI
	}
	return ""
}

func turnOutcome(o circusv1.TurnOutcome) store.TurnOutcome {
	switch o {
	case circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED:
		return store.TurnCompleted
	case circusv1.TurnOutcome_TURN_OUTCOME_INTERRUPTED:
		return store.TurnInterrupted
	case circusv1.TurnOutcome_TURN_OUTCOME_ERROR:
		return store.TurnError
	case circusv1.TurnOutcome_TURN_OUTCOME_STOPPED:
		return store.TurnStopped
	}
	return "unspecified"
}

// jobCalls maps job keys to the ids of the calls that started them, from
// the fenced session's job records (at most store.MaxSessionJobs). Keys of
// no session job are skipped, as the store skips other sessions' ids.
func jobCalls(ctx context.Context, tx store.Ops, f store.Fence, keys []*circusv1.JobKey) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	want := make(map[[3]string]bool, len(keys))
	for _, k := range keys {
		for _, v := range []string{k.GetDeviceId(), k.GetBootId(), k.GetJobId()} {
			if err := store.CheckID("job key", v); err != nil {
				return nil, err
			}
		}
		want[[3]string{k.GetDeviceId(), k.GetBootId(), k.GetJobId()}] = true
	}
	var out []string
	var after *store.RecordsCursor
	for {
		page, err := tx.GetSessionRecords(ctx, f, store.RecordsRequest{Sections: store.RecordJobs, MaxBytes: store.MaxTranscriptPageBytes, After: after})
		if err != nil {
			return nil, err
		}
		for _, j := range page.Jobs {
			if want[[3]string{j.DeviceID, j.BootID, j.JobID}] {
				out = append(out, j.CallID)
			}
		}
		if page.Next == nil {
			return out, nil
		}
		after = page.Next
	}
}

// persisted converts a persisted set.
func persisted(ctx context.Context, tx store.Ops, f store.Fence, p *circusv1.ReportTurn_Persisted) (store.TurnPersisted, error) {
	if p == nil {
		return store.TurnPersisted{}, nil
	}
	if p.GapNoticeTurnNo > math.MaxInt64 {
		return store.TurnPersisted{}, store.ErrInvalid
	}
	jobs, err := jobCalls(ctx, tx, f, p.Jobs)
	if err != nil {
		return store.TurnPersisted{}, err
	}
	return store.TurnPersisted{InputIDs: p.InputIds, CallIDs: p.CallIds, JobCallIDs: jobs, LateCallIDs: p.LateCallIds,
		GapNoticeTurnNo: int64(p.GapNoticeTurnNo)}, nil
}

func callOutcome(s store.CallState) circusv1.CallOutcome {
	switch s.Outcome() {
	case store.OutcomeAccepted:
		return circusv1.CallOutcome_CALL_OUTCOME_ACCEPTED
	case store.OutcomeDispatched:
		return circusv1.CallOutcome_CALL_OUTCOME_DISPATCHED
	case store.OutcomeUnknown:
		return circusv1.CallOutcome_CALL_OUTCOME_UNKNOWN
	case store.OutcomeResult:
		return circusv1.CallOutcome_CALL_OUTCOME_RESULT
	case store.OutcomeOffline:
		return circusv1.CallOutcome_CALL_OUTCOME_OFFLINE
	case store.OutcomeLost:
		return circusv1.CallOutcome_CALL_OUTCOME_LOST
	case store.OutcomeRejected:
		return circusv1.CallOutcome_CALL_OUTCOME_REJECTED
	case store.OutcomeCancelled:
		return circusv1.CallOutcome_CALL_OUTCOME_CANCELLED
	}
	return circusv1.CallOutcome_CALL_OUTCOME_UNSPECIFIED
}

func callProto(ctx context.Context, d *devices, c store.Call) (*circusv1.SessionState_Call, error) {
	name, err := d.name(ctx, c.DeviceID)
	if err != nil {
		return nil, err
	}
	out := &circusv1.SessionState_Call{CallId: c.CallID, ToolUseId: c.ToolUseID, Device: name, Returned: c.ReturnedAt != 0,
		Tool: c.Tool, Outcome: callOutcome(c.State), NoticeOwed: c.NoticeOwed}
	if c.ExitCode != nil && c.State.Terminal() {
		v := *c.ExitCode
		out.ExitCode = &v
	}
	if c.State == store.CallRejected {
		out.RejectedCode = circusv1.CallUpdate_Rejected_Code(circusv1.CallUpdate_Rejected_Code_value["CODE_"+c.OutcomeReason])
	}
	return out, nil
}

func jobState(s store.JobState) circusv1.JobEvent_State {
	switch s {
	case store.JobRunning:
		return circusv1.JobEvent_STATE_RUNNING
	case store.JobExited:
		return circusv1.JobEvent_STATE_EXITED
	case store.JobKilled:
		return circusv1.JobEvent_STATE_KILLED
	case store.JobLost:
		return circusv1.JobEvent_STATE_LOST
	}
	return circusv1.JobEvent_STATE_UNSPECIFIED
}

// sha256Bytes decodes a FileRecord hash: empty, or 64 hex digits.
func sha256Bytes(h string) ([]byte, bool) {
	if h == "" {
		return nil, true
	}
	if len(h) != 64 {
		return nil, false
	}
	b, err := hex.DecodeString(strings.ToLower(h))
	return b, err == nil
}

// sections maps GetSessionState sections to record sections; calls are
// read separately. ok is false for an invalid request: more than five,
// UNSPECIFIED, a repeat, or NONE beside another section.
func sections(in []circusv1.GetSessionState_Section) (recs store.RecordSections, calls bool, valid bool) {
	if len(in) == 0 {
		return store.RecordCwds | store.RecordFiles | store.RecordJobs | store.RecordGrants, true, true
	}
	if len(in) > 5 {
		return 0, false, false
	}
	seen := map[circusv1.GetSessionState_Section]bool{}
	for _, v := range in {
		if seen[v] {
			return 0, false, false
		}
		seen[v] = true
		switch v {
		case circusv1.GetSessionState_SECTION_CWDS:
			recs |= store.RecordCwds
		case circusv1.GetSessionState_SECTION_FILES:
			recs |= store.RecordFiles
		case circusv1.GetSessionState_SECTION_JOBS:
			recs |= store.RecordJobs
		case circusv1.GetSessionState_SECTION_CALLS:
			calls = true
		case circusv1.GetSessionState_SECTION_GRANTS:
			recs |= store.RecordGrants
		case circusv1.GetSessionState_SECTION_NONE:
			if len(in) != 1 {
				return 0, false, false
			}
		default:
			return 0, false, false
		}
	}
	return recs, calls, true
}

// buildState reads the selected sections of the fenced session's state.
// The scalars (stopped, interrupt level) come from sess.
func buildState(ctx context.Context, tx store.Ops, d *devices, f store.Fence, sess store.Session, recs store.RecordSections) (*circusv1.SessionState, error) {
	st := &circusv1.SessionState{Stopped: sess.State == store.SessionStopped, InterruptTurnNo: uint64(max(sess.InterruptTurnNo, 0))}
	var after *store.RecordsCursor
	for recs != 0 {
		page, err := tx.GetSessionRecords(ctx, f, store.RecordsRequest{Sections: recs, MaxBytes: store.MaxTranscriptPageBytes, After: after})
		if err != nil {
			return nil, err
		}
		for _, c := range page.Cwds {
			name, err := d.name(ctx, c.DeviceID)
			if err != nil {
				return nil, err
			}
			st.Cwds = append(st.Cwds, &circusv1.PatchSessionState_Cwd{AgentId: c.AgentID, Device: name, Cwd: c.Cwd})
		}
		for _, fr := range page.Files {
			name, err := d.name(ctx, fr.DeviceID)
			if err != nil {
				return nil, err
			}
			st.Files = append(st.Files, &circusv1.PatchSessionState_FileRecord{Device: name, Path: fr.Path, Sha256: hex.EncodeToString(fr.SHA256), MtimeUnixMs: fr.MtimeMs})
		}
		for _, j := range page.Jobs {
			if j.Notice == store.JobNoticePersisted {
				continue
			}
			name, err := d.name(ctx, j.DeviceID)
			if err != nil {
				return nil, err
			}
			pj := &circusv1.SessionState_Job{Key: &circusv1.JobKey{DeviceId: j.DeviceID, BootId: j.BootID, JobId: j.JobID}, CallId: j.CallID,
				ToolUseId: j.ToolUseID, Description: j.Description, OutputPath: j.OutputPath, State: jobState(j.State), Device: name,
				Handed: j.Notice == store.JobNoticeHanded}
			if j.ExitCode != nil {
				pj.ExitCode = *j.ExitCode
			}
			st.Jobs = append(st.Jobs, pj)
		}
		for _, g := range page.Grants {
			name, err := d.name(ctx, g.Key.DeviceID)
			if err != nil {
				return nil, err
			}
			st.Grants = append(st.Grants, &circusv1.SessionState_Grant{Tool: g.Key.Tool, Device: name, MatchKey: g.Key.MatchKey, ExpiresAtUnixMs: g.ExpiresAt})
		}
		if page.Next == nil {
			break
		}
		after = page.Next
	}
	return st, nil
}

// recoveryCalls returns every recovery call of the fenced session: the
// ones GetRecovery listed and, when it truncated, the rest by page.
func recoveryCalls(ctx context.Context, tx store.Ops, d *devices, f store.Fence, rec store.Recovery) ([]*circusv1.SessionState_Call, error) {
	calls := rec.Calls
	if rec.CallsTruncated && len(calls) > 0 {
		last := calls[len(calls)-1]
		after := &store.CallCursor{SubmittedAt: last.SubmittedAt, CallID: last.CallID}
		for len(calls) < store.MaxRecoveryCalls {
			page, err := tx.ListCalls(ctx, store.CallFilter{SessionID: f.SessionID, Recovery: true, Limit: store.MaxPageSize, After: after})
			if err != nil {
				return nil, err
			}
			calls = append(calls, page...)
			if len(page) < store.MaxPageSize {
				break
			}
			last = page[len(page)-1]
			after = &store.CallCursor{SubmittedAt: last.SubmittedAt, CallID: last.CallID}
		}
		calls = calls[:min(len(calls), store.MaxRecoveryCalls)]
	}
	out := make([]*circusv1.SessionState_Call, 0, len(calls))
	for _, c := range calls {
		pc, err := callProto(ctx, d, c)
		if err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, nil
}
