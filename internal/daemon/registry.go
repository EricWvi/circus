package daemon

import (
	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/daemon/fstools"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"github.com/EricWvi/circus/internal/daemon/workspace"
)

// DefaultTools is the single registry for tools available on this device.
// Workspace preparation runs git with the Bash tool's environment.
func DefaultTools(manager *jobs.Manager, options bashtool.Options) []Tool {
	bash := bashtool.NewBash(manager, options)
	ws := workspace.Options{Jobs: manager, Environment: bash, Home: options.Home}
	return []Tool{fstools.NewRead(), fstools.NewWrite(), fstools.NewEdit(), bash, bashtool.NewTaskStop(manager),
		workspace.NewPrepare(ws), workspace.NewRemove(ws)}
}
