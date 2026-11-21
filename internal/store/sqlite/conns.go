package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"
)

// errClosed refuses connections requested after Close.
var errClosed = errors.New("sqlite: database is closed")

// driverConn is what database/sql uses of a modernc.org/sqlite connection.
// A tracked connection forwards exactly these interfaces.
type driverConn interface {
	driver.Conn
	driver.ConnBeginTx
	driver.ConnPrepareContext
	driver.ExecerContext
	driver.QueryerContext
	driver.Pinger
	driver.SessionResetter
	driver.Validator
}

// connTracker opens the pool's connections and counts them until the driver
// has closed them, so Close can return only after the last one is gone.
// sql.DB.Close cannot promise that: connections still in use stay open
// until released, and database/sql works on the pool in goroutines of its
// own after a store call has returned (a transaction whose context ends is
// rolled back by one, and a connection discarded after an interrupted
// statement is replaced in the background for a waiting caller).
type connTracker struct {
	inner driver.Connector

	mu      sync.Mutex
	closed  bool
	open    int
	drained chan struct{} // closed once open is zero after stop
	drain   sync.Once
}

var _ driver.Connector = (*connTracker)(nil)

func newConnTracker(inner driver.Connector) *connTracker {
	return &connTracker{inner: inner, drained: make(chan struct{})}
}

func (t *connTracker) Connect(ctx context.Context) (driver.Conn, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, errClosed
	}
	t.open++
	t.mu.Unlock()
	c, err := t.inner.Connect(ctx)
	if err != nil {
		t.release()
		return nil, err
	}
	dc, ok := c.(driverConn)
	if !ok {
		err := c.Close()
		t.release()
		return nil, errors.Join(fmt.Errorf("sqlite: driver connection %T lacks required interfaces", c), err)
	}
	return &trackedConn{driverConn: dc, tracker: t}, nil
}

func (t *connTracker) Driver() driver.Driver { return t.inner.Driver() }

func (t *connTracker) release() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.open--
	t.drainedLocked()
}

func (t *connTracker) drainedLocked() {
	if t.closed && t.open == 0 {
		t.drain.Do(func() { close(t.drained) })
	}
}

// closeDB closes db, refuses new connections and waits up to wait until
// every connection db opened through t has been closed by the driver.
// Concurrent and repeated calls all return once the last one is closed.
func (t *connTracker) closeDB(db *sql.DB, wait time.Duration) error {
	err := db.Close()
	t.mu.Lock()
	t.closed = true
	t.drainedLocked()
	t.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-t.drained:
		return err
	case <-timer.C:
		t.mu.Lock()
		n := t.open
		t.mu.Unlock()
		return errors.Join(err, fmt.Errorf("sqlite: connections still open %v after close: %d", wait, n))
	}
}

type trackedConn struct {
	driverConn
	tracker *connTracker
	once    sync.Once
}

// Close counts the connection as closed only when the driver closed it.
// modernc.org/sqlite keeps the database handle open when sqlite3_close_v2
// fails (it only fails on misuse), and database/sql never retries a failed
// Close, so the files may stay open: closeDB then reports the connection
// instead of claiming that nothing touches the files any more.
func (c *trackedConn) Close() error {
	err := c.driverConn.Close()
	if err == nil {
		c.once.Do(c.tracker.release)
	}
	return err
}
