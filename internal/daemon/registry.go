package daemon

import (
	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/daemon/fstools"
	"github.com/EricWvi/circus/internal/daemon/jobs"
)

// DefaultTools is the single registry for tools available on this device.
func DefaultTools(manager *jobs.Manager) []Tool {
	return []Tool{fstools.NewRead(), fstools.NewWrite(), fstools.NewEdit(), bashtool.NewBash(manager), bashtool.NewTaskStop(manager)}
}
