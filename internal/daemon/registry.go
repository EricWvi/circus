package daemon

import (
	"errors"

	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/daemon/fstools"
)

type bashJobs struct{ jobs *JobManager }

func (a bashJobs) StartBash(sessionID, command string, launch func(string) (bashtool.Job, string, error)) (string, string, error) {
	info, err := a.jobs.Start(sessionID, command, func(id string) (Job, string, error) {
		job, outputPath, err := launch(id)
		return Job(job), outputPath, err
	})
	return info.ID, info.OutputPath, err
}

func (a bashJobs) StopBash(sessionID, id string) (string, bool, error) {
	info, err := a.jobs.StopOwned(sessionID, id)
	if errors.Is(err, ErrJobNotFound) {
		return "", false, nil
	}
	return info.Command, true, err
}

// DefaultTools is the single registry for tools available on this device.
// Tools that need background jobs can receive jobs at construction here.
func DefaultTools(jobs *JobManager) []Tool {
	var manager bashtool.Jobs
	if jobs != nil {
		manager = bashJobs{jobs: jobs}
	}
	return []Tool{fstools.NewRead(), fstools.NewWrite(), fstools.NewEdit(), bashtool.NewBash(manager), bashtool.NewTaskStop(manager)}
}
