package auth

import (
	"container/list"
	"sync"
	"time"
)

// failureLimiter reserves attempts before work starts. Failures retain their
// reservation; success and internal errors release it. The least recently
// used key is evicted at capacity, including any outstanding reservations.
type failureLimiter struct {
	maxAttempts int
	backoff     time.Duration
	mu          sync.Mutex
	entries     map[string]*failureEntry
	oldest      list.List
}
type failureEntry struct {
	failures, pending   int
	since, blockedUntil time.Time
	order               *list.Element
}
type loginAttempt struct {
	limiter *failureLimiter
	key     string
	entry   *failureEntry
	once    sync.Once
}

const maxFailureKeys = 10000

func (l *failureLimiter) reserve(key string, now time.Time) *loginAttempt {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]*failureEntry)
	}
	e := l.entries[key]
	if e == nil {
		if len(l.entries) >= maxFailureKeys {
			oldest := l.oldest.Front()
			delete(l.entries, oldest.Value.(string))
			l.oldest.Remove(oldest)
		}
		e = &failureEntry{since: now, order: l.oldest.PushBack(key)}
		l.entries[key] = e
	} else {
		l.oldest.MoveToBack(e.order)
		if now.Sub(e.since) >= 10*time.Minute || (!e.blockedUntil.IsZero() && !now.Before(e.blockedUntil)) {
			// Keep pending work in the same entry so it cannot escape the cap.
			e.failures = 0
			e.since = now
			e.blockedUntil = time.Time{}
		}
	}
	if now.Before(e.blockedUntil) || e.failures+e.pending >= l.attemptLimit() {
		return nil
	}
	e.pending++
	return &loginAttempt{limiter: l, key: key, entry: e}
}
func (a *loginAttempt) finish(failed bool, now time.Time) {
	a.once.Do(func() {
		l := a.limiter
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.entries[a.key] != a.entry {
			return
		}
		e := a.entry
		e.pending--
		if failed {
			e.failures++
			if e.failures >= l.attemptLimit() {
				backoff := l.backoff
				if backoff == 0 {
					backoff = 3 * time.Minute
				}
				e.blockedUntil = now.Add(backoff)
			}
		}
		if e.failures == 0 && e.pending == 0 {
			delete(l.entries, a.key)
			l.oldest.Remove(e.order)
		}
	})
}

func (l *failureLimiter) attemptLimit() int {
	if l.maxAttempts == 0 {
		return 5
	}
	return l.maxAttempts
}
