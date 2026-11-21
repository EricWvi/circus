package sqlite

import (
	"errors"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func TestSessionClientRequestIDIndex(t *testing.T) {
	d := openAt(t, t.TempDir(), Options{})
	defer d.Close()
	ctx := t.Context()
	const now = 1
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.CreateUser(ctx, store.User{ID: "owner", Name: "Owner", CreatedAt: now}))
	must(d.CreateIdentity(ctx, store.Identity{
		ID: "device", Kind: store.KindDevice, UserID: "owner", Name: "desk", MachineID: "machine", CreatedAt: now,
	}))
	for _, project := range []string{"first", "second"} {
		_, err := d.CreateProject(ctx, store.Project{ID: project, UserID: "owner", Name: project, CreatedAt: now, UpdatedAt: now})
		must(err)
	}
	_, err := d.CreateSession(ctx, store.Session{
		ID: "original", ClientRequestID: "shared", ProjectID: "first", UserID: "owner", ClaudeSessionID: "claude-original",
		DefaultDeviceID: "device", CreatedAt: now,
	}, nil, 10)
	must(err)

	// Bypass CreateSession's lookup to exercise the database constraint.
	insert := func(id, project string, key any) (int, error) {
		return d.exec(ctx, `INSERT INTO sessions (session_id, claude_session_id, project_id, client_request_id,
			user_id, state, default_device_id, created_at, updated_at, last_activity_at)
			SELECT ?, ?, ?, ?, user_id, state, default_device_id, created_at, updated_at, last_activity_at
			FROM sessions WHERE session_id = 'original'`, id, "claude-"+id, project, key)
	}
	if _, err := insert("duplicate", "first", "shared"); !errors.Is(err, store.ErrExists) {
		t.Fatalf("duplicate project key: got %v, want %v", err, store.ErrExists)
	}
	for _, row := range []struct {
		id, project string
		key         any
	}{
		{"other-project", "second", "shared"},
		{"legacy-one", "first", nil},
		{"legacy-two", "first", nil},
	} {
		if n, err := insert(row.id, row.project, row.key); err != nil || n != 1 {
			t.Fatalf("insert %s: affected %d rows, error %v", row.id, n, err)
		}
	}

	var id, parent, unused int
	var detail string
	must(d.sqldb.QueryRowContext(ctx, `EXPLAIN QUERY PLAN `+sessionByClientRequestIDQuery,
		now, "first", "shared").Scan(&id, &parent, &unused, &detail))
	if !strings.Contains(detail, "USING INDEX sessions_project_client_request_id (project_id=? AND client_request_id=?)") {
		t.Fatalf("client request key lookup plan: %q", detail)
	}
}
