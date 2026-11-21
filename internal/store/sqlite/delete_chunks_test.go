package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// TestDeleteStopsBetweenChunks: a delete stopped after its first
// transaction, as by a crash, leaves the transcript charges in step with
// the entries left, and calling DeleteSession again finishes it.
func TestDeleteStopsBetweenChunks(t *testing.T) {
	d := openAt(t, t.TempDir(), Options{})
	defer d.Close()
	ctx := context.Background()
	const now = int64(1_700_000_000_000)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(d.CreateUser(ctx, store.User{ID: "u1", Name: "u", CreatedAt: now}))
	must(d.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "d1", UserID: "u1", Name: "d", MachineID: "m", CreatedAt: now}))
	must(d.CreateIdentity(ctx, store.Identity{Kind: store.KindWorker, ID: "w1", UserID: "u1", Name: "w", MachineID: "m", CreatedAt: now}))
	_, errProject := d.CreateProject(ctx, store.Project{ID: "p1", UserID: "u1", Name: "Project", CreatedAt: now, UpdatedAt: now})
	must(errProject)
	for _, id := range []string{"s1", "keep"} {
		if _, err := d.CreateSession(ctx, store.Session{ID: id, ClientRequestID: id, UserID: "u1", ProjectID: "p1", ClaudeSessionID: "c-" + id, DefaultDeviceID: "d1", CreatedAt: now}, nil, 10); err != nil {
			t.Fatal(err)
		}
	}
	q := store.TranscriptQuota{MaxEntryBytes: 1 << 20, MaxSessionBytes: 1 << 30, MaxUserBytes: 1 << 30}
	fill := func(id string, n int) {
		r, err := d.AcquireLease(ctx, store.LeaseRequest{SessionID: id, WorkerID: "w1", BootID: "b1", ConnID: "c", Now: now, ExpiresAt: now + 1000})
		must(err)
		f := store.Fence{SessionID: id, Epoch: r.Lease.Epoch, WorkerID: "w1", BootID: "b1", ConnID: "c", Now: now}
		for i, seq := 0, int64(1); i < n; i, seq = i+500, seq+1 {
			var es []store.TranscriptEntry
			for j := i; j < min(i+500, n); j++ {
				es = append(es, store.TranscriptEntry{JSON: []byte(`{"n":1}`)})
			}
			_, err := d.AppendTranscript(ctx, f, store.TranscriptBatch{Subpath: "p", BatchSeq: seq, Entries: es}, q)
			must(err)
		}
		_, err = d.ReleaseLease(ctx, f)
		must(err)
	}
	fill("s1", 2*store.DeleteChunkRows+7)
	fill("keep", 3)
	_, _, _, err := d.StopSession(ctx, "s1", now+1, "")
	must(err)

	stop := errors.New("stop after one chunk")
	d.afterDeleteChunk = func() error { return stop }
	if err := d.DeleteSession(ctx, "s1", now+2000); !errors.Is(err, stop) {
		t.Fatalf("partial delete: %v", err)
	}
	d.afterDeleteChunk = nil
	// The counters match what is left, in the session, its subpath and the
	// user's total.
	var left, subpath, sess, user int64
	must(mapErr(d.sqldb.QueryRow(`SELECT COALESCE(SUM(bytes), 0) FROM transcript_entries WHERE session_id = 's1'`).Scan(&left)))
	must(mapErr(d.sqldb.QueryRow(`SELECT bytes FROM transcript_subpaths WHERE session_id = 's1'`).Scan(&subpath)))
	s1, err := d.GetSession(ctx, "s1", now)
	must(err)
	sess = s1.TranscriptBytes
	k, err := d.GetSession(ctx, "keep", now)
	must(err)
	u, err := d.GetUser(ctx, "u1")
	must(err)
	user = u.TranscriptBytes
	entry := store.TranscriptCharge(store.TranscriptEntry{JSON: []byte(`{"n":1}`)})
	if want := int64(store.DeleteChunkRows+7) * entry; left != want || subpath != left || sess != left || user != left+k.TranscriptBytes {
		t.Fatalf("after one chunk: entries %d (want %d), subpath %d, session %d, user %d (keep %d)", left, want, subpath, sess, user, k.TranscriptBytes)
	}
	must(d.DeleteSession(ctx, "s1", now+2000))
	u, err = d.GetUser(ctx, "u1")
	must(err)
	if u.TranscriptBytes != k.TranscriptBytes {
		t.Fatalf("user total after the delete: %d, want %d", u.TranscriptBytes, k.TranscriptBytes)
	}
}
