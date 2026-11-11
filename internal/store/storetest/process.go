package storetest

import (
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// Backend is how the contract suite reaches one store implementation.
type Backend struct {
	// Open opens the store kept in dir, creating it if dir holds none, and
	// fails t on error. Opening the same dir twice yields two independent
	// handles on one database, as two processes would have.
	Open func(t testing.TB, dir string) store.Store
	// Image, when set, captures the durable state a process killed at this
	// instant would leave in dir, and returns a function that writes that
	// state back; it is called only after every handle on dir is closed.
	// Without Image, a crash is emulated by rolling back the open
	// transaction and closing the store.
	Image func(t testing.TB, dir string) (restore func())
}

// Process stands for one control plane process using the database in dir:
// it owns a Faulty store, can be killed at a write point, and can be
// restarted on the same database.
type Process struct {
	t       testing.TB
	b       Backend
	dir     string
	store   *Faulty
	restore func()
}

// StartProcess opens the database in dir. The latest store is closed when
// the test ends.
func StartProcess(t testing.TB, b Backend, dir string) *Process {
	t.Helper()
	p := &Process{t: t, b: b, dir: dir}
	p.store = NewFaulty(b.Open(t, dir))
	t.Cleanup(func() { _ = p.store.Close() })
	return p
}

// Store returns the live store. After a restart it is a different object;
// do not keep the old one.
func (p *Process) Store() *Faulty { return p.store }

// CrashAfterWrite kills the process right after its next n-th write point
// (see Faulty): the write, or commit, goes through, the backend's durable
// state is captured, and every later operation fails with ErrCrashed.
func (p *Process) CrashAfterWrite(n int) {
	p.store.CrashAfterWrite(n, func() {
		if p.b.Image != nil {
			p.restore = p.b.Image(p.t, p.dir)
		}
	})
}

// Restart drops the store and every in-memory object behind it, puts back
// the durable state captured at the crash (if one fired), and opens the same
// database again with no faults armed.
func (p *Process) Restart() {
	p.t.Helper()
	if err := p.store.Close(); err != nil && !p.store.Crashed() {
		p.t.Fatalf("close store before restart: %v", err)
	}
	if p.restore != nil {
		p.restore()
		p.restore = nil
	}
	p.store = NewFaulty(p.b.Open(p.t, p.dir))
}
