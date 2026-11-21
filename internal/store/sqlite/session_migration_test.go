package sqlite

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

func TestSessionClientRequestIDMigration(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), dbName)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	ms, err := migrations()
	must(err)
	_, err = raw.ExecContext(ctx, `CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, applied_at INTEGER NOT NULL) STRICT`)
	must(err)
	for _, m := range ms[:9] {
		_, err = raw.ExecContext(ctx, m.sql)
		must(err)
		_, err = raw.ExecContext(ctx, `INSERT INTO schema_migrations VALUES (?, 1)`, m.version)
		must(err)
	}
	_, err = raw.ExecContext(ctx, `
		INSERT INTO users(id,name,created_at) VALUES('u','user',1);
		INSERT INTO projects(id,user_id,name,name_key,created_at,updated_at) VALUES('p','u','Project','project',1,1);
		INSERT INTO devices(id,user_id,name,machine_id,created_at) VALUES('d','u','device','machine',1);
		INSERT INTO sessions(session_id,user_id,claude_session_id,state,default_device_id,created_at,updated_at,project_id,last_activity_at)
			VALUES('old1','u','claude1','idle','d',1,1,'p',1),('old2','u','claude2','idle','d',1,1,'p',1);`)
	must(err)
	must(raw.Close())

	d, err := Open(ctx, path, Options{})
	must(err)
	defer d.Close()
	for _, id := range []string{"old1", "old2"} {
		old, err := d.GetSession(ctx, id, 1)
		must(err)
		if old.ClientRequestID != "" {
			t.Fatalf("legacy session %s has a client request key: %q", id, old.ClientRequestID)
		}
		if _, err := d.GetSessionByClientRequestID(ctx, "p", id, 1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("lookup by legacy session id %s: %v", id, err)
		}
	}
	res, err := d.CreateSession(ctx, store.Session{
		ID: "new", ClientRequestID: "old1", UserID: "u", ProjectID: "p",
		ClaudeSessionID: "claude3", DefaultDeviceID: "d", CreatedAt: 2,
	}, nil, 10)
	must(err)
	if !res.Created || res.Session.ID != "new" {
		t.Fatalf("create with legacy session id as key = %+v", res)
	}
}

