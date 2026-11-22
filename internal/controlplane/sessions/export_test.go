package sessions

import "context"

// Expire runs one lease expiry sweep now.
func Expire(s *Service, ctx context.Context) { s.expire(ctx) }

// Extend runs one lease extension round now.
func Extend(s *Service, ctx context.Context) { s.extend(ctx) }

// Fallback runs the session_schedule fallback now.
func Fallback(s *Service, ctx context.Context) { s.fallback(ctx) }

// Idle reports whether no Wake or rescan waits for the scheduler.
func Idle(s *Service) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.wakes) == 0 && len(s.rescan) == 0 && len(s.provScans) == 0 && s.busy == 0
}

// Tracked returns the number of sessions in the in-memory table.
func Tracked(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// Rounds returns how many upkeep rounds have run (each re-arms the alarm).
func Rounds(s *Service) uint64 { return s.rounds.Load() }

// Attached returns the number of connections with an Attach stream.
func Attached(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.conns {
		if c.attached {
			n++
		}
	}
	return n
}

// AttachedConns returns the ids of the connections with an Attach stream.
func AttachedConns(s *Service) map[string]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]bool{}
	for id, c := range s.conns {
		if c.attached {
			out[id] = true
		}
	}
	return out
}

// DisableFallback turns the periodic rescan off, so a test sees what the
// other paths do alone.
func DisableFallback(s *Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noFallback = true
}

// Generation returns the current Wake generation.
func Generation(s *Service) uint64 { return s.generation() }

// ClearInput clears INPUT on session id as an operation at epoch that
// observed generation g would.
func ClearInput(s *Service, id string, epoch int64, g uint64) {
	s.clearReasons(id, epoch, reasonInput, g)
}

// HasInput reports whether INPUT is pending for session id.
func HasInput(s *Service, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	return e != nil && e.reasons&reasonInput != 0
}

// Pending reports whether session id's lease waits for its Lease reply.
func Pending(s *Service, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	return e != nil && e.pending
}

// DisableUpkeep stops lease expiry, extension and the fallback (the timer
// still runs), so a test can let a lease expire unswept.
func DisableUpkeep(s *Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.noUpkeep = true
}

// SetMaxTracked lowers the session table's bound.
func SetMaxTracked(s *Service, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxTracked = n
}

// Forget asks the scheduler's forget for session id, as an evaluation
// that found nothing to keep would.
func Forget(s *Service, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.entries[id]; e != nil {
		s.forget(id, e)
	}
}

// Has reports whether session id has an entry.
func Has(s *Service, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.entries[id] != nil
}

// Connections returns the number of registered connections.
func Connections(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// Abandoned reports whether session id's lease was abandoned by its
// AcquireLease.
func Abandoned(s *Service, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[id]
	return e != nil && e.abandoned
}

// SetCurrentBoot records boot as the worker's current boot in memory.
func SetCurrentBoot(s *Service, worker, boot string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bootSeq++
	s.boots[worker] = bootRec{boot: boot, seq: s.bootSeq}
}

// Busy reports whether the scheduler is processing Wakes or rescans.
func Busy(s *Service) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy > 0
}
