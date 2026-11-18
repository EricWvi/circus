package storetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/EricWvi/circus/internal/store"
)

// projectFixture is the legacy machine owner with two devices and two Web
// users who bind those devices in their projects.
type projectFixture struct {
	fixture
	other         store.Identity // a second device of the owner
	alice, bob    store.User     // Web users
	foreignDevice store.Identity // a device of another owner
}

func seedProjects(t testing.TB, s store.Store) projectFixture {
	t.Helper()
	ctx := context.Background()
	f := projectFixture{fixture: seed(t, s)}
	f.other = store.Identity{Kind: store.KindDevice, ID: "d2", UserID: f.user.ID, Name: "desktop", MachineID: "m2", OS: "windows", CreatedAt: t0}
	must(t, s.CreateIdentity(ctx, f.other))
	f.alice = store.User{ID: "alice", Email: "alice@example.com", Role: store.RoleUser, CreatedAt: t0}
	f.bob = store.User{ID: "bob", Email: "bob@example.com", Role: store.RoleUser, CreatedAt: t0}
	must(t, s.CreateUser(ctx, f.alice))
	must(t, s.CreateUser(ctx, f.bob))
	must(t, s.CreateUser(ctx, store.User{ID: "u2", Name: "elsewhere", CreatedAt: t0}))
	f.foreignDevice = store.Identity{Kind: store.KindDevice, ID: "d9", UserID: "u2", Name: "laptop", MachineID: "m9", CreatedAt: t0}
	must(t, s.CreateIdentity(ctx, f.foreignDevice))
	return f
}

func loc(device, path string) store.ProjectLocation {
	return store.ProjectLocation{DeviceID: device, Path: path}
}

func newProject(id, user, name string, locs ...store.ProjectLocation) store.Project {
	return store.Project{ID: id, UserID: user, Name: name, Locations: locs, CreatedAt: t0, UpdatedAt: t0}
}

// withNames fills the device names reads return.
func withNames(p store.Project, names map[string]string) store.Project {
	locs := make([]store.ProjectLocation, len(p.Locations))
	for i, l := range p.Locations {
		l.DeviceName = names[l.DeviceID]
		locs[i] = l
	}
	p.Locations = locs
	return p
}

