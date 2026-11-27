package auth_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/store"
)

// presence answers auth.EnrollRequest.Online from a set of online ids and
// records what it was asked.
type presence struct {
	online map[string]bool
	err    error
	asked  []string // kind/user/id
}

func (p *presence) check(_ context.Context, kind store.PrincipalKind, userID, id string) (bool, error) {
	p.asked = append(p.asked, string(kind)+"/"+userID+"/"+id)
	return p.online[id], p.err
}

// enroll enrolls kind with token, named name ("" for the default), from
// machine, with p telling who is online.
func (f *fixture) enrollWith(token string, kind store.PrincipalKind, name, machine string, p *presence) (auth.Enrollment, error) {
	return auth.Enroll(context.Background(), f.st, auth.EnrollRequest{
		Token: token, Kind: kind, Name: name, Hostname: "other-host.example", MachineID: machine,
		OS: "linux", Arch: "amd64", ClientVersion: "test", Online: p.check,
	}, t0.Add(time.Minute))
}

// rejoinToken issues a token that replaces the user's identity id of kind.
func (f *fixture) rejoinToken(t *testing.T, kind store.PrincipalKind, id string) string {
	t.Helper()
	tok, rec, err := auth.CreateRejoinToken(context.Background(), f.st, f.user.ID, kind, id, 0, t0)
	if err != nil {
		t.Fatal(err)
	}
	if rec.ReplaceID != id || len(rec.Kinds) != 1 || rec.Kinds[0] != kind {
		t.Fatalf("rejoin token record %+v", rec)
	}
	return tok
}

// revocationEffects is what an administrator's revoke changes that can be
// observed afterwards, for one identity enrolled by setupRevocationTarget.
type revocationEffects struct {
	Authenticate error // with the identity's long-term token
	Valid        bool
	DeletedAt    int64
	Integrator   bool
	ByName       error  // GetIdentityByName of its name
	Credentials  int    // unrevoked credentials (by RevokeCredentials, which counts them)
	Providers    string // the provider's workers, for a worker
	Location     bool   // DeviceRevoked of the project location, for a device
}

