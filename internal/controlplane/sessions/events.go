package sessions

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/EricWvi/circus/internal/store"
)

// Stop reasons recorded in the stop event's payload.
const (
	StopReasonUser           = "user"
	StopReasonTranscriptFull = "transcript_full"
)

// AppendStopEvent records the control-plane view events of a session stop
// in the caller's transaction (the one that stopped the session): first
// APPROVAL_RESOLVED {"approval_id","kind":"question","state":"expired"} for
// each pending question the stop ended (only a stop expires a question, and
// a session stops once, so these are the session's expired questions), then
// KIND_SESSION_STATE {"state":"stopped","reason":reason}, all boundaries. It
// returns the last event's seq, 0 when the view no longer stores it. Both
// stops use it: StopSession (StopReasonUser) and the transcript quota
// (StopReasonTranscriptFull). maxViewBytes is the session view's cap, as
// for every other event the caller's service appends. The caller announces
// the seq on the Bus after commit.
func AppendStopEvent(ctx context.Context, tx store.Ops, sessionID, reason string, at, maxViewBytes int64) (int64, error) {
	ended, err := tx.ListApprovals(ctx, store.ApprovalFilter{SessionID: sessionID, States: []store.ApprovalState{store.ApprovalExpired},
		Limit: store.MaxPageSize, MaxBytes: store.MaxTranscriptPageBytes})
	if err != nil {
		return 0, err
	}
	var seq int64
	for _, a := range ended {
		if a.Kind != store.ApprovalQuestion {
			continue
		}
		n, err := AppendApprovalResolvedEvent(ctx, tx, sessionID, ApprovalResolvedPayload{
			ApprovalID: a.ApprovalID, Kind: string(store.ApprovalQuestion), State: string(store.ApprovalExpired),
		}, at, maxViewBytes)
		if err != nil {
			return 0, err
		}
		seq = max(seq, n)
	}
	n, err := appendJSONEvent(ctx, tx, sessionID, store.EventKindSessionState, struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	}{"stopped", reason}, at, maxViewBytes)
	return max(seq, n), err
}

// ApprovalResolvedPayload is the payload of KIND_APPROVAL_RESOLVED. A tool
// approval's has no kind: {"approval_id","state"}. A question's names its
// kind, and an answered one its answers.
type ApprovalResolvedPayload struct {
	ApprovalID string                `json:"approval_id"`
	Kind       string                `json:"kind,omitempty"`
	State      string                `json:"state"`
	Answers    []QuestionAnswerEvent `json:"answers,omitempty"`
	// InputID is the KIND_ANSWER input an answered question wrote in the
	// same transaction (the uuid of the answer's user message).
	InputID string `json:"input_id,omitempty"`
}

// QuestionAnswerEvent is one answer in an APPROVAL_RESOLVED payload.
type QuestionAnswerEvent struct {
	Question string   `json:"question"`
	Selected []string `json:"selected"`
	Other    string   `json:"other"`
}

// ApprovalPendingPayload is the payload of KIND_APPROVAL_PENDING, which the
// control plane records in the RequestApproval transaction that creates
// the approval. Device is the device's name (empty for a question);
// InputJSON is set for a question only.
type ApprovalPendingPayload struct {
	ApprovalID string  `json:"approval_id"`
	Kind       string  `json:"kind"`
	Tool       string  `json:"tool"`
	Device     string  `json:"device"`
	ToolUseID  string  `json:"tool_use_id"`
	Display    string  `json:"display"`
	Reason     string  `json:"reason"`
	InputJSON  *string `json:"input_json,omitempty"`
}

// AppendApprovalResolvedEvent records KIND_APPROVAL_RESOLVED with payload,
// encoded without HTML escaping, as a boundary, in the caller's
// transaction. It returns the event's seq, 0 when the view no longer
// stores it; the caller announces it on the Bus after commit.
func AppendApprovalResolvedEvent(ctx context.Context, tx store.Ops, sessionID string, payload ApprovalResolvedPayload, at, maxViewBytes int64) (int64, error) {
	return appendJSONEvent(ctx, tx, sessionID, store.EventKindApprovalResolved, payload, at, maxViewBytes)
}

// appendJSONEvent records one durable boundary event of kind with payload
// encoded without HTML escaping; it returns its seq, 0 when the view no
// longer stores it.
func appendJSONEvent(ctx context.Context, tx store.Ops, sessionID, kind string, payload any, at, maxViewBytes int64) (int64, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return 0, err
	}
	res, err := tx.AppendEvents(ctx, sessionID, nil, []store.EventInput{{Kind: kind,
		Payload: bytes.TrimSuffix(b.Bytes(), []byte("\n")), At: at, Boundary: true}}, maxViewBytes)
	if err != nil {
		return 0, err
	}
	return res.Seqs[0], nil
}
