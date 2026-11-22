// Package dbtemplate gives tests fresh copies of a database file that is
// built once per test process.
//
// Building a database from scratch (running every schema migration) is slow,
// above all under the race detector, and tests that each start from an empty
// schema pay that cost again and again. A Template builds the database once,
// keeps the finished file's bytes in memory and writes a byte copy wherever a
// test asks for one. The copy is an ordinary file: the code under test opens
// it the usual way and finds the schema already in place.
//
// Tests that are about how a database file comes to be (migrations, empty or
// corrupt files, settings applied at creation) build theirs the real way.
package dbtemplate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Template is a database file built once per process on first use. It is
// safe for concurrent use.
type Template struct {
	build func(path string) error

	once sync.Once
	data []byte
	err  error
}

// New returns a template built by build, which creates a database file at
// path and returns only once that file is complete and closed: the directory
// holding path must contain nothing else afterwards (for SQLite in WAL mode,
// no -wal or -shm file), so the single file is the whole database.
func New(build func(path string) error) *Template {
	return &Template{build: build}
}

func (tp *Template) load() ([]byte, error) {
	tp.once.Do(func() { tp.data, tp.err = tp.make() })
	return tp.data, tp.err
}

func (tp *Template) make() (data []byte, err error) {
	dir, err := os.MkdirTemp("", "dbtemplate-")
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "template.db")
	if err := tp.build(path); err != nil {
		return nil, fmt.Errorf("dbtemplate: build: %w", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Name() != filepath.Base(path) {
			return nil, fmt.Errorf("dbtemplate: build left %s beside the database file", e.Name())
		}
	}
	data, err = os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, errors.New("dbtemplate: build left an empty database file")
	}
	return data, nil
}

// Seed creates path as a copy of the template, building the template first
// if this process has not yet. path must not exist; Seed fails t if it does.
// The file appears complete or not at all, so a concurrent opener of path
// never sees a partial copy.
func (tp *Template) Seed(t testing.TB, path string) {
	t.Helper()
	if err := tp.seed(path); err != nil {
		t.Fatal(err)
	}
}

func (tp *Template) seed(path string) (err error) {
	data, err := tp.load()
	if err != nil {
		return err
	}
	// Write beside path and link into place: the link fails if path exists
	// and never exposes a half-written file.
	f, err := os.CreateTemp(filepath.Dir(path), ".dbtemplate-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.Remove(f.Name())) }()
	if _, err := f.Write(data); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(f.Name(), path); err != nil {
		return fmt.Errorf("dbtemplate: %w", err)
	}
	return nil
}
