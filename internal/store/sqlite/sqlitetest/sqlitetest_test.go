package sqlitetest_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
)

// schema lists the database's schema objects and applied migration versions,
// read without sqlite.Open so that reading migrates nothing.
func schema(t *testing.T, path string) []string {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var out []string
	rows, err := raw.QueryContext(t.Context(), `SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var typ, name, table, def string
		if err := rows.Scan(&typ, &name, &table, &def); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s on %s: %s", typ, name, table, def))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows, err = raw.QueryContext(t.Context(), `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("migration %d", v))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// A seeded database has exactly the schema sqlite.Open builds from scratch,
// and opens and writes like one.
func TestSeededMatchesMigrated(t *testing.T) {
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	db, err := sqlite.Open(t.Context(), fresh, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	seeded := sqlitetest.Path(t)
	want, got := schema(t, fresh), schema(t, seeded)
	if len(want) == 0 || !slices.Equal(got, want) {
		t.Fatalf("seeded schema differs from a migrated one:\n got %q\nwant %q", got, want)
	}
	db, err = sqlite.Open(t.Context(), seeded, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.CreateUser(t.Context(), store.User{ID: "u1", Name: "a", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
}

// Concurrent seeds each get a complete, independent database.
func TestSeedConcurrent(t *testing.T) {
	const n = 8
	paths := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		dir := t.TempDir()
		wg.Go(func() {
			paths[i] = filepath.Join(dir, "db")
			sqlitetest.Seed(t, paths[i])
		})
	}
	wg.Wait()
	for i, p := range paths {
		db, err := sqlite.Open(t.Context(), p, sqlite.Options{})
		if err != nil {
			t.Fatal(err)
		}
		// The same id in every database: each starts empty.
		if err := db.CreateUser(t.Context(), store.User{ID: "u1", Name: "a", CreatedAt: 1}); err != nil {
			t.Fatalf("database %d: %v", i, err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		entries, err := os.ReadDir(filepath.Dir(p))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("directory of database %d holds %d entries, want only the database", i, len(entries))
		}
	}
}
