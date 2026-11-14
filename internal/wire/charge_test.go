package wire_test

import (
	"strings"
	"testing"
	"unsafe"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
	"google.golang.org/protobuf/proto"
)

func TestOwnedCloneDetachesStringStorage(t *testing.T) {
	backing := strings.Repeat("x", 1<<20)
	small := backing[:4]
	m := &circusv1.DaemonDownlink{Payload: &circusv1.DaemonDownlink_CallQuery{CallQuery: &circusv1.CallQuery{CallId: small}}}
	if _, err := wire.Charge(m, 1024); err != nil {
		t.Fatal(err)
	}
	clone := wire.CloneOwned(m).(*circusv1.DaemonDownlink)
	got := clone.GetCallQuery().CallId
	if got != small || unsafe.StringData(got) == unsafe.StringData(small) {
		t.Fatal("clone retained caller backing storage")
	}
}

// Charge and the decoder enforce the same repeated-field caps, matched by
// full field name, and charge with the same formula.
func TestChargeAndDecodeShareLimitsAndCost(t *testing.T) {
	update := func(blocks int) *circusv1.WorkerDownlink {
		content := make([]*circusv1.ContentBlock, blocks)
		for i := range content {
			content[i] = &circusv1.ContentBlock{Block: &circusv1.ContentBlock_Text{Text: "x"}}
		}
		return &circusv1.WorkerDownlink{Payload: &circusv1.WorkerDownlink_CallUpdate{CallUpdate: &circusv1.CallUpdate{CallId: "c", Update: &circusv1.CallUpdate_Result{Result: &circusv1.DeviceResult{CallId: "c", Output: &circusv1.ToolOutput{Content: content}}}}}}
	}
	ok, over := update(128), update(129)
	cost, err := wire.Charge(ok, wire.DefaultMaxMessageBytes)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := proto.Marshal(ok)
	if want := wire.DecodedCost(wire.WorkerDownlink, data); cost != want {
		t.Fatalf("Charge cost %d, decoder cost %d", cost, want)
	}
	if _, err := wire.Charge(over, wire.DefaultMaxMessageBytes); err == nil || !strings.Contains(err.Error(), "circus.v1.ToolOutput.content") {
		t.Fatalf("Charge accepted 129 content blocks: %v", err)
	}
	data, _ = proto.Marshal(over)
	if _, err := wire.Decode(wire.WorkerDownlink, data, wire.DefaultMaxMessageBytes); err == nil || !strings.Contains(err.Error(), "circus.v1.ToolOutput.content") {
		t.Fatalf("Decode accepted 129 content blocks: %v", err)
	}
}

func TestTerminal(t *testing.T) {
	notFound := &circusv1.CallUpdate{Update: &circusv1.CallUpdate_Rejected_{Rejected: &circusv1.CallUpdate_Rejected{Code: circusv1.CallUpdate_Rejected_CODE_NOT_FOUND}}}
	invalid := &circusv1.CallUpdate{Update: &circusv1.CallUpdate_Rejected_{Rejected: &circusv1.CallUpdate_Rejected{Code: circusv1.CallUpdate_Rejected_CODE_INVALID}}}
	dispatched := &circusv1.CallUpdate{Update: &circusv1.CallUpdate_Dispatched_{Dispatched: &circusv1.CallUpdate_Dispatched{}}}
	if wire.Terminal(notFound) || wire.Terminal(dispatched) || !wire.Terminal(invalid) {
		t.Fatal("Terminal misclassifies")
	}
}
