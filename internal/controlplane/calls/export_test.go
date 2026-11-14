package calls

import "time"

// WithLockObserver sets the lock-hold observer used by the measurement in
// measure_lock_test.go on c.
func WithLockObserver(c *Config, f func(kind string, held time.Duration)) {
	c.lockObserver = f
}

// LastRead is the time the subscriber-stall sweep measures s from.
func LastRead(s *Subscription) time.Time {
	s.owner.mu.Lock()
	defer s.owner.mu.Unlock()
	return s.lastRead
}
