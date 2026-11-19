package sessions

import (
	"context"
	"encoding/json"

	"github.com/EricWvi/circus/internal/store"
)

// Stop reasons recorded in the stop event's payload.
const (
	StopReasonUser           = "user"
	StopReasonTranscriptFull = "transcript_full"
)

// AppendStopEvent records the control-plane view event of a session stop,
// KIND_SESSION_STATE {"state":"stopped","reason":reason}, as a boundary, in
// the caller's transaction (the one that stopped the session). It returns
// the event's seq, 0 when the view no longer stores it. Both stops use it:
// StopSession (StopReasonUser) and the transcript quota
// (StopReasonTranscriptFull). maxViewBytes is the session view's cap, as
// for every other event the caller's service appends. The caller announces
// the seq on the Bus after commit.
func AppendStopEvent(ctx context.Context, tx store.Ops, sessionID, reason string, at, maxViewBytes int64) (int64, error) {
	payload, err := json.Marshal(struct {
		State  string `json:"state"`
		Reason string `json:"reason"`
	}{"stopped", reason})
	if err != nil {
		return 0, err
	}
	res, err := tx.AppendEvents(ctx, sessionID, nil, []store.EventInput{{Kind: store.EventKindSessionState, Payload: payload, At: at, Boundary: true}}, maxViewBytes)
	if err != nil {
		return 0, err
	}
	return res.Seqs[0], nil
}