// TestSessionProjectMigration upgrades a populated pre-project-session schema
// through Open, including the real migrator's transaction and foreign keys.
func TestSessionProjectMigration(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), dbName)
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = raw.Close() })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	_, err = raw.ExecContext(ctx, `PRAGMA foreign_keys=ON`)
	must(err)
	ms, err := migrations()
	must(err)
	for _, m := range ms[:5] {
		_, err = raw.ExecContext(ctx, m.sql)
		must(err)
	}
	_, err = raw.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
            version    INTEGER PRIMARY KEY,
            applied_at INTEGER NOT NULL
        ) STRICT;
 INSERT INTO schema_migrations VALUES(1,1),(2,1),(3,1),(4,1),(5,1);
 INSERT INTO users(id,name,created_at,transcript_bytes,email,role) VALUES('owner','owner',1,67,'owner@example.com','user'),('web','web',1,0,'web@example.com','user');
 INSERT INTO devices(id,user_id,name,machine_id,created_at) VALUES('device','owner','device','machine',1);
 INSERT INTO workers(id,user_id,name,machine_id,created_at) VALUES('worker','owner','worker','machine',1);
 INSERT INTO projects(id,user_id,name,name_key,created_at,updated_at) VALUES('project','web','Project','project',1,1);
 INSERT INTO project_locations(project_id,position,device_id,path) VALUES('project',0,'device','/work');
 INSERT INTO sessions(session_id,user_id,claude_session_id,state,default_device_id,last_turn_outcome,created_at,updated_at,transcript_bytes)
 VALUES('old','owner','claude-old','running','device','interrupted',1,1,67);
 INSERT INTO session_leases(session_id,epoch,holder_worker_id,holder_boot_id,holder_conn_id,acquired_at,expires_at)
 VALUES('old',1,'worker','boot','conn',1,100);
 INSERT INTO transcript_subpaths(session_id,subpath,last_seq,bytes) VALUES('old','',1,67);
 INSERT INTO transcript_entries(session_id,subpath,seq,uuid,entry,bytes,epoch) VALUES('old','',1,'u',X'7B7D',67,1);
 INSERT INTO session_inputs(input_id,session_id,client_msg_id,kind,text,priority,state,created_at) VALUES('input','old','client','user','hello','normal','pending',1);
 INSERT INTO session_turns(session_id,turn_no,epoch,local_seq,origin,started_at,ended_at,outcome) VALUES('old',1,1,1,'input',1,2,'interrupted');
 INSERT INTO session_events(session_id,seq,at,kind,payload,payload_bytes) VALUES('old',1,1,'ASSISTANT',X'7B7D',2);
 INSERT INTO calls(call_id,user_id,device_id,session_id,tool,request_hash,request_bytes,state,submitted_at)
 VALUES('call','owner','device','old','bash',X'01',1,'accepted',1),('unrelated','owner','device','never-a-session','bash',X'02',1,'accepted',1);
 INSERT INTO jobs(device_id,boot_id,job_id,user_id,session_id,call_id,state,last_seq) VALUES('device','boot','job','owner','old','call','running',1);
 INSERT INTO session_cwds(session_id,agent_id,device_id,cwd,updated_at) VALUES('old','','device','/work',1);
 INSERT INTO session_files(session_id,device_id,path,sha256,mtime_ms,touched_at) VALUES('old','device','/work/file',zeroblob(32),1,1);
 INSERT INTO session_jobs(session_id,call_id,device_id,boot_id,job_id,created_at) VALUES('old','call','device','boot','job',1);
 INSERT INTO session_bgtasks(session_id,task_id,kind,epoch,started_at) VALUES('old','task','agent',1,1);
 INSERT INTO session_approvals(approval_id,session_id,user_id,tool,device_id,match_key,state,created_at,expires_at)
 VALUES('approval','old','owner','bash','device',replace(hex(zeroblob(32)),'0','a'),'pending',1,100);
 INSERT INTO session_grants(grant_id,session_id,tool,device_id,match_key,created_at,expires_at)
 VALUES('grant','old','bash','device',replace(hex(zeroblob(32)),'0','a'),1,100);`)
	must(err)
	dependents := []string{"sessions", "session_leases", "transcript_subpaths", "transcript_entries", "session_inputs", "session_turns", "session_events", "session_cwds", "session_files", "session_jobs", "session_bgtasks", "session_approvals", "session_grants", "jobs"}
	for _, table := range dependents {
		var count int
		must(raw.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count))
		if count != 1 {
			t.Fatalf("fixture %s has %d rows", table, count)
		}
	}
	must(raw.Close())
	d, err := Open(ctx, path, Options{})
	must(err)
	defer d.Close()
	// Compare every table, index (including autoindexes) and trigger against
	// a fresh database. Physical root pages depend on data and are excluded.
	fresh := openAt(t, t.TempDir(), Options{})
	defer fresh.Close()
	type definition struct {
		Type, Name, Table string
		SQL               sql.NullString
	}
	schema := func(db *DB) []definition {
		t.Helper()
		rows, err := db.q.QueryContext(ctx, `SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY type, name`)
		must(err)
		defer rows.Close()
		var out []definition
		for rows.Next() {
			var entry definition
			must(rows.Scan(&entry.Type, &entry.Name, &entry.Table, &entry.SQL))
			// The legacy fixture's hand-written migration ledger may differ
			// in indentation, which is not a schema difference.
			if entry.Name == "schema_migrations" {
				entry.SQL.String = strings.Join(strings.Fields(entry.SQL.String), " ")
			}
			out = append(out, entry)
		}
		must(rows.Err())
		return out
	}
	if got, want := schema(d), schema(fresh); !reflect.DeepEqual(got, want) {
		t.Fatalf("migrated schema differs from fresh: got %#v, want %#v", got, want)
	}
	// Both creation paths apply the same migration, so also assert the
	// required session indexes independently of their equality.
	var indexes []string
	for _, entry := range schema(d) {
		if entry.Type == "index" && entry.Table == "sessions" && entry.SQL.Valid {
			indexes = append(indexes, entry.Name)
		}
	}
	if want := []string{"sessions_keyset", "sessions_project_client_request_id", "sessions_project_keyset", "sessions_queued", "sessions_user_keyset"}; !reflect.DeepEqual(indexes, want) {
		t.Fatalf("session indexes = %v, want %v", indexes, want)
	}
	for _, table := range dependents {
		var count int
		must(d.q.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count))
		if count != 0 {
			t.Fatalf("%s retained %d session rows", table, count)
		}
	}
	for table, want := range map[string]int{"users": 2, "devices": 1, "workers": 1, "projects": 1, "project_locations": 1, "calls": 1} {
		var count int
		must(d.q.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count))
		if count != want {
			t.Fatalf("%s count %d, want %d", table, count, want)
		}
	}
	owner, err := d.GetUser(ctx, "owner")
	must(err)
	if owner.TranscriptBytes != 0 {
		t.Fatalf("transcript charge %d", owner.TranscriptBytes)
	}
	rows, err := d.q.QueryContext(ctx, `PRAGMA foreign_key_check`)
	must(err)
	if rows.Next() {
		t.Fatal("foreign key violation after migration")
	}
	must(rows.Err())
	must(rows.Close())
	var fk int
	must(d.q.QueryRow(ctx, `PRAGMA foreign_keys`).Scan(&fk))
	if fk != 1 {
		t.Fatal("foreign keys disabled")
	}
	// New sessions use a project owned by the same user. All historical
	// outcomes remain valid, and stopped works in both checks.
	_, err = d.CreateProject(ctx, store.Project{ID: "new-project", UserID: "owner", Name: "New", CreatedAt: 10, UpdatedAt: 10})
	must(err)
	for i, outcome := range []store.TurnOutcome{store.TurnCompleted, store.TurnInterrupted, store.TurnError, store.TurnStopped} {
		id := fmt.Sprintf("new%d", i)
		if _, err := d.CreateSession(ctx, store.Session{ID: id, ClientRequestID: id, UserID: "owner", ProjectID: "new-project", ClaudeSessionID: id, DefaultDeviceID: "device", CreatedAt: 10}, nil, 100); err != nil {
			t.Fatal(err)
		}
		lease, err := d.AcquireLease(ctx, store.LeaseRequest{SessionID: id, WorkerID: "worker", BootID: "boot", ConnID: "conn", Now: 10, ExpiresAt: 100})
		must(err)
		f := store.Fence{SessionID: id, Epoch: lease.Lease.Epoch, WorkerID: "worker", BootID: "boot", ConnID: "conn", Now: 11}
		_, err = d.ReportTurn(ctx, f, store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted, Origin: store.TurnFromInput, At: 11})
		must(err)
		result, err := d.ReportTurn(ctx, f, store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: outcome, At: 12})
		must(err)
		if result.Turn.Outcome != outcome || result.Session.LastTurnOutcome != outcome {
			t.Fatalf("outcome lost: %+v", result)
		}
	}
	// Child-table FKs still name sessions after the root rebuild.
	var target string
	must(d.q.QueryRow(ctx, `SELECT "table" FROM pragma_foreign_key_list('session_approvals')`).Scan(&target))
	if target != "sessions" {
		t.Fatalf("approval foreign key targets %s", target)
	}
	for _, table := range []string{"sessions", "session_turns"} {
		var ddl string
		must(d.q.QueryRow(ctx, `SELECT sql FROM sqlite_master WHERE name=?`, table).Scan(&ddl))
		if !strings.Contains(ddl, "'stopped'") {
			t.Fatalf("missing stopped check: %s", table)
		}
	}
}
