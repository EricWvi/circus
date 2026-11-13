package calls

import "time"

// SetLockObserver installs the lock-hold observer used by the measurement in
// measure_lock_test.go and returns a function that restores the previous
// one. Call it only while no router runs.
func SetLockObserver(f func(kind string, held time.Duration)) (restore func()) {
	previous := lockObserver
	lockObserver = f
	return func() { lockObserver = previous }
}
