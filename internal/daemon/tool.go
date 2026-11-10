package daemon

import (
	"context"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

// Tool executes one kind of device call. Implementations must not keep session state.
type Tool interface {
	// Name is the oneof field name in DeviceCall.tool, e.g. "bash", "read".
	Name() string
	// Run executes the call. A tool-level failure (file not found, non-zero exit, ...) is
	// reported inside the returned DeviceResult with output.is_error set; a returned error
	// means the daemon itself failed.
	// A PreparingTool calls toolcontext.StartTimeout after its bounded preparation. Direct
	// callers' context deadlines are still respected.
	// Tools with internal processes may implement Shutdown(context.Context) error;
	// dispatcher shutdown invokes it after cancelling and settling active calls.
	Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error)
}

// PreparingTool excludes preparation from the command execution budget. A
// positive PreparationBudget bounds that phase; zero uses normal call timing.
// Run must call toolcontext.StartTimeout(ctx) after preparing. Cancellation remains active
// during preparation. The tool may also return before starting execution.
type PreparingTool interface {
	Tool
	PreparationBudget() time.Duration
}
