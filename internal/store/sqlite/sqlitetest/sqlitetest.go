// Package sqlitetest gives tests fresh, fully migrated SQLite databases
// without running the migrations for each one.
//
// The first use in a test process migrates one template database with
// sqlite.Open and closes it; every later database is a byte copy of that file
// (see dbtemplate). Tests open the copy with sqlite.Open as usual, so the
// connection settings are checked and the migration check runs, finding
// nothing to apply.
//
// Tests that depend on how a database file is created or migrated (a schema
// at a particular version, an empty or corrupt file, faults during Open)
// still call sqlite.Open on a path that holds no file.
package sqlitetest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/testutil/dbtemplate"
)

var template = dbtemplate.New(func(path string) error {
	db, err := sqlite.Open(context.Background(), path, sqlite.Options{})
	if err != nil {
		return err
	}
	// Close returns after the driver has closed the last connection, which
	// checkpoints the WAL into the database file and removes the -wal and
	// -shm files; dbtemplate checks that nothing else is left.
	return db.Close()
})

// Seed creates a fresh, fully migrated database file at path, which must not
// exist.
func Seed(t testing.TB, path string) {
	t.Helper()
	template.Seed(t, path)
}

// Path creates a fresh, fully migrated database file in a new t.TempDir and
// returns its path.
func Path(t testing.TB) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "circus.db")
	Seed(t, path)
	return path
}