func projectNames(ps []store.Project) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func testProjects(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	names := map[string]string{"d1": "laptop", "d2": "desktop"}
	owner := f.user.ID

	p := newProject("p1", f.alice.ID, "Circus", loc("d1", "/src/circus"), loc("d2", `C:\src`))
	got, err := s.CreateProject(ctx, p, owner)
	must(t, err)
	mustEqual(t, "created", got, withNames(p, names))
	got, err = s.GetProject(ctx, f.alice.ID, "p1")
	must(t, err)
	mustEqual(t, "get", got, withNames(p, names))

	// Names are unique per user ignoring case, within one user only.
	_, err = s.CreateProject(ctx, newProject("p2", f.alice.ID, "cIRCUS", loc("d1", "/x")), owner)
	mustIs(t, err, store.ErrExists)
	_, err = s.CreateProject(ctx, newProject("p2", f.bob.ID, "circus", loc("d1", "/x")), owner)
	must(t, err)
	// The id is taken.
	_, err = s.CreateProject(ctx, newProject("p1", f.alice.ID, "other", loc("d1", "/x")), owner)
	mustIs(t, err, store.ErrExists)

	// Listing: by name ignoring case, then id; only the user's own.
	for i, name := range []string{"beta", "Alpha", "gamma"} {
		_, err := s.CreateProject(ctx, newProject(fmt.Sprintf("q%d", i), f.alice.ID, name, loc("d1", "/q")), owner)
		must(t, err)
	}
	list, err := s.ListProjects(ctx, f.alice.ID)
	must(t, err)
	mustEqual(t, "names", projectNames(list), []string{"Alpha", "beta", "Circus", "gamma"})
	mustEqual(t, "listed project", list[2], withNames(p, names))
	list, err = s.ListProjects(ctx, f.bob.ID)
	must(t, err)
	mustEqual(t, "bob's names", projectNames(list), []string{"circus"})
	list, err = s.ListProjects(ctx, "nobody")
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("unknown user's projects = %v", list)
	}

	// Ownership: another user's project is not found, for every operation.
	_, err = s.GetProject(ctx, f.bob.ID, "p1")
	mustIs(t, err, store.ErrNotFound)
	_, err = s.UpdateProject(ctx, f.bob.ID, "p1", store.ProjectPatch{Name: ptr("mine"), UpdatedAt: t0 + 1}, owner)
	mustIs(t, err, store.ErrNotFound)
	mustIs(t, s.DeleteProject(ctx, f.bob.ID, "p1", t0), store.ErrNotFound)
	_, err = s.GetProject(ctx, f.alice.ID, "missing")
	mustIs(t, err, store.ErrNotFound)

	// Rename: to its own name in another case is no conflict; to another
	// project's name is.
	got, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Name: ptr("CIRCUS"), UpdatedAt: t0 + 5}, owner)
	must(t, err)
	if got.Name != "CIRCUS" || got.UpdatedAt != t0+5 || got.CreatedAt != t0 {
		t.Fatalf("renamed = %+v", got)
	}
	_, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Name: ptr("ALPHA"), UpdatedAt: t0 + 6}, owner)
	mustIs(t, err, store.ErrExists)
	// An empty patch changes nothing, not even the update time.
	got, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{UpdatedAt: t0 + 7}, owner)
	must(t, err)
	if got.UpdatedAt != t0+5 {
		t.Fatalf("empty patch touched the project: %+v", got)
	}

	// Explicit last used location: in range only.
	got, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{LastUsedLocation: ptr(1), UpdatedAt: t0 + 8}, owner)
	must(t, err)
	if got.LastUsedLocation != 1 {
		t.Fatalf("last used = %d", got.LastUsedLocation)
	}
	for _, i := range []int{2, -1} {
		_, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{LastUsedLocation: ptr(i), UpdatedAt: t0 + 9}, owner)
		mustIs(t, err, store.ErrLocationIndex)
	}

	// Delete, then it is gone; deleting again is not found.
	must(t, s.DeleteProject(ctx, f.alice.ID, "q0", t0))
	_, err = s.GetProject(ctx, f.alice.ID, "q0")
	mustIs(t, err, store.ErrNotFound)
	mustIs(t, s.DeleteProject(ctx, f.alice.ID, "q0", t0), store.ErrNotFound)
}

