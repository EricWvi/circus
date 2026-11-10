package bashtool

import "github.com/EricWvi/circus/internal/daemon/jobs"

// ShareEnvironmentForTest reuses a fixture snapshot without sharing job
// ownership. The source Bash keeps ownership: only its Shutdown releases the
// shared instance.
func ShareEnvironmentForTest(manager *jobs.Manager, source *Bash) *Bash {
	return &Bash{jobs: manager, environment: source.environment}
}
