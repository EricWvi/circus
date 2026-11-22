package controlplane_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/ledger"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
)

type failLedgerAdvance struct {
	store.LedgerOps
}

func (failLedgerAdvance) AdvanceLedgerIndex(context.Context, string, string, string, []store.LedgerEntry, []store.LedgerVersion) error {
	return store.ErrBusy
}

func TestServerRebuildsLedgerAtStartup(t *testing.T) {
	ctx := t.Context()
	dbPath := sqlitetest.Path(t)
	st, err := sqlite.Open(ctx, dbPath, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now().UnixMilli()
	if err := st.CreateUser(ctx, store.User{ID: "owner", Name: "Owner", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	const id = "startup"
	if _, err := st.CreateProject(ctx, store.Project{ID: id, UserID: "owner", Name: "Project", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	repos, err := ledger.New(dbPath, failLedgerAdvance{st}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	head, err := repos.CommitMain(ctx, id, []ledger.Decision{{Title: "Original"}}, ledger.CommitOptions{Author: "import", Message: "Seed"})
	if !errors.Is(err, store.ErrBusy) || head == "" {
		t.Fatalf("simulated interruption = %q, %v", head, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dbPath), "ledger", id+".git")); err != nil {
		t.Fatal(err)
	}
	old, err := st.GetLedgerHead(ctx, id)
	if err != nil || old != "" {
		t.Fatalf("old indexed head = %q, %v", old, err)
	}
	// A separate malformed repository must not stop this startup or
	// prevent rebuilding the stale, healthy project.
	if _, err := st.CreateProject(ctx, store.Project{ID: "broken", UserID: "owner", Name: "Broken", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(dbPath), "ledger", "broken.git"), []byte("not a repository"), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	server, err := controlplane.New(ctx, st, controlplane.Config{DBPath: dbPath, Listen: "127.0.0.1:0", DevInsecure: true, LogOutput: &logs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), controlplane.DefaultShutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			t.Error(err)
		}
	})
	if !strings.Contains(logs.String(), "ledger repository skipped at startup") || !strings.Contains(logs.String(), "broken.git") {
		t.Fatalf("missing repository diagnostic: %s", logs.String())
	}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	index, err := st.GetLedgerIndex(ctx, id)
	if err != nil || index.Head != head || len(index.Entries) != 1 || index.Entries[0].Title != "Original" {
		t.Fatalf("rebuilt index = %+v, %v", index, err)
	}
}