func testProjectLocations(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	owner := f.user.ID
	a, bb, c := loc("d1", "/a"), loc("d1", "/b"), loc("d1", "/c")
	p := newProject("p1", f.alice.ID, "multi", a, bb, c)
	p.LastUsedLocation = 2
	_, err := s.CreateProject(ctx, p, owner)
	must(t, err)
	replace := func(locs ...store.ProjectLocation) store.Project {
		t.Helper()
		got, err := s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Locations: locs, UpdatedAt: t0 + 1}, owner)
		must(t, err)
		return got
	}
	// The default follows its location to its new index.
	if got := replace(c, a); got.LastUsedLocation != 0 {
		t.Fatalf("followed = %d", got.LastUsedLocation)
	}
	// The same path on another device is another location: the default is
	// gone and the index is clamped.
	got := replace(bb, loc("d2", "/c"), a)
	if got.LastUsedLocation != 0 {
		t.Fatalf("kept index = %d", got.LastUsedLocation)
	}
	mustEqual(t, "replaced locations", got.Locations, []store.ProjectLocation{
		{DeviceID: "d1", DeviceName: "laptop", Path: "/b"}, {DeviceID: "d2", DeviceName: "desktop", Path: "/c"}, {DeviceID: "d1", DeviceName: "laptop", Path: "/a"}})
	_, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{LastUsedLocation: ptr(2), UpdatedAt: t0 + 2}, owner)
	must(t, err)
	if got := replace(loc("d1", "/x"), loc("d1", "/y")); got.LastUsedLocation != 1 {
		t.Fatalf("clamped = %d", got.LastUsedLocation)
	}

	// An explicit index wins over following, checked against the new list.
	got, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Locations: []store.ProjectLocation{a, bb, c}, LastUsedLocation: ptr(0), UpdatedAt: t0 + 3}, owner)
	must(t, err)
	if got.LastUsedLocation != 0 || len(got.Locations) != 3 {
		t.Fatalf("explicit = %+v", got)
	}
	_, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Locations: []store.ProjectLocation{a}, LastUsedLocation: ptr(1), UpdatedAt: t0 + 4}, owner)
	mustIs(t, err, store.ErrLocationIndex)

	// Devices must be live devices of the owner; the error names the index.
	// A refused write changes nothing.
	before, err := s.GetProject(ctx, f.alice.ID, "p1")
	must(t, err)
	for _, bad := range []string{"d9", "missing"} {
		_, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Locations: []store.ProjectLocation{a, loc(bad, "/z")}, Name: ptr("renamed"), UpdatedAt: t0 + 5}, owner)
		var de *store.LocationDeviceError
		if !errors.As(err, &de) || de.Index != 1 || !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("device %s: error = %v", bad, err)
		}
	}
	after, err := s.GetProject(ctx, f.alice.ID, "p1")
	must(t, err)
	mustEqual(t, "after refused update", after, before)
	_, err = s.CreateProject(ctx, newProject("p2", f.alice.ID, "foreign", loc("d9", "/z")), owner)
	var de *store.LocationDeviceError
	if !errors.As(err, &de) || de.Index != 0 {
		t.Fatalf("foreign device: %v", err)
	}
	_, err = s.GetProject(ctx, f.alice.ID, "p2")
	mustIs(t, err, store.ErrNotFound)

	// A deleted device can no longer be bound, but existing locations keep
	// naming it, also after another device takes its name.
	_, err = s.CreateProject(ctx, newProject("p3", f.alice.ID, "old", loc("d2", "/k")), owner)
	must(t, err)
	_, err = s.DeleteIdentity(ctx, store.KindDevice, "d2", t0+10)
	must(t, err)
	must(t, s.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "d3", UserID: owner, Name: "desktop", MachineID: "m3", CreatedAt: t0}))
	got, err = s.GetProject(ctx, f.alice.ID, "p3")
	must(t, err)
	mustEqual(t, "location of a deleted device", got.Locations, []store.ProjectLocation{{DeviceID: "d2", DeviceName: "desktop", DeviceRevoked: true, Path: "/k"}})
	_, err = s.UpdateProject(ctx, f.alice.ID, "p3", store.ProjectPatch{Locations: []store.ProjectLocation{loc("d2", "/k")}, UpdatedAt: t0 + 11}, owner)
	mustIs(t, err, store.ErrNotFound)
	// Renaming keeps the locations without checking their devices again.
	_, err = s.UpdateProject(ctx, f.alice.ID, "p3", store.ProjectPatch{Name: ptr("older"), UpdatedAt: t0 + 12}, owner)
	must(t, err)
}

