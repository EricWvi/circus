package sqlite

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
)

// TestDeleteFullSessionTiming measures DeleteSession on a session holding
// a full default transcript (store.DefaultMaxTranscriptBytes). It writes
// that much to disk, so it runs only with CIRCUS_MEASURE_DELETE=1.
func TestDeleteFullSessionTiming(t *testing.T) {
	if os.Getenv("CIRCUS_MEASURE_DELETE") == "" {
		t.Skip("set CIRCUS_MEASURE_DELETE=1 to measure")
	}
	d := openSeeded(t, t.TempDir(), Options{})
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
	if _, err := d.CreateSession(ctx, store.Session{ID: "s1", ClientRequestID: "s1", UserID: "u1", ProjectID: "p1", ClaudeSessionID: "c1", DefaultDeviceID: "d1", CreatedAt: now}, nil, 10); err != nil {
		t.Fatal(err)
	}
	r, err := d.AcquireLease(ctx, store.LeaseRequest{SessionID: "s1", WorkerID: "w1", BootID: "b1", ConnID: "c1", Now: now, ExpiresAt: now + 3_600_000})
	must(err)
	f := store.Fence{SessionID: "s1", Epoch: r.Lease.Epoch, WorkerID: "w1", BootID: "b1", ConnID: "c1", Now: now}
	// CIRCUS_MEASURE_ENTRY_BYTES sets the JSON size of an entry (default
	// 64 KiB); smaller entries mean more rows to delete. Each entry also
	// carries a 36-byte uuid.
	entryBytes := 64 << 10
	if v := os.Getenv("CIRCUS_MEASURE_ENTRY_BYTES"); v != "" {
		n, err := strconv.Atoi(v)
		must(err)
		entryBytes = n
	}
	perBatch := max(1, min(4096, (4<<20)/entryBytes))
	body := bytes.Repeat([]byte("x"), entryBytes)
	q := store.TranscriptQuota{MaxEntryBytes: store.MaxTranscriptPageBytes, MaxSessionBytes: store.DefaultMaxTranscriptBytes, MaxUserBytes: store.DefaultMaxUserTranscriptBytes}
	start := time.Now()
	charge := store.TranscriptCharge(store.TranscriptEntry{UUID: fmt.Sprintf("%036d", 0), JSON: body})
	n := int(store.DefaultMaxTranscriptBytes / charge)
	for i := 0; i < n; i += perBatch {
		es := make([]store.TranscriptEntry, perBatch)
		for j := range es {
			if i+j >= n {
				es = es[:j]
				break
			}
			es[j] = store.TranscriptEntry{UUID: fmt.Sprintf("%036d", i+j), JSON: body}
		}
		_, err := d.AppendTranscript(ctx, f, store.TranscriptBatch{BatchSeq: int64(i/perBatch + 1), Entries: es}, q)
		must(err)
	}
	t.Logf("appended %d MiB in %d entries: %v", store.DefaultMaxTranscriptBytes>>20, n, time.Since(start))
	_, err = d.ReleaseLease(ctx, f)
	must(err)
	_, _, _, err = d.StopSession(ctx, "s1", now+1, "")
	must(err)
	start = time.Now()
	must(d.DeleteSession(ctx, "s1", now+2))
	took := time.Since(start)
	chunks := (n + store.DeleteChunkRows - 1) / store.DeleteChunkRows
	t.Logf("DeleteSession of a %d MiB transcript (%d rows): %v in %d transactions, %v each on average",
		store.DefaultMaxTranscriptBytes>>20, n, took, chunks+1, took/time.Duration(chunks+1))
}
