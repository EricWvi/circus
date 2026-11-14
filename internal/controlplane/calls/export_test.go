package calls

import "time"

// WithLockObserver sets the lock-hold observer used by the measurement in
// measure_lock_test.go on c.
func WithLockObserver(c *Config, f func(kind string, held time.Duration)) {
	c.lockObserver = f
}