func testProjectValidation(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	owner := f.user.ID
	ok := newProject("p1", f.alice.ID, "ok", loc("d1", "/a"))
	for _, tc := range []struct {
		name string
		edit func(*store.Project)
		want error
	}{
		{"no id", func(p *store.Project) { p.ID = "" }, store.ErrInvalid},
		{"no user", func(p *store.Project) { p.UserID = "" }, store.ErrInvalid},
		{"empty name", func(p *store.Project) { p.Name = "" }, store.ErrInvalid},
		{"long name", func(p *store.Project) { p.Name = strings.Repeat("é", 65) }, store.ErrInvalid},
		{"invalid UTF-8", func(p *store.Project) { p.Name = "a\xffb" }, store.ErrInvalid},
		{"no time", func(p *store.Project) { p.CreatedAt = 0 }, store.ErrInvalid},
		{"too many locations", func(p *store.Project) {
			p.Locations = nil
			for i := range store.MaxProjectLocations + 1 {
				p.Locations = append(p.Locations, loc("d1", fmt.Sprintf("/%d", i)))
			}
		}, store.ErrInvalid},
		{"duplicate location", func(p *store.Project) { p.Locations = append(p.Locations, loc("d1", "/a")) }, store.ErrInvalid},
		{"two defaults on one device", func(p *store.Project) { p.Locations = []store.ProjectLocation{loc("d1", ""), loc("d1", "")} }, store.ErrInvalid},
		{"long repository URL", func(p *store.Project) { p.RepoURL = "https://h/" + strings.Repeat("x", store.MaxRepoURLBytes) }, store.ErrInvalid},
		{"invalid UTF-8 repository URL", func(p *store.Project) { p.RepoURL = "https://h/\xff" }, store.ErrInvalid},
		{"last used without locations", func(p *store.Project) { p.Locations = nil; p.LastUsedLocation = 1 }, store.ErrLocationIndex},
		{"long path", func(p *store.Project) { p.Locations[0].Path = "/" + strings.Repeat("x", store.MaxPathBytes) }, store.ErrInvalid},
		{"no device", func(p *store.Project) { p.Locations[0].DeviceID = "" }, store.ErrInvalid},
		{"last used out of range", func(p *store.Project) { p.LastUsedLocation = 1 }, store.ErrLocationIndex},
		{"unknown user", func(p *store.Project) { p.UserID = "ghost" }, store.ErrNotFound},
	} {
		p := ok
		p.Locations = append([]store.ProjectLocation(nil), ok.Locations...)
		tc.edit(&p)
		_, err := s.CreateProject(ctx, p, owner)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: error = %v, want %v", tc.name, err, tc.want)
		}
	}
	// 64 code points of multi-byte text and a 4096-byte path are fine.
	p := newProject("p1", f.alice.ID, strings.Repeat("語", 64), loc("d1", "/"+strings.Repeat("x", store.MaxPathBytes-1)))
	_, err := s.CreateProject(ctx, p, owner)
	must(t, err)
	_, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Name: ptr(""), UpdatedAt: t0 + 1}, owner)
	mustIs(t, err, store.ErrInvalid)
	_, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Name: ptr("x")}, owner)
	mustIs(t, err, store.ErrInvalid)
	_, err = s.GetProject(ctx, f.alice.ID, "")
	mustIs(t, err, store.ErrInvalid)
}

func testProjectCascade(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	_, err := s.CreateProject(ctx, newProject("p1", f.alice.ID, "a", loc("d1", "/a"), loc("d2", "/b")), f.user.ID)
	must(t, err)
	_, err = s.CreateProject(ctx, newProject("p2", f.bob.ID, "a", loc("d1", "/a")), f.user.ID)
	must(t, err)
	// Deleting a project deletes its locations: the same id can be created
	// again with the same locations.
	must(t, s.DeleteProject(ctx, f.alice.ID, "p1", t0))
	_, err = s.CreateProject(ctx, newProject("p1", f.alice.ID, "a", loc("d1", "/a"), loc("d2", "/b")), f.user.ID)
	must(t, err)
	// Deleting a Web user deletes its projects, not other users' projects.
	must(t, s.DeleteUser(ctx, f.alice.ID))
	_, err = s.GetProject(ctx, f.alice.ID, "p1")
	mustIs(t, err, store.ErrNotFound)
	_, err = s.GetProject(ctx, f.bob.ID, "p2")
	must(t, err)

	// A transaction that fails after a project write leaves nothing.
	boom := errors.New("boom")
	err = s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		if _, err := tx.CreateProject(ctx, newProject("p3", f.bob.ID, "b", loc("d1", "/b")), f.user.ID); err != nil {
			return err
		}
		return boom
	})
	mustIs(t, err, boom)
	_, err = s.GetProject(ctx, f.bob.ID, "p3")
	mustIs(t, err, store.ErrNotFound)
	// A failed project write inside a committed transaction leaves nothing
	// of itself.
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		_, err := tx.CreateProject(ctx, newProject("p4", f.bob.ID, "c", loc("d1", "/c"), loc("d9", "/c")), f.user.ID)
		mustIs(t, err, store.ErrNotFound)
		return nil
	}))
	_, err = s.GetProject(ctx, f.bob.ID, "p4")
	mustIs(t, err, store.ErrNotFound)
}

