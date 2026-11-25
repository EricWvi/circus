// Package ledgerslots bounds the ledger operations that use the most
// memory (checks, merges and conflict reads), in total and per user. The
// changeset RPCs take a slot or are refused at once; session snapshot work
// waits for one.
package ledgerslots

import (
	"context"
	"errors"
	"sync"
)

// The ledger write slots of a server: checks, merges and conflict reads
// of changesets, sessions' collections included, in total and per user. A
// user's share is smaller than the whole, so one user's clients that stop
// sending or reading cannot keep other users out; it takes the clients of
// three users to hold every slot. MaxWaiters bounds the session work
// waiting for a slot: the snapshot manager runs at most four operations
// at once and the RPCs that wait admit fewer than that each, so the bound
// is never reached.
const (
	Writes        = 3
	WritesPerUser = 1
	MaxWaiters    = 64
)

// ErrTooManyWaiters refuses a wait beyond the waiter bound. Callers bound
// their own concurrency below it, so it is never expected.
var ErrTooManyWaiters = errors.New("ledgerslots: too many waiters")

// Slots counts operations in flight, at most limit in total and perUser
// per user.
type Slots struct {
	mu             sync.Mutex
	limit, perUser int
	maxWaiters     int
	total, waiters int
	byUser         map[string]int
	// freed is closed, and replaced, whenever a slot is released.
	freed chan struct{}
}

// New returns slots for at most limit operations, perUser of one user,
// and at most maxWaiters callers waiting in Take.
func New(limit, perUser, maxWaiters int) *Slots {
	return &Slots{limit: limit, perUser: perUser, maxWaiters: maxWaiters, byUser: make(map[string]int), freed: make(chan struct{})}
}

// TryTake takes a slot for userID if one is free.
func (s *Slots) TryTake(userID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.takeLocked(userID)
}

func (s *Slots) takeLocked(userID string) bool {
	if s.total >= s.limit || s.byUser[userID] >= s.perUser {
		return false
	}
	s.total++
	s.byUser[userID]++
	return true
}

// Take waits until a slot for userID is free and takes it, or until ctx
// ends. Waiters are woken together when a slot is released; which one
// takes it is not ordered.
func (s *Slots) Take(ctx context.Context, userID string) error {
	s.mu.Lock()
	if s.takeLocked(userID) {
		s.mu.Unlock()
		return nil
	}
	if s.waiters >= s.maxWaiters {
		s.mu.Unlock()
		return ErrTooManyWaiters
	}
	s.waiters++
	defer func() {
		s.mu.Lock()
		s.waiters--
		s.mu.Unlock()
	}()
	for {
		freed := s.freed
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-freed:
		}
		s.mu.Lock()
		if s.takeLocked(userID) {
			s.mu.Unlock()
			return nil
		}
	}
}

// Waiting reports how many callers wait in Take.
func (s *Slots) Waiting() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waiters
}

// Release frees a slot userID took.
func (s *Slots) Release(userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total--
	if s.byUser[userID]--; s.byUser[userID] <= 0 {
		delete(s.byUser, userID)
	}
	close(s.freed)
	s.freed = make(chan struct{})
}
