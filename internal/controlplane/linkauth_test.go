package controlplane

import (
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
)

func TestDaemonAuthUsesAcceptedCredentialInstance(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "db")
	st, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	user, err := auth.EnsureUser(ctx, st, "admin", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	enroll := func(kind store.PrincipalKind) auth.Enrollment {
		token, _, err := auth.CreateEnrollToken(ctx, st, user.ID, kind, time.Hour, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		e, err := auth.Enroll(ctx, st, auth.EnrollRequest{Token: token, Kind: kind, Name: "desk", MachineID: "machine", Hostname: "host", OS: "linux", Arch: "amd64"}, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	daemon, worker := enroll(store.KindDevice), enroll(store.KindWorker)
	a := daemonAuth{auth.NewAuthenticator(st, auth.Options{}), st}
	req := httptest.NewRequest("GET", "/v1/link/daemon", nil)
	if _, err = a.Authenticate(ctx, req); !errors.Is(err, gateway.ErrUnauthenticated) {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+worker.Token)
	if _, err = a.Authenticate(ctx, req); !errors.Is(err, gateway.ErrWrongRole) {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+daemon.Token)
	p, err := a.Authenticate(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if p.UserID != user.ID || p.DeviceID != daemon.Identity.ID || p.DeviceName != "desk" || p.CredentialID != daemon.CredentialID {
		t.Fatal(p)
	}
	if err = a.Recheck(ctx, p); err != nil {
		t.Fatal(err)
	}
	other := daemonAuth{auth.NewAuthenticator(st, auth.Options{}), st}
	if err = other.Recheck(ctx, p); !errors.Is(err, gateway.ErrRevoked) {
		t.Fatal("separate instance unexpectedly knows credential", err)
	}
	// A second store handle models the CLI process, bypassing any auth cache.
	cli, err := sqlite.Open(ctx, path, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, _, err = auth.RevokeByName(ctx, cli, store.KindDevice, user.ID, "desk", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err = a.Recheck(ctx, p); !errors.Is(err, gateway.ErrRevoked) {
		t.Fatal(err)
	}
	if _, err = a.Authenticate(ctx, req); !errors.Is(err, gateway.ErrRevoked) {
		t.Fatal(err)
	}
	_ = st.Close()
	if err = a.Recheck(ctx, p); err == nil || errors.Is(err, gateway.ErrRevoked) {
		t.Fatal("store error not propagated", err)
	}
}