func testProjectLimit(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := range store.MaxProjectsPerUser {
			if _, err := tx.CreateProject(ctx, newProject(fmt.Sprintf("p%d", i), f.alice.ID, fmt.Sprintf("n%d", i), loc("d1", "/a")), f.user.ID); err != nil {
				return err
			}
		}
		return nil
	}))
	_, err := s.CreateProject(ctx, newProject("over", f.alice.ID, "over", loc("d1", "/a")), f.user.ID)
	mustIs(t, err, store.ErrLimit)
	_, err = s.CreateProject(ctx, newProject("bobs", f.bob.ID, "over", loc("d1", "/a")), f.user.ID)
	must(t, err)
}

// testProjectConcurrentCreate races creates of names equal ignoring case,
// on one handle and on two handles: exactly one wins.
func testProjectConcurrentCreate(t *testing.T, b Backend) {
	s, dir := open(t, b)
	ctx := deadline(t)
	f := seedProjects(t, s)
	handles := []store.Store{s, reopen(t, b, dir)}
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Go(func() {
			name := "Shared"
			if i%2 == 1 {
				name = "sHARED"
			}
			_, errs[i] = handles[i%2].CreateProject(ctx, newProject(fmt.Sprintf("p%d", i), f.alice.ID, name, loc("d1", "/a")), f.user.ID)
		})
	}
	wg.Wait()
	wins := 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrExists), errors.Is(err, store.ErrBusy):
		default:
			t.Fatalf("create %d: %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d creates won: %v", wins, errs)
	}
	list, err := s.ListProjects(ctx, f.alice.ID)
	must(t, err)
	if len(list) != 1 {
		t.Fatalf("projects = %v", list)
	}
}

// testProjectNameKey checks that keys are equal exactly when the names are
// equal under simple case folding.
func testProjectNameKey(t *testing.T, _ Backend) {
	names := []string{"circus", "CIRCUS", "Circus", "circuſ", "kelvin", "\u212aelvin", "straße", "STRASSE", "ΑΣ", "ας", "ασ",
		"İstanbul", "istanbul", "Istanbul", "ǅ", "ǆ", "Ǆ", "日本", "", "a b", "A B"}
	for _, a := range names {
		for _, b := range names {
			if got, want := store.ProjectNameKey(a) == store.ProjectNameKey(b), strings.EqualFold(a, b); got != want {
				t.Errorf("key(%q) == key(%q) is %v, EqualFold %v", a, b, got, want)
			}
		}
	}
	mustEqual(t, "key", store.ProjectNameKey("CirCus K"), "circus k")
}