// setupRevocationTarget enrolls "laptop" of kind from machine m-1 and ties
// it into the state a revocation touches: a worker gets the integrator
// grant and a provider assignment, a device a project location.
func setupRevocationTarget(t *testing.T, f *fixture, kind store.PrincipalKind) auth.Enrollment {
	t.Helper()
	ctx := context.Background()
	en, err := f.enrollWith(f.token(t, kind, 0, t0), kind, "laptop", "m-1", &presence{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.NewAuthenticator(f.st, auth.Options{}).Authenticate(ctx, bearer(en.Token)); err != nil {
		t.Fatal(err)
	}
	if kind == store.KindWorker {
		if _, err := f.st.SetWorkerIntegrator(ctx, f.user.ID, en.Identity.ID, true); err != nil {
			t.Fatal(err)
		}
		if _, err := f.st.CreateProvider(ctx, store.Provider{
			ID: "p1", UserID: f.user.ID, Name: "P", Kind: store.ProviderKindClaudeCode, Scheme: store.ProviderSchemeXAPIKey,
			Secrets: []byte("sealed"), WorkerIDs: []string{en.Identity.ID}, CreatedAt: t0.UnixMilli(), UpdatedAt: t0.UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := f.st.CreateProject(ctx, store.Project{
			ID: "proj", UserID: f.user.ID, Name: "proj", Locations: []store.ProjectLocation{{DeviceID: en.Identity.ID, Path: "/src"}},
			CreatedAt: t0.UnixMilli(), UpdatedAt: t0.UnixMilli(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return en
}

func observeRevocation(t *testing.T, f *fixture, en auth.Enrollment) revocationEffects {
	t.Helper()
	ctx := context.Background()
	a := auth.NewAuthenticator(f.st, auth.Options{})
	var e revocationEffects
	_, e.Authenticate = a.Authenticate(ctx, bearer(en.Token))
	// Valid answers only for a credential this authenticator has seen.
	e.Valid, _ = a.Valid(ctx, en.CredentialID)
	id, err := f.st.GetIdentity(ctx, en.Identity.Kind, en.Identity.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.DeletedAt, e.Integrator = id.DeletedAt, id.Integrator
	if got, err := f.st.GetIdentityByName(ctx, en.Identity.Kind, f.user.ID, "laptop"); err != nil {
		e.ByName = err
	} else if got.ID == en.Identity.ID {
		e.ByName = errors.New("still resolves to the old identity")
	}
	if err := f.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		n, err := tx.RevokeCredentials(ctx, en.Identity.Kind, en.Identity.ID, t0.Add(time.Hour).UnixMilli())
		e.Credentials = n
		if err != nil {
			return err
		}
		return errors.New("roll back")
	}); err == nil || err.Error() != "roll back" {
		t.Fatal(err)
	}
	if en.Identity.Kind == store.KindWorker {
		p, err := f.st.GetProvider(ctx, f.user.ID, "p1", t0.UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		e.Providers = strings.Join(p.WorkerIDs, ",")
	} else {
		p, err := f.st.GetProject(ctx, f.user.ID, "proj")
		if err != nil {
			t.Fatal(err)
		}
		e.Location = p.Locations[0].DeviceRevoked
	}
	return e
}

// A rejoin token replaces its identity: in one transaction the identity
// ends exactly as an administrator's revoke ends it and the new one takes
// its name, whatever the machine reports (another machine id, another
// hostname) and with or without the name in another case.
func TestRejoinReplacesIdentity(t *testing.T) {
	for _, kind := range []store.PrincipalKind{store.KindDevice, store.KindWorker} {
		for _, name := range []string{"", "LapTop"} {
			t.Run(string(kind)+"/"+name, func(t *testing.T) {
				ctx := context.Background()

				// The baseline: an administrator revokes the identity.
				admin := newFixture(t)
				revoked := setupRevocationTarget(t, admin, kind)
				if _, _, err := auth.RevokeByName(ctx, admin.st, kind, admin.user.ID, "laptop", t0.Add(time.Minute)); err != nil {
					t.Fatal(err)
				}
				want := observeRevocation(t, admin, revoked)
				if !errors.Is(want.Authenticate, auth.ErrRevoked) || want.DeletedAt == 0 || !errors.Is(want.ByName, store.ErrNotFound) {
					t.Fatalf("admin revoke effects: %+v", want)
				}

				f := newFixture(t)
				old := setupRevocationTarget(t, f, kind)
				p := &presence{}
				en, err := f.enrollWith(f.rejoinToken(t, kind, old.Identity.ID), kind, name, "m-2", p)
				if err != nil {
					t.Fatal(err)
				}
				if en.Replaced.ID != old.Identity.ID || en.Replaced.Name != "laptop" || en.Replaced.DeletedAt != 0 || en.ReplacedCredentials != 1 {
					t.Fatalf("Replaced = %+v, %d credentials", en.Replaced, en.ReplacedCredentials)
				}
				if want := []string{string(kind) + "/" + f.user.ID + "/" + old.Identity.ID}; !slices.Equal(p.asked, want) {
					t.Fatalf("Online asked %q, want %q", p.asked, want)
				}
				got := observeRevocation(t, f, old)
				got.ByName = nil // the name resolves again, to the new identity
				want.ByName = nil
				if got != want {
					t.Fatalf("rejoin effects\n got %+v\nwant %+v (admin revoke)", got, want)
				}
				// The new identity holds the replaced one's name, as it was.
				byName, err := f.st.GetIdentityByName(ctx, kind, f.user.ID, "laptop")
				if err != nil || byName.ID != en.Identity.ID || byName.Name != "laptop" || byName.MachineID != "m-2" {
					t.Fatalf("name now resolves to %+v, %v", byName, err)
				}
				if _, err := auth.NewAuthenticator(f.st, auth.Options{}).Authenticate(ctx, bearer(en.Token)); err != nil {
					t.Fatalf("new credential: %v", err)
				}
				if n, err := f.st.CountLiveIdentities(ctx, kind, f.user.ID); n != 1 || err != nil {
					t.Fatalf("live identities = %d, %v", n, err)
				}
			})
		}
	}
}

// A rejoin is refused, without using its token or touching its identity,
// while the identity is online, when the request names another name, and
// when the online check fails or is missing.
func TestRejoinRefusals(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	old, err := f.enrollWith(f.token(t, store.KindWorker, 0, t0), store.KindWorker, "laptop", "m-1", &presence{})
	if err != nil {
		t.Fatal(err)
	}
	tok := f.rejoinToken(t, store.KindWorker, old.Identity.ID)
	try := func(name string, p *presence) error {
		_, err := f.enrollWith(tok, store.KindWorker, name, "m-1", p)
		return err
	}
	if err := try("", &presence{online: map[string]bool{old.Identity.ID: true}}); !errors.Is(err, auth.ErrReplaceOnline) ||
		!strings.Contains(err.Error(), `worker "laptop" (`+old.Identity.ID+") is connected now") {
		t.Fatalf("online: %v", err)
	}
	if err := try("desk", &presence{}); !errors.Is(err, auth.ErrInvalidRequest) ||
		!strings.Contains(err.Error(), `rejoins as worker "laptop"`) || !strings.Contains(err.Error(), "--name laptop") {
		t.Fatalf("another name: %v", err)
	}
	if err := try("", &presence{err: errors.New("registry down")}); err == nil || !strings.Contains(err.Error(), "registry down") {
		t.Fatalf("presence failure: %v", err)
	}
	if _, err := auth.Enroll(ctx, f.st, auth.EnrollRequest{Token: tok, Kind: store.KindWorker, MachineID: "m-1"}, t0); !errors.Is(err, auth.ErrReplaceOnline) {
		t.Fatalf("no presence check: %v", err)
	}
	// A device token for the worker role is a token failure as ever.
	if err := func() error {
		_, err := f.enrollWith(tok, store.KindDevice, "", "m-1", &presence{})
		return err
	}(); err != auth.ErrEnrollRejected { //nolint:errorlint
		t.Fatalf("wrong role: %v", err)
	}
	if _, err := auth.NewAuthenticator(f.st, auth.Options{}).Authenticate(ctx, bearer(old.Token)); err != nil {
		t.Fatalf("identity after refusals: %v", err)
	}
	// The token was never used: the rejoin still goes through.
	en, err := f.enrollWith(tok, store.KindWorker, "LAPTOP", "m-1", &presence{})
	if err != nil || en.Replaced.ID != old.Identity.ID || en.Identity.Name != "laptop" {
		t.Fatalf("rejoin after refusals: %+v, %v", en, err)
	}
}

// A rejoin whose identity was deleted since its token was issued is
// refused; a token can only be issued for a live identity of its user and
// kind.
func TestRejoinTargetGone(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	old := f.mustEnroll(t, store.KindDevice, "laptop")
	tok := f.rejoinToken(t, store.KindDevice, old.Identity.ID)
	if _, _, err := auth.RevokeByID(ctx, f.st, store.KindDevice, f.user.ID, old.Identity.ID, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enrollWith(tok, store.KindDevice, "", "m-1", &presence{}); !errors.Is(err, auth.ErrReplaceGone) {
		t.Fatalf("deleted target: %v", err)
	}
	// Another enrollment took the name meanwhile: still refused, not renamed.
	f.mustEnroll(t, store.KindDevice, "laptop")
	if _, err := f.enrollWith(tok, store.KindDevice, "", "m-1", &presence{}); !errors.Is(err, auth.ErrReplaceGone) {
		t.Fatalf("deleted target, name taken again: %v", err)
	}

	other := store.User{ID: "other", Email: "other@example.com", Role: store.RoleUser, CreatedAt: t0.UnixMilli()}
	if err := f.st.CreateUser(ctx, other); err != nil {
		t.Fatal(err)
	}
	live, err := f.st.GetIdentityByName(ctx, store.KindDevice, f.user.ID, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	for label, c := range map[string]struct {
		user, id string
		kind     store.PrincipalKind
	}{
		"another user's":  {other.ID, live.ID, store.KindDevice},
		"of another kind": {f.user.ID, live.ID, store.KindWorker},
		"deleted":         {f.user.ID, old.Identity.ID, store.KindDevice},
		"unknown":         {f.user.ID, "nope", store.KindDevice},
	} {
		if _, _, err := auth.CreateRejoinToken(ctx, f.st, c.user, c.kind, c.id, 0, t0); !errors.Is(err, auth.ErrNoSuchIdentity) {
			t.Errorf("%s identity: %v", label, err)
		}
	}
}

// A plain enrollment never replaces anything: a taken name is refused
// whatever the machine and whether its holder is online.
func TestEnrollTakenNameRefused(t *testing.T) {
	f := newFixture(t)
	old := f.mustEnroll(t, store.KindWorker, "laptop")
	for _, machine := range []string{"m-1", "m-2"} {
		_, err := f.enrollWith(f.token(t, store.KindWorker, 0, t0), store.KindWorker, "LAPTOP", machine, &presence{})
		if !errors.Is(err, auth.ErrNameTaken) || !strings.Contains(err.Error(), `a worker named "laptop" already exists`) {
			t.Fatalf("from %s: %v", machine, err)
		}
	}
	if _, err := auth.NewAuthenticator(f.st, auth.Options{}).Authenticate(context.Background(), bearer(old.Token)); err != nil {
		t.Fatalf("holder: %v", err)
	}
}

// A rejoin frees the slot of the identity it replaces, so it succeeds at
// the live identity limit.
func TestRejoinAtIdentityLimit(t *testing.T) {
	f := newFixture(t)
	var first auth.Enrollment
	for i := range auth.MaxLiveWorkers {
		en, err := f.enrollWith(f.token(t, store.KindWorker, 0, t0), store.KindWorker, "w"+string(rune('a'+i)), "m-1", &presence{})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = en
		}
	}
	if _, err := f.enrollWith(f.token(t, store.KindWorker, 0, t0), store.KindWorker, "new", "m-1", &presence{}); !errors.Is(err, auth.ErrIdentityLimit) {
		t.Fatalf("past the limit: %v", err)
	}
	en, err := f.enrollWith(f.rejoinToken(t, store.KindWorker, first.Identity.ID), store.KindWorker, "", "m-1", &presence{})
	if err != nil || en.Replaced.Name != "wa" {
		t.Fatalf("rejoin at the limit: %+v, %v", en.Replaced, err)
	}
}
