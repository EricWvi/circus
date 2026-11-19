package fakeworker

import (
	"encoding/json"
	"fmt"

	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/google/uuid"
)

// Keep fake usage checkpoints separate from the SDK's conversation entries.
const usageSubpath = "fakeworker/usage"

type usageTotals struct {
	Input    int `json:"input_tokens"`
	Output   int `json:"output_tokens"`
	Thinking int `json:"thinking_tokens"`
}

// Save cumulative usage before publishing the result that exposes it. A new
// holder can then continue even if the worker died before it reported ENDED.
func (s *session) saveUsage(total usageTotals) error {
	if total == s.totalUsage {
		return nil
	}
	entry := &pb.TranscriptEntry{Uuid: uuid.NewString(), Json: jsonString(total)}
	r, err := s.request(wc.Append(s.id, s.epoch, usageSubpath, s.usageBatchSeq+1, entry))
	if err != nil {
		return fmt.Errorf("save usage: %w", err)
	}
	if r.GetOk() == nil {
		return fmt.Errorf("save usage: %v", r)
	}
	s.usageBatchSeq++
	s.totalUsage = total
	return nil
}

func (s *session) loadUsage(raw string) error {
	var total usageTotals
	if err := json.Unmarshal([]byte(raw), &total); err != nil {
		return fmt.Errorf("load usage: %w", err)
	}
	if total.Input < s.totalUsage.Input || total.Output < s.totalUsage.Output || total.Thinking < s.totalUsage.Thinking {
		return fmt.Errorf("load usage: cumulative token counts decreased")
	}
	s.totalUsage = total
	return nil
}