// testProjectOptionalParts covers what a project may leave out: locations,
// paths (a device's default directory) and the repository URL.
func testProjectOptionalParts(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	owner := f.user.ID

	// A project with only a name, and one with only a repository URL.
	bare, err := s.CreateProject(ctx, newProject("p1", f.alice.ID, "bare"), owner)
	must(t, err)
	if len(bare.Locations) != 0 || bare.LastUsedLocation != 0 || bare.RepoURL != "" {
		t.Fatalf("bare = %+v", bare)
	}
	repo := newProject("p2", f.alice.ID, "repo")
	repo.RepoURL = "git@github.com:owner/repo.git"
	got, err := s.CreateProject(ctx, repo, owner)
	must(t, err)
	mustEqual(t, "repo-only project", got, repo)
	list, err := s.ListProjects(ctx, f.alice.ID)
	must(t, err)
	mustEqual(t, "listed", list, []store.Project{bare, repo})

	// Default locations: one per device, next to explicit paths.
	p := newProject("p3", f.alice.ID, "defaults", loc("d1", ""), loc("d1", "/src"), loc("d2", ""))
	p.LastUsedLocation = 2
	got, err = s.CreateProject(ctx, p, owner)
	must(t, err)
	mustEqual(t, "defaults", got, withNames(p, map[string]string{"d1": "laptop", "d2": "desktop"}))
	_, err = s.UpdateProject(ctx, f.alice.ID, "p3", store.ProjectPatch{Locations: []store.ProjectLocation{loc("d2", ""), loc("d2", "")}, UpdatedAt: t0 + 1}, owner)
	mustIs(t, err, store.ErrInvalid)
	// The default location is followed like any other.
	got, err = s.UpdateProject(ctx, f.alice.ID, "p3", store.ProjectPatch{Locations: []store.ProjectLocation{loc("d1", "/src"), loc("d2", "")}, UpdatedAt: t0 + 1}, owner)
	must(t, err)
	if got.LastUsedLocation != 1 {
		t.Fatalf("followed default = %d", got.LastUsedLocation)
	}

	// Removing every location resets the index to 0; with no locations
	// only 0 is accepted.
	got, err = s.UpdateProject(ctx, f.alice.ID, "p3", store.ProjectPatch{Locations: []store.ProjectLocation{}, UpdatedAt: t0 + 2}, owner)
	must(t, err)
	if len(got.Locations) != 0 || got.LastUsedLocation != 0 {
		t.Fatalf("emptied = %+v", got)
	}
	_, err = s.UpdateProject(ctx, f.alice.ID, "p3", store.ProjectPatch{LastUsedLocation: ptr(1), UpdatedAt: t0 + 3}, owner)
	mustIs(t, err, store.ErrLocationIndex)
	_, err = s.UpdateProject(ctx, f.alice.ID, "p3", store.ProjectPatch{LastUsedLocation: ptr(0), UpdatedAt: t0 + 3}, owner)
	must(t, err)

	// Repository URL: set, kept by other updates, cleared with "".
	got, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{RepoURL: ptr("https://example.com/a.git"), UpdatedAt: t0 + 4}, owner)
	must(t, err)
	if got.RepoURL != "https://example.com/a.git" || got.UpdatedAt != t0+4 {
		t.Fatalf("set URL = %+v", got)
	}
	got, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Name: ptr("renamed"), UpdatedAt: t0 + 5}, owner)
	must(t, err)
	if got.RepoURL != "https://example.com/a.git" {
		t.Fatalf("rename dropped the URL: %+v", got)
	}
	got, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{RepoURL: ptr(""), UpdatedAt: t0 + 6}, owner)
	must(t, err)
	if got.RepoURL != "" {
		t.Fatalf("cleared URL = %+v", got)
	}
	got, err = s.GetProject(ctx, f.alice.ID, "p1")
	must(t, err)
	if got.RepoURL != "" || got.Name != "renamed" {
		t.Fatalf("read back = %+v", got)
	}
}

// testProjectWithoutOwner: a write that binds no device needs no device
// owner (a user without devices).
func testProjectWithoutOwner(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	_, err := s.CreateProject(ctx, newProject("p1", f.bob.ID, "mine"), "")
	must(t, err)
	_, err = s.UpdateProject(ctx, f.bob.ID, "p1", store.ProjectPatch{Locations: []store.ProjectLocation{}, Name: ptr("x"), UpdatedAt: t0 + 1}, "")
	must(t, err)
	_, err = s.CreateProject(ctx, newProject("p2", f.bob.ID, "bound", loc("d1", "/a")), "")
	mustIs(t, err, store.ErrInvalid)
	_, err = s.UpdateProject(ctx, f.bob.ID, "p1", store.ProjectPatch{Locations: []store.ProjectLocation{loc("d1", "")}, UpdatedAt: t0 + 2}, "")
	mustIs(t, err, store.ErrInvalid)
}

