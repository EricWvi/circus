package controlplane

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
)

func purgeTestServer(t *testing.T, logs *bytes.Buffer) (*Server, *sqlite.DB) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(t.Context(), st, Config{Listen: "127.0.0.1:0", DevInsecure: true, LogOutput: logs})
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

func TestEnrollmentCleanupIgnoresStaleClaim(t *testing.T) {
	var logs bytes.Buffer
	s, st := purgeTestServer(t, &logs)
	ctx := t.Context()
	rows, err := st.ListTimers(ctx, store.TimerFilter{Kind: timers.EnrollTokenExpiry, SubjectID: "purge", Limit: 1})
	if err != nil || len(rows) != 1 {
		t.Fatalf("timers=%+v err=%v", rows, err)
	}
	claims, err := st.ClaimDueTimers(ctx, rows[0].DueAt, "other", 10_000, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims=%+v err=%v", claims, err)
	}
	if err = st.SetTimer(ctx, timers.EnrollTokenExpiry, "purge", rows[0].DueAt+1000); err != nil {
		t.Fatal(err)
	}
	u, err := st.GetUserByName(ctx, auth.DefaultUserName)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = auth.CreateEnrollToken(ctx, st, u.ID, store.KindDevice, time.Second, time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = s.handleTimer(ctx, claims[0]); err != nil {
		t.Fatal(err)
	}
	// The stale claim must not purge. This direct purge proves the token remains.
	if n, err := st.PurgeEnrollTokens(ctx, time.Now().UnixMilli()); n != 1 || err != nil {
		t.Fatalf("stale claim had effects: purged=%d err=%v", n, err)
	}
}

func TestUnhandledTimerCompleted(t *testing.T) {
	for _, kind := range []string{"unknown_kind", timers.DeviceGrace} {
		t.Run(kind, func(t *testing.T) {
			var logs bytes.Buffer
			s, st := purgeTestServer(t, &logs)
			ctx := t.Context()
			if err := st.CancelTimer(ctx, timers.EnrollTokenExpiry, "purge"); err != nil {
				t.Fatal(err)
			}
			if err := st.SetTimer(ctx, kind, "subject", time.Now().UnixMilli()); err != nil {
				t.Fatal(err)
			}
			claims, err := st.ClaimDueTimers(ctx, time.Now().Add(time.Second).UnixMilli(), "owner", 10_000, 1)
			if err != nil || len(claims) != 1 {
				t.Fatalf("claims=%+v err=%v", claims, err)
			}
			if err = s.handleTimer(ctx, claims[0]); err != nil {
				t.Fatal(err)
			}
			rows, err := st.ListTimers(ctx, store.TimerFilter{Kind: kind, SubjectID: "subject", Limit: 1})
			if err != nil || len(rows) != 0 {
				t.Fatalf("unhandled timer survived: %+v err=%v", rows, err)
			}
			if err = s.handleTimer(ctx, claims[0]); err != nil {
				t.Fatal(err)
			}
			completedLogs := 0
			for line := range strings.Lines(logs.String()) {
				var rec struct {
					Msg       string `json:"msg"`
					Kind      string `json:"kind"`
					SubjectID string `json:"subject_id"`
				}
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("log line is not JSON: %q: %v", line, err)
				}
				if rec.Msg != "completed unhandled durable timer" {
					continue
				}
				completedLogs++
				if rec.Kind != kind || rec.SubjectID != "subject" {
					t.Fatalf("unhandled-timer log = %+v", rec)
				}
			}
			if completedLogs != 1 {
				t.Fatalf("unhandled-timer logs = %d, want 1:\n%s", completedLogs, logs.String())
			}
		})
	}
}
