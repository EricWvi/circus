package daemon

// DefaultTools is the single registry for tools available on this device.
// Tools that need background jobs can receive jobs at construction here.
func DefaultTools(jobs *JobManager) []Tool {
	return []Tool{}
}
