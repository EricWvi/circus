package daemon

import (
	"context"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
)

// Tool executes one kind of device call. Implementations must not keep session state.
type Tool interface {
	// Name is the oneof field name in DeviceCall.tool, e.g. "bash", "read".
	Name() string
	// Run executes the call. A tool-level failure (file not found, non-zero exit, ...) is
	// reported inside the returned DeviceResult with output.is_error set; a returned error
	// means the daemon itself failed.
	Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error)
}
