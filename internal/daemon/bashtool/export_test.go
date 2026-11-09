package bashtool

import "github.com/EricWvi/circus/internal/daemon/jobs"

// ShareEnvironmentForTest reuses a fixture snapshot without sharing job ownership.
func ShareEnvironmentForTest(manager *jobs.Manager, source *Bash) *Bash {
	return &Bash{jobs: manager, environment: source.environment}
}
