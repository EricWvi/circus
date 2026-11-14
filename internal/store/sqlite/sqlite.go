// Package sqlite implements store.Store on SQLite through modernc.org/sqlite
// (pure Go, no cgo).
//
// Every connection runs in WAL mode with synchronous=FULL, foreign_keys=ON
// and a busy_timeout. FULL makes each commit durable before Tx returns: if a
// "dispatched" row were lost on power failure while the daemon also
// restarted, the call could be sent to the new boot and run twice. The pool holds a single connection, so the process is the single
// writer; transactions begin IMMEDIATE, so a second process on the same file
// (for example "circus server enroll-token create") waits for the write lock
// up to the busy timeout instead of failing mid-transaction. Lock timeouts
// surface as store.ErrBusy.
//
// The schema lives in embedded, ordered migrations (migrations/NNNN_*.sql)
// recorded in schema_migrations. Open applies the pending ones and refuses a
// database migrated by a newer binary.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"time"

	msqlite "modernc.org/sqlite"

	"github.com/EricWvi/circus/internal/store"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Options configures Open.
type Options struct {
	// BusyTimeout bounds how long a statement waits for a lock held by
	// another connection before failing with store.ErrBusy. Zero means five
	// seconds.
	BusyTimeout time.Duration
}

// DB is the SQLite store. Its Ops methods each run as one transaction.
type DB struct {
	ops
	sqldb *sql.DB
}

var _ store.Store = (*DB)(nil)

// Open opens or creates the database file at path and migrates it.
func Open(ctx context.Context, path string, opts Options) (*DB, error) {
	if path == "" || strings.ContainsAny(path, "?#") || strings.HasPrefix(path, "file:") || path == ":memory:" {
		return nil, fmt.Errorf("sqlite: unsupported database path %q", path)
	}
	busy := opts.BusyTimeout
	if busy == 0 {
		busy = 5 * time.Second
	}
	if busy < time.Millisecond {
		return nil, fmt.Errorf("sqlite: busy timeout %v is below 1ms", busy)
	}
	dsn := path + "?_pragma=busy_timeout(" + strconv.FormatInt(busy.Milliseconds(), 10) + ")" +
		"&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)
	sqldb.SetConnMaxLifetime(0)
	sqldb.SetConnMaxIdleTime(0)
	d := &DB{sqldb: sqldb}
	d.ops = ops{q: poolQuerier{d}, db: d}
	if err := d.checkSettings(ctx, busy); err != nil {
		sqldb.Close()
		return nil, err
	}
	if err := d.migrate(ctx); err != nil {
		sqldb.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the database.
func (d *DB) Close() error { return d.sqldb.Close() }

// Tx implements store.Store. It is serializable: BEGIN IMMEDIATE takes the
// database's single write lock before fn runs, so the transactions of every
// handle and process on the file run one at a time.
func (d *DB) Tx(ctx context.Context, fn func(ctx context.Context, tx store.Ops) error) (err error) {
	if err := store.CheckOutsideTx(ctx, d); err != nil {
		return err // a nested Tx would wait for this connection forever
	}
	tx, err := d.sqldb.BeginTx(ctx, nil)
	if err != nil {
		return mapErr(err)
	}
	txCtx, exit := store.EnterTx(ctx, d)
	defer exit()
	done := false
	defer func() {
		if !done {
			// fn panicked.
			_ = tx.Rollback()
		}
	}()
	err = fn(txCtx, &ops{q: txQuerier{tx}})
	done = true
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	return mapErr(tx.Commit())
}

// settings are the per-connection pragmas Open requires.
type settings struct {
	JournalMode string
	Synchronous int // 2 = FULL
	ForeignKeys int
	BusyTimeout int // milliseconds
}

func readSettings(ctx context.Context, q querier) (settings, error) {
	var s settings
	for _, p := range []struct {
		name string
		dst  any
	}{
		{"journal_mode", &s.JournalMode},
		{"synchronous", &s.Synchronous},
		{"foreign_keys", &s.ForeignKeys},
		{"busy_timeout", &s.BusyTimeout},
	} {
		if err := q.QueryRow(ctx, "PRAGMA "+p.name).Scan(p.dst); err != nil {
			return s, fmt.Errorf("sqlite: read pragma %s: %w", p.name, mapErr(err))
		}
	}
	return s, nil
}

func (d *DB) checkSettings(ctx context.Context, busy time.Duration) error {
	s, err := readSettings(ctx, d.q)
	if err != nil {
		return err
	}
	if !strings.EqualFold(s.JournalMode, "wal") || s.Synchronous != 2 || s.ForeignKeys != 1 ||
		int64(s.BusyTimeout) != busy.Milliseconds() {
		return fmt.Errorf("sqlite: connection settings not in effect: %+v", s)
	}
	return nil
}

type migration struct {
	version int
	name    string
	sql     string
}

func migrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		num, _, ok := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(num)
		if !ok || err != nil || v != len(ms)+1 {
			return nil, fmt.Errorf("sqlite: migration %s out of sequence", e.Name())
		}
		body, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, err
		}
		ms = append(ms, migration{version: v, name: e.Name(), sql: string(body)})
	}
	return ms, nil
}

func (d *DB) migrate(ctx context.Context) error {
	ms, err := migrations()
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		return errors.New("sqlite: no migrations embedded")
	}
	// One transaction: DDL is transactional in SQLite, so a failed migration
	// leaves the previous schema intact.
	return d.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		q := tx.(*ops).q
		if _, err := q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
			version    INTEGER PRIMARY KEY,
			applied_at INTEGER NOT NULL
		) STRICT`); err != nil {
			return fmt.Errorf("sqlite: create schema_migrations: %w", mapErr(err))
		}
		var current int
		if err := q.QueryRow(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&current); err != nil {
			return mapErr(err)
		}
		if current > len(ms) {
			return fmt.Errorf("sqlite: database schema version %d is newer than this binary's %d", current, len(ms))
		}
		for _, m := range ms[current:] {
			if _, err := q.ExecContext(ctx, m.sql); err != nil {
				return fmt.Errorf("sqlite: apply migration %s: %w", m.name, mapErr(err))
			}
			if _, err := q.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at)
				VALUES (?, CAST(unixepoch('subsec') * 1000 AS INTEGER))`, m.version); err != nil {
				return fmt.Errorf("sqlite: record migration %s: %w", m.name, mapErr(err))
			}
		}
		return nil
	})
}

// SQLite primary result codes and the extended constraint codes mapErr
// distinguishes.
const (
	codeBusy             = 5
	codeLocked           = 6
	codeConstraint       = 19
	codeConstraintFK     = 787
	codeConstraintPK     = 1555
	codeConstraintUnique = 2067
)

// mapErr turns driver errors into store sentinels, keeping the driver's
// message but not its type.
func mapErr(err error) error {
	if err == nil {
		return nil
	}
	var se *msqlite.Error
	if !errors.As(err, &se) {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	switch code := se.Code(); {
	case code&0xff == codeBusy || code&0xff == codeLocked:
		return fmt.Errorf("%w: %s", store.ErrBusy, se.Error())
	case code == codeConstraintPK || code == codeConstraintUnique:
		return fmt.Errorf("%w: %s", store.ErrExists, se.Error())
	case code == codeConstraintFK:
		return fmt.Errorf("%w: referenced row missing: %s", store.ErrNotFound, se.Error())
	case code&0xff == codeConstraint:
		return fmt.Errorf("%w: %s", store.ErrInvalid, se.Error())
	}
	return fmt.Errorf("sqlite: %s", se.Error())
}