// testProjectStale: ExpectedUpdatedAt guards against lost updates, and
// UpdatedAt strictly increases even when the caller's clock does not.
func testProjectStale(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	owner := f.user.ID
	p, err := s.CreateProject(ctx, newProject("p1", f.alice.ID, "shared", loc("d1", "/a")), owner)
	must(t, err)
	read := p.UpdatedAt

	// Two tabs read the same version. The first save wins; the second,
	// made in the same millisecond, is refused and changes nothing.
	first, err := s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Name: ptr("tab one"), ExpectedUpdatedAt: ptr(read), UpdatedAt: read}, owner)
	must(t, err)
	if first.UpdatedAt != read+1 {
		t.Fatalf("same-millisecond update: updated at %d, want %d", first.UpdatedAt, read+1)
	}
	_, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Locations: []store.ProjectLocation{loc("d1", "/b")}, ExpectedUpdatedAt: ptr(read), UpdatedAt: read}, owner)
	mustIs(t, err, store.ErrProjectStale)
	// A field set to its current value is still an edit of a stale form.
	_, err = s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Name: ptr("tab one"), ExpectedUpdatedAt: ptr(read), UpdatedAt: read}, owner)
	mustIs(t, err, store.ErrProjectStale)
	got, err := s.GetProject(ctx, f.alice.ID, "p1")
	must(t, err)
	mustEqual(t, "after stale update", got, first)

	// With the current version it applies; a clock behind the stored time
	// still moves UpdatedAt forward.
	second, err := s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{RepoURL: ptr("git@h:r"), ExpectedUpdatedAt: ptr(first.UpdatedAt), UpdatedAt: t0 - 1000}, owner)
	must(t, err)
	if second.UpdatedAt != first.UpdatedAt+1 {
		t.Fatalf("clock behind: updated at %d, want %d", second.UpdatedAt, first.UpdatedAt+1)
	}
	// A clock ahead is taken as is; without ExpectedUpdatedAt nothing is
	// compared.
	third, err := s.UpdateProject(ctx, f.alice.ID, "p1", store.ProjectPatch{Name: ptr("third"), UpdatedAt: t0 + 50_000}, owner)
	must(t, err)
	if third.UpdatedAt != t0+50_000 {
		t.Fatalf("clock ahead: updated at %d", third.UpdatedAt)
	}
	// Another user's project is not found, not stale.
	_, err = s.UpdateProject(ctx, f.bob.ID, "p1", store.ProjectPatch{ExpectedUpdatedAt: ptr(int64(1)), UpdatedAt: t0}, owner)
	mustIs(t, err, store.ErrNotFound)
}

// testProjectBytes: the names, repository URLs and paths of one user's
// projects are bounded together; other users have their own budget.
func testProjectBytes(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	owner := f.user.ID
	path := func(i, j int) string {
		p := fmt.Sprintf("/%03d-%02d-", i, j)
		return p + strings.Repeat("x", store.MaxPathBytes-len(p))
	}
	// 64 projects × 16 paths × 4096 bytes is exactly the budget, less the
	// names; make room for the names by shortening one path.
	names := 0
	must(t, s.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		for i := range 64 {
			p := newProject(fmt.Sprintf("p%d", i), f.alice.ID, fmt.Sprintf("n%d", i))
			names += len(p.Name)
			for j := range store.MaxProjectLocations {
				p.Locations = append(p.Locations, loc("d1", path(i, j)))
			}
			if i == 63 {
				p.Locations[15].Path = p.Locations[15].Path[:store.MaxPathBytes-names]
			}
			if _, err := tx.CreateProject(ctx, p, owner); err != nil {
				return err
			}
		}
		return nil
	}))
	// At the bound: one more byte anywhere is refused, as ErrLimit too.
	_, err := s.CreateProject(ctx, newProject("over", f.alice.ID, "x"), owner)
	mustIs(t, err, store.ErrProjectBytes)
	mustIs(t, err, store.ErrLimit)
	_, err = s.UpdateProject(ctx, f.alice.ID, "p0", store.ProjectPatch{RepoURL: ptr("g"), UpdatedAt: t0 + 1}, owner)
	mustIs(t, err, store.ErrProjectBytes)
	_, err = s.UpdateProject(ctx, f.alice.ID, "p0", store.ProjectPatch{Name: ptr("n0x"), UpdatedAt: t0 + 1}, owner)
	mustIs(t, err, store.ErrProjectBytes)
	got, err := s.GetProject(ctx, f.alice.ID, "p0")
	must(t, err)
	if got.Name != "n0" || got.RepoURL != "" {
		t.Fatalf("refused update changed the project: %+v", got.Name)
	}
	// Writes that do not grow the total still work: a rename of equal
	// length, a shorter path, and then the freed byte can be used.
	_, err = s.UpdateProject(ctx, f.alice.ID, "p0", store.ProjectPatch{Name: ptr("m0"), UpdatedAt: t0 + 2}, owner)
	must(t, err)
	locs := append([]store.ProjectLocation(nil), got.Locations...)
	for i := range locs {
		locs[i].DeviceName = ""
	}
	locs[0].Path = locs[0].Path[:len(locs[0].Path)-1]
	_, err = s.UpdateProject(ctx, f.alice.ID, "p0", store.ProjectPatch{Locations: locs, UpdatedAt: t0 + 3}, owner)
	must(t, err)
	_, err = s.CreateProject(ctx, newProject("one", f.alice.ID, "1"), owner)
	must(t, err)
	_, err = s.CreateProject(ctx, newProject("two", f.alice.ID, "2"), owner)
	mustIs(t, err, store.ErrProjectBytes)
	// Bob is not affected; deleting frees Alice's budget.
	_, err = s.CreateProject(ctx, newProject("bob", f.bob.ID, strings.Repeat("b", 64), loc("d1", path(0, 0))), owner)
	must(t, err)
	must(t, s.DeleteProject(ctx, f.alice.ID, "p1", t0))
	_, err = s.CreateProject(ctx, newProject("two", f.alice.ID, "2"), owner)
	must(t, err)
}

