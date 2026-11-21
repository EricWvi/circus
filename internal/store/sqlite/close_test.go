package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

// walGone reports whether the last connection has closed: SQLite deletes
// the WAL and shared-memory files when it closes the last connection.
func walGone(t *testing.T, dir string) bool {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm"} {
		_, err := os.Stat(filepath.Join(dir, dbName+suffix))
		if err == nil {
			return false
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	return true
}

// Close returns only after the connection a caller still holds when Close
// starts has been closed, so nothing touches the files afterwards (a test's
// TempDir cleanup then removes them all).
func TestCloseWaitsForConnectionInUse(t *testing.T) {
	dir := t.TempDir()
	d := openAt(t, dir, Options{})
	rows, err := d.sqldb.QueryContext(t.Context(), "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	select {
	case err := <-closed:
		rows.Close()
		t.Fatalf("Close returned while a connection was in use (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	if walGone(t, dir) {
		t.Fatal("WAL removed while a connection was in use")
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := wait.Recv(t, closed); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if !walGone(t, dir) {
		t.Fatal("WAL still present after Close returned")
	}
}

// Smoke test of the canceled-transaction path on the real driver; see
// TestCloseWaitsForBackgroundRollback for the deterministic version.
func TestCloseAfterCanceledTx(t *testing.T) {
	for range 5 {
		dir := t.TempDir()
		d := openAt(t, dir, Options{})
		ctx, cancel := context.WithCancel(t.Context())
		err := d.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			if _, err := tx.GetSuperUser(ctx); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("GetSuperUser = %v", err)
			}
			cancel()
			<-ctx.Done()
			return ctx.Err()
		})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Tx = %v", err)
		}
		if err := d.Close(); err != nil {
			t.Fatalf("Close = %v", err)
		}
		if !walGone(t, dir) {
			t.Fatal("WAL still present after Close returned")
		}
	}
}

// A connection never released makes Close give up after closeWait and say
// so; connections requested after Close are refused.
func TestCloseReportsConnectionStillOpen(t *testing.T) {
	d := openAt(t, t.TempDir(), Options{})
	d.closeWait = 10 * time.Millisecond
	rows, err := d.sqldb.QueryContext(t.Context(), "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if err := d.Close(); err == nil || !strings.Contains(err.Error(), "connections still open 10ms after close: 1") {
		t.Fatalf("Close = %v", err)
	}
	if _, err := d.conns.Connect(t.Context()); !errors.Is(err, errClosed) {
		t.Fatalf("Connect after Close = %v", err)
	}
}

// fakeConnector opens fakeConns: a driver whose transactions block in
// Rollback until rollback is closed, and whose Close fails when closeErr is
// set.
type fakeConnector struct {
	rolling  chan struct{} // closed when a Rollback starts
	rollback chan struct{} // closing it lets Rollback return
	closeErr error
	closed   atomic.Int32
}

func (c *fakeConnector) Connect(context.Context) (driver.Conn, error) { return &fakeConn{c: c}, nil }
func (c *fakeConnector) Driver() driver.Driver                        { return nil }

type fakeConn struct{ c *fakeConnector }

func (f *fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.ErrUnsupported }
func (f *fakeConn) Close() error {
	if f.c.closeErr != nil {
		return f.c.closeErr
	}
	f.c.closed.Add(1)
	return nil
}
func (f *fakeConn) Begin() (driver.Tx, error) { return fakeTx{f.c}, nil }
func (f *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return fakeTx{f.c}, nil
}
func (f *fakeConn) PrepareContext(context.Context, string) (driver.Stmt, error) {
	return nil, errors.ErrUnsupported
}
func (f *fakeConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return nil, errors.ErrUnsupported
}
func (f *fakeConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return nil, errors.ErrUnsupported
}
func (f *fakeConn) Ping(context.Context) error         { return nil }
func (f *fakeConn) ResetSession(context.Context) error { return nil }
func (f *fakeConn) IsValid() bool                      { return true }

type fakeTx struct{ c *fakeConnector }

func (fakeTx) Commit() error { return nil }
func (t fakeTx) Rollback() error {
	close(t.c.rolling)
	<-t.c.rollback
	return nil
}

// database/sql rolls back a transaction whose context ends in a goroutine of
// its own, so Tx can return while that goroutine still holds the
// connection. Close waits for the rollback and returns right after it.
func TestCloseWaitsForBackgroundRollback(t *testing.T) {
	fake := &fakeConnector{rolling: make(chan struct{}), rollback: make(chan struct{})}
	conns := newConnTracker(fake)
	db := sql.OpenDB(conns)
	ctx, cancel := context.WithCancel(t.Context())
	if _, err := db.BeginTx(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	wait.Recv(t, fake.rolling)
	closed := make(chan error, 1)
	go func() { closed <- conns.closeDB(db, wait.Timeout) }()
	select {
	case err := <-closed:
		close(fake.rollback)
		t.Fatalf("Close returned during the background rollback (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	released := time.Now()
	close(fake.rollback)
	if err := wait.Recv(t, closed); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if d := time.Since(released); d > time.Second {
		t.Errorf("Close returned %v after the rollback", d)
	}
	if n := fake.closed.Load(); n != 1 {
		t.Errorf("driver closed %d connections, want 1", n)
	}
}

// Concurrent Close calls all return as soon as the last connection closes.
func TestConcurrentCloseReturnsWhenDrained(t *testing.T) {
	fake := &fakeConnector{}
	conns := newConnTracker(fake)
	db := sql.OpenDB(conns)
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 2)
	for range 2 {
		go func() { closed <- conns.closeDB(db, wait.Timeout) }()
	}
	select {
	case err := <-closed:
		conn.Close()
		t.Fatalf("Close returned while a connection was in use (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}
	released := time.Now()
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := wait.Recv(t, closed); err != nil {
			t.Errorf("Close = %v", err)
		}
	}
	if d := time.Since(released); d > time.Second {
		t.Errorf("Close calls returned %v after the release", d)
	}
}

// A connection the driver failed to close may still hold the files, so
// Close reports it instead of claiming that everything is closed.
func TestCloseReportsFailedDriverClose(t *testing.T) {
	fake := &fakeConnector{closeErr: errors.New("close failed")}
	conns := newConnTracker(fake)
	db := sql.OpenDB(conns)
	if err := db.PingContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := conns.closeDB(db, 10*time.Millisecond); err == nil || !strings.Contains(err.Error(), "connections still open 10ms after close: 1") {
		t.Fatalf("Close = %v", err)
	}
}
