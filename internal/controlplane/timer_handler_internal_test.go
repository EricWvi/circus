package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/timers"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
)

func purgeTestServer(t *testing.T, logs *bytes.Buffer) (*Server, *sqlite.DB) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s, err := New(t.Context(), st, Config{Listen: "127.0.0.1:0", DevInsecure: true, LogOutput: logs})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
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
	u := createMachineTestUser(t, st)
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
	for _, kind := range []string{"unknown_kind"} {
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

func TestPeriodicCleanupPurgesWebSessions(t *testing.T) {
	var logs bytes.Buffer
	s, st := purgeTestServer(t, &logs)
	now := time.Now()
	u := createMachineTestUser(t, st)
	created := now.Add(-store.WebSessionIdleTimeout - time.Second).UnixMilli()
	expired := store.WebSession{TokenHash: auth.HashToken("expired"), UserID: u.ID, CreatedAt: created, LastUsedAt: created, ExpiresAt: now.Add(-time.Second).UnixMilli()}
	active := store.WebSession{TokenHash: auth.HashToken("active"), UserID: u.ID, CreatedAt: now.UnixMilli(), LastUsedAt: now.UnixMilli(), ExpiresAt: now.Add(store.WebSessionIdleTimeout).UnixMilli()}
	for _, sess := range []store.WebSession{expired, active} {
		if err := st.CreateWebSession(t.Context(), sess); err != nil {
			t.Fatal(err)
		}
	}
	claims, err := st.ClaimDueTimers(t.Context(), now.UnixMilli(), "cleanup", 10000, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims: %v %v", claims, err)
	}
	if err = s.handleTimer(t.Context(), claims[0]); err != nil {
		t.Fatal(err)
	}
	if err = st.CreateWebSession(t.Context(), expired); err != nil {
		t.Fatal("expired row was not physically deleted", err)
	}
	if _, err = st.UseWebSession(t.Context(), active.TokenHash, now.UnixMilli()); err != nil {
		t.Fatal("cleanup removed active session", err)
	}
	if !strings.Contains(logs.String(), "purged expired web sessions") {
		t.Fatal("cleanup count was not logged")
	}
	// Reusing the completed timer claim cannot purge a row inserted afterward.
	if err = s.handleTimer(t.Context(), claims[0]); err != nil {
		t.Fatal(err)
	}
	if n, err := st.PurgeWebSessions(t.Context(), now.UnixMilli()); err != nil || n != 1 {
		t.Fatalf("stale timer had effects: %d %v", n, err)
	}
}

func TestSetupBannerFallback(t *testing.T) {
	for _, tc := range []struct {
		name, listen string
		proxy        bool
		wantPrefix   string
		wantHint     bool
	}{
		{"tls empty host", ":8443", false, "https://localhost:8443/setup?token=", false},
		{"proxy empty host", ":8443", true, "/setup?token=", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := sqlite.Open(t.Context(), sqlitetest.Path(t), sqlite.Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			var logs bytes.Buffer
			cfg := Config{Listen: tc.listen, BehindProxy: tc.proxy, LogOutput: &logs, BcryptCost: 4}
			if !tc.proxy {
				cfg.TLSCert = "not-opened.pem"
				cfg.TLSKey = "not-opened-key.pem"
			}
			s, err := New(t.Context(), st, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Shutdown(context.Background())
			found := false
			for line := range strings.Lines(logs.String()) {
				var rec struct{ URL, Hint string }
				if json.Unmarshal([]byte(line), &rec) != nil || rec.URL == "" {
					continue
				}
				found = true
				if !strings.HasPrefix(rec.URL, tc.wantPrefix) {
					t.Fatal("wrong setup link fallback (token redacted)")
				}
				if strings.Contains(rec.Hint, "public_url") != tc.wantHint {
					t.Fatal("missing public_url hint for proxy")
				}
			}
			if !found {
				t.Fatal("no setup banner")
			}
		})
	}
}

func createMachineTestUser(t *testing.T, st store.Store) store.User {
	t.Helper()
	u := store.User{ID: "owner", Email: "owner@example.com", Role: store.RoleUser, CreatedAt: time.Now().UnixMilli()}
	if err := st.CreateUser(t.Context(), u); err != nil {
		t.Fatal(err)
	}
	return u
}