// testProjectInvisibleUpdates: updates that change nothing visible leave
// UpdatedAt alone, and LastUsedLocation alone is never stale, so moving it
// (as session start will) cannot break an open settings form.
func testProjectInvisibleUpdates(t *testing.T, b Backend) {
	s, _ := open(t, b)
	ctx := t.Context()
	f := seedProjects(t, s)
	owner := f.user.ID
	p := newProject("p1", f.alice.ID, "same", loc("d1", "/a"), loc("d1", ""))
	p.RepoURL = "git@h:r"
	created, err := s.CreateProject(ctx, p, owner)
	must(t, err)
	read := created.UpdatedAt
	update := func(patch store.ProjectPatch) store.Project {
		t.Helper()
		patch.UpdatedAt = t0 + 1000
		got, err := s.UpdateProject(ctx, f.alice.ID, "p1", patch, owner)
		must(t, err)
		return got
	}

	// Every field set to its current value: no change.
	got := update(store.ProjectPatch{Name: ptr("same"), RepoURL: ptr("git@h:r"),
		Locations: []store.ProjectLocation{loc("d1", "/a"), loc("d1", "")}, ExpectedUpdatedAt: ptr(read)})
	mustEqual(t, "same-value update", got, created)
	// A different case is a visible change.
	got = update(store.ProjectPatch{Name: ptr("SAME"), ExpectedUpdatedAt: ptr(read)})
	if got.UpdatedAt != t0+1000 {
		t.Fatalf("rename: updated at %d", got.UpdatedAt)
	}
	read = got.UpdatedAt

	// LastUsedLocation alone: stored, UpdatedAt kept, a stale version
	// ignored.
	got = update(store.ProjectPatch{LastUsedLocation: ptr(1), ExpectedUpdatedAt: ptr(int64(1))})
	if got.LastUsedLocation != 1 || got.UpdatedAt != read {
		t.Fatalf("last used only = %+v", got)
	}
	// An empty patch is never stale either.
	got = update(store.ProjectPatch{ExpectedUpdatedAt: ptr(int64(1))})
	if got.UpdatedAt != read {
		t.Fatalf("empty patch moved updated at: %+v", got)
	}
	// The open form saved after a session moved the default still applies.
	got = update(store.ProjectPatch{RepoURL: ptr(""), ExpectedUpdatedAt: ptr(read)})
	if got.RepoURL != "" || got.LastUsedLocation != 1 || got.UpdatedAt != read+1 {
		t.Fatalf("form after session start = %+v", got)
	}
	// Locations reordered are a change; the default follows.
	got = update(store.ProjectPatch{Locations: []store.ProjectLocation{loc("d1", ""), loc("d1", "/a")}, ExpectedUpdatedAt: ptr(got.UpdatedAt)})
	if got.LastUsedLocation != 0 || got.UpdatedAt != read+2 {
		t.Fatalf("reordered = %+v", got)
	}
}
