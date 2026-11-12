package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/storetest"
	"github.com/EricWvi/circus/internal/testutil/leakcheck"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Run(m, leakcheck.Config{})) }

const dbName = "circus.db"

func openAt(t testing.TB, dir string, opts Options) *DB {
	t.Helper()
	d, err := Open(context.Background(), filepath.Join(dir, dbName), opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// backend opens the database file in dir. Its crash image is the database
// and WAL files as they are on disk at the crash point: exactly what a
// SIGKILLed process leaves behind (SQLite never writes uncommitted pages to
// the main file in WAL mode, and recovery ignores WAL frames after the last
// commit). The shared-memory index is not part of it; SQLite rebuilds it.
var backend = storetest.Backend{
	Open: func(t testing.TB, dir string) store.Store { return openAt(t, dir, Options{}) },
	Image: func(t testing.TB, dir string) func() {
		t.Helper()
		files := map[string][]byte{}
		for _, name := range []string{dbName, dbName + "-wal"} {
			b, err := os.ReadFile(filepath.Join(dir, name))
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			files[name] = b
		}
		return func() {
			t.Helper()
			for _, name := range []string{dbName, dbName + "-wal", dbName + "-shm"} {
				path := filepath.Join(dir, name)
				if b, ok := files[name]; ok {
					if err := os.WriteFile(path, b, 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					t.Fatal(err)
				}
			}
		}
	},
}

func TestContract(t *testing.T) { storetest.Run(t, backend) }

// TestSettings proves the pragmas are in effect on every connection the pool
// opens, not only the first, and that the pool is a single writer.
func TestSettings(t *testing.T) {
	dir := t.TempDir()
	d := openAt(t, dir, Options{BusyTimeout: 1234 * time.Millisecond})
	defer d.Close()
	ctx := t.Context()
	if err := d.CreateUser(ctx, store.User{ID: "u1", Name: "a", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if n := d.sqldb.Stats().MaxOpenConnections; n != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1", n)
	}
	// WAL is really used: the commit landed in the -wal file. (Closing the
	// last connection checkpoints and removes it, so look first.)
	if fi, err := os.Stat(filepath.Join(dir, dbName+"-wal")); err != nil || fi.Size() == 0 {
		t.Fatalf("wal file: %v, %v", fi, err)
	}
	// Without idle connections every query below runs on a fresh one.
	d.sqldb.SetMaxIdleConns(0)
	for range 3 {
		s, err := readSettings(ctx, d.q)
		if err != nil {
			t.Fatal(err)
		}
		want := settings{JournalMode: "wal", Synchronous: 2, ForeignKeys: 1, BusyTimeout: 1234}
		if s != want {
			t.Fatalf("settings = %+v, want %+v", s, want)
		}
	}
	// The database files are the only ones, all inside dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !slices.Contains([]string{dbName, dbName + "-wal", dbName + "-shm"}, e.Name()) {
			t.Errorf("unexpected file %s", e.Name())
		}
	}
}

// TestBusy: a second process holding the write lock past the busy timeout
// makes writes fail with store.ErrBusy, writing nothing; once the lock is
// released the same write succeeds.
func TestBusy(t *testing.T) {
	dir := t.TempDir()
	holder := openAt(t, dir, Options{})
	defer holder.Close()
	waiter := openAt(t, dir, Options{BusyTimeout: 50 * time.Millisecond})
	defer waiter.Close()
	ctx := t.Context()

	locked, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- holder.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
			if err := tx.SetTimer(ctx, "k", "held", 1); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	start := time.Now()
	err := waiter.CreateUser(ctx, store.User{ID: "u1", Name: "a", CreatedAt: 1})
	if !errors.Is(err, store.ErrBusy) {
		t.Fatalf("write under lock: %v, want ErrBusy", err)
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("gave up after %v, before the busy timeout", waited)
	}
	err = waiter.Tx(ctx, func(ctx context.Context, tx store.Ops) error { return nil })
	if !errors.Is(err, store.ErrBusy) {
		t.Fatalf("begin under lock: %v, want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := waiter.GetUser(ctx, "u1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("busy write left a row: %v", err)
	}
	if err := waiter.CreateUser(ctx, store.User{ID: "u1", Name: "a", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	ts, err := waiter.ListTimers(ctx, store.TimerFilter{Limit: store.MaxPageSize})
	if err != nil || len(ts) != 1 {
		t.Fatalf("holder's commit: %v %v", ts, err)
	}
}

func TestMigrations(t *testing.T) {
	dir := t.TempDir()
	ms, err := migrations()
	if err != nil || len(ms) == 0 {
		t.Fatalf("embedded migrations: %d, %v", len(ms), err)
	}
	versions := func(d *DB) []int {
		t.Helper()
		rows, err := d.sqldb.QueryContext(t.Context(), `SELECT version, applied_at FROM schema_migrations ORDER BY version`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var vs []int
		for rows.Next() {
			var v int
			var at int64
			if err := rows.Scan(&v, &at); err != nil {
				t.Fatal(err)
			}
			if at < time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC).UnixMilli() {
				t.Fatalf("migration %d applied_at %d is not Unix milliseconds", v, at)
			}
			vs = append(vs, v)
		}
		return vs
	}
	want := []int{}
	for i := range ms {
		want = append(want, i+1)
	}

	d := openAt(t, dir, Options{})
	if got := versions(d); !slices.Equal(got, want) {
		t.Fatalf("versions = %v, want %v", got, want)
	}
	if err := d.CreateUser(t.Context(), store.User{ID: "u1", Name: "a", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	d.Close()

	// Reopening applies nothing again and keeps the data.
	d = openAt(t, dir, Options{})
	if got := versions(d); !slices.Equal(got, want) {
		t.Fatalf("versions after reopen = %v, want %v", got, want)
	}
	if _, err := d.GetUser(t.Context(), "u1"); err != nil {
		t.Fatal(err)
	}
	// A database migrated by a newer binary is refused.
	if _, err := d.sqldb.ExecContext(t.Context(), `INSERT INTO schema_migrations VALUES (?, 1)`, len(ms)+1); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if d, err := Open(t.Context(), filepath.Join(dir, dbName), Options{}); err == nil || !strings.Contains(err.Error(), "newer") {
		if d != nil {
			d.Close()
		}
		t.Fatalf("open newer database: %v", err)
	}
}

func TestOpenRejectsNonFilePaths(t *testing.T) {
	for _, p := range []string{"", ":memory:", "file:x.db", filepath.Join(t.TempDir(), "a?mode=ro")} {
		if d, err := Open(t.Context(), p, Options{}); err == nil {
			d.Close()
			t.Errorf("Open(%q) succeeded", p)
		}
	}
}

// TestSchemaGuards: the schema itself refuses what the Go checks refuse, so
// a bug that bypasses them still cannot redispatch a call, store long text
// or cross users.
func TestSchemaGuards(t *testing.T) {
	d := openAt(t, t.TempDir(), Options{})
	defer d.Close()
	ctx := t.Context()
	for _, err := range []error{
		d.CreateUser(ctx, store.User{ID: "u1", Name: "a", CreatedAt: 1}),
		d.CreateUser(ctx, store.User{ID: "u2", Name: "b", CreatedAt: 1}),
		d.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "d1", UserID: "u1", Name: "n", MachineID: "m", CreatedAt: 1}),
		d.CreateCall(ctx, store.Call{CallID: "c1", UserID: "u1", DeviceID: "d1", SessionID: "s", Tool: "bash",
			RequestHash: []byte("h"), State: store.CallAccepted, SubmittedAt: 1}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if ok, err := d.TransitionCall(ctx, "c1", store.CallExpect{States: []store.CallState{store.CallAccepted}},
		store.CallDispatched, store.CallPatch{DispatchedBootID: "b1"}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	raw := func(q string, args ...any) error {
		_, err := d.sqldb.ExecContext(ctx, q, args...)
		return mapErr(err)
	}
	for name, tc := range map[string]struct {
		q    string
		args []any
		want error
	}{
		"boot rewritten":   {`UPDATE calls SET dispatched_boot_id = 'b2' WHERE call_id = 'c1'`, nil, store.ErrInvalid},
		"boot cleared":     {`UPDATE calls SET dispatched_boot_id = NULL, state = 'lost' WHERE call_id = 'c1'`, nil, store.ErrInvalid},
		"long reason":      {`UPDATE calls SET outcome_reason = ? WHERE call_id = 'c1'`, []any{strings.Repeat("é", 129)}, store.ErrInvalid},
		"long fault":       {`UPDATE calls SET fault_code = ? WHERE call_id = 'c1'`, []any{strings.Repeat("x", 257)}, store.ErrInvalid},
		"call other user":  {`UPDATE calls SET user_id = 'u2' WHERE call_id = 'c1'`, nil, store.ErrNotFound},
		"job other user":   {`INSERT INTO jobs (device_id, boot_id, job_id, user_id, state, last_seq) VALUES ('d1', 'b1', 'j1', 'u2', 'running', 1)`, nil, store.ErrNotFound},
		"boot same value":  {`UPDATE calls SET dispatched_boot_id = 'b1' WHERE call_id = 'c1'`, nil, nil},
		"reason 256 bytes": {`UPDATE calls SET outcome_reason = ? WHERE call_id = 'c1'`, []any{strings.Repeat("é", 128)}, nil},
	} {
		if err := raw(tc.q, tc.args...); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	c, err := d.GetCall(ctx, "c1")
	if err != nil || c.DispatchedBootID != "b1" || c.UserID != "u1" || c.State != store.CallDispatched {
		t.Fatalf("call after refused writes: %+v %v", c, err)
	}
}

// TestCheckSettingsBusyTimeout: Open refuses a connection whose busy_timeout
// is not the configured one.
func TestCheckSettingsBusyTimeout(t *testing.T) {
	d := openAt(t, t.TempDir(), Options{BusyTimeout: 1234 * time.Millisecond})
	defer d.Close()
	if err := d.checkSettings(t.Context(), 1234*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := d.checkSettings(t.Context(), time.Second); err == nil {
		t.Fatal("busy_timeout mismatch accepted")
	}
	if d, err := Open(t.Context(), filepath.Join(t.TempDir(), dbName), Options{BusyTimeout: time.Microsecond}); err == nil {
		d.Close()
		t.Fatal("sub-millisecond busy timeout accepted")
	}
}
