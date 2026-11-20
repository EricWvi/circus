package webapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/controlplane/webapi"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/coder/websocket"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const password = "password-123"

type harness struct {
	st     *sqlite.DB
	server *httptest.Server
	owner  store.User // the super user
	clock  atomic.Int64
	bus    *sessions.Bus
	waker  *recordingWaker
	jobs   *recordingJobs

	mu     sync.Mutex
	online map[string]bool // device id → link connected
}

// client is one browser: a cookie jar and the services.
type client struct {
	http     *http.Client
	auth     circusv1connect.AuthServiceClient
	projects circusv1connect.ProjectServiceClient
	machines circusv1connect.MachineServiceClient
	sessions circusv1connect.SessionServiceClient
}

func newHarness(t *testing.T, options ...func(*webapi.Config)) (*harness, *client) {
	t.Helper()
	h := &harness{online: map[string]bool{}, bus: sessions.NewBus(sessions.BusOptions{}), waker: &recordingWaker{}, jobs: &recordingJobs{}}
	h.clock.Store(time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC).UnixMilli())
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "web.db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h.st = st
	t.Cleanup(func() { st.Close() })
	now := func() time.Time { return time.UnixMilli(h.clock.Load()) }
	mux := http.NewServeMux()
	// As the server's top-level handler does: a request starts without the
	// write deadline the previous one on the connection left.
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(h.server.Close)
	// Watch streams end before the server waits for its handlers.
	t.Cleanup(h.bus.Close)
	var logs bytes.Buffer
	web, err := auth.NewWebService(t.Context(), st, auth.WebConfig{PublicURL: h.server.URL, DevInsecure: true, Now: now,
		BcryptCost: bcrypt.MinCost, Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	web.Register(mux)
	cfg := webapi.Config{Store: st, Interceptor: web.Interceptor(), Now: now, Online: h.onlineDevices, Bus: h.bus, Waker: h.waker, Jobs: h.jobs, PublicURL: h.server.URL}
	for _, option := range options {
		option(&cfg)
	}
	svc, err := webapi.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	svc.Register(mux)

	// Set up the super user with the logged setup token.
	var token string
	for _, line := range strings.Split(logs.String(), "\n") {
		var rec struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.URL != "" {
			u, _ := url.Parse(rec.URL)
			token = u.Query().Get("token")
		}
	}
	super := h.browser(t)
	if _, err := super.auth.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: token, Email: "admin@example.com", Password: password})); err != nil {
		t.Fatal(err)
	}
	h.owner, err = h.st.GetSuperUser(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return h, super
}

func (h *harness) onlineDevices(_ context.Context, userID string) (map[string]bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]bool{}
	if userID != h.owner.ID {
		return out, nil
	}
	for id, on := range h.online {
		out[id] = on
	}
	return out, nil
}

func (h *harness) setOnline(id string, on bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.online[id] = on
}

func (h *harness) browser(t *testing.T) *client {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: http.DefaultTransport.(*http.Transport).Clone()}
	t.Cleanup(c.CloseIdleConnections)
	return &client{
		http:     c,
		auth:     circusv1connect.NewAuthServiceClient(c, h.server.URL),
		projects: circusv1connect.NewProjectServiceClient(c, h.server.URL),
		machines: circusv1connect.NewMachineServiceClient(c, h.server.URL),
		sessions: circusv1connect.NewSessionServiceClient(c, h.server.URL),
	}
}

// signIn creates a normal Web user and signs a new browser in as it.
func (h *harness) signIn(t *testing.T, id, email string) *client {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateUser(t.Context(), store.User{ID: id, Email: email, Role: store.RoleUser, PasswordHash: string(hash), CreatedAt: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}
	c := h.browser(t)
	if _, err := c.auth.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: email, Password: password})); err != nil {
		t.Fatal(err)
	}
	return c
}

// device creates a device of the super user.
func (h *harness) device(t *testing.T, id, name, os string) store.Identity {
	t.Helper()
	d := store.Identity{Kind: store.KindDevice, ID: id, UserID: h.owner.ID, Name: name, MachineID: "m-" + id,
		OS: os, Arch: "amd64", ClientVersion: "0.1.0", CreatedAt: h.clock.Load()}
	if err := h.st.CreateIdentity(t.Context(), d); err != nil {
		t.Fatal(err)
	}
	return d
}

func wantCode(t *testing.T, err error, code connect.Code) {
	t.Helper()
	if connect.CodeOf(err) != code {
		t.Fatalf("error = %v, want %v", err, code)
	}
}

// fieldOf returns the field of the error's BadRequest violation, or "".
func fieldOf(err error) string {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return ""
	}
	for _, d := range ce.Details() {
		if v, e := d.Value(); e == nil {
			if b, ok := v.(*errdetails.BadRequest); ok && len(b.FieldViolations) == 1 {
				return b.FieldViolations[0].Field
			}
		}
	}
	return ""
}

func wantField(t *testing.T, err error, code connect.Code, field string) {
	t.Helper()
	wantCode(t, err, code)
	if got := fieldOf(err); got != field {
		t.Fatalf("error %v names field %q, want %q", err, got, field)
	}
}

func loc(device, path string) *circusv1.Project_Location {
	return &circusv1.Project_Location{Device: device, Path: path}
}

func (c *client) create(t *testing.T, name string, locs ...*circusv1.Project_Location) (*circusv1.Project, error) {
	t.Helper()
	resp, err := c.projects.CreateProject(t.Context(), connect.NewRequest(&circusv1.CreateProjectRequest{Name: name, Locations: locs}))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Project, nil
}

func (c *client) mustCreate(t *testing.T, name string, locs ...*circusv1.Project_Location) *circusv1.Project {
	t.Helper()
	p, err := c.create(t, name, locs...)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (c *client) update(t *testing.T, req *circusv1.UpdateProjectRequest) (*circusv1.Project, error) {
	t.Helper()
	resp, err := c.projects.UpdateProject(t.Context(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.Project, nil
}

func (c *client) list(t *testing.T) []*circusv1.Project {
	t.Helper()
	resp, err := c.projects.ListProjects(t.Context(), connect.NewRequest(&circusv1.ListProjectsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.Projects
}

func (c *client) devices(t *testing.T) []*circusv1.Device {
	t.Helper()
	resp, err := c.machines.ListDevices(t.Context(), connect.NewRequest(&circusv1.ListDevicesRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.Devices
}

func names(ps []*circusv1.Project) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func locations(ls ...*circusv1.Project_Location) *circusv1.UpdateProjectRequest_LocationList {
	return &circusv1.UpdateProjectRequest_LocationList{Locations: ls}
}

func TestAuthRequired(t *testing.T) {
	h, _ := newHarness(t)
	anon := h.browser(t)
	ctx := t.Context()
	_, err := anon.projects.CreateProject(ctx, connect.NewRequest(&circusv1.CreateProjectRequest{Name: "x"}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = anon.projects.ListProjects(ctx, connect.NewRequest(&circusv1.ListProjectsRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = anon.projects.GetProject(ctx, connect.NewRequest(&circusv1.GetProjectRequest{ProjectId: "p"}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = anon.projects.UpdateProject(ctx, connect.NewRequest(&circusv1.UpdateProjectRequest{ProjectId: "p"}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = anon.projects.DeleteProject(ctx, connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: "p"}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = anon.machines.ListDevices(ctx, connect.NewRequest(&circusv1.ListDevicesRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)

	// A signed-out browser is unauthenticated again.
	c := h.signIn(t, "u-carol", "carol@example.com")
	c.list(t)
	if _, err := c.auth.Logout(ctx, connect.NewRequest(&circusv1.LogoutRequest{})); err != nil {
		t.Fatal(err)
	}
	_, err = c.projects.ListProjects(ctx, connect.NewRequest(&circusv1.ListProjectsRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestOriginChecked(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "web", loc("box", "/src"))
	foreign := func(req connect.AnyRequest) {
		req.Header().Set("Origin", "https://evil.example")
	}
	ctx := t.Context()
	create := connect.NewRequest(&circusv1.CreateProjectRequest{Name: "x", Locations: []*circusv1.Project_Location{loc("box", "/x")}})
	foreign(create)
	_, err := super.projects.CreateProject(ctx, create)
	wantCode(t, err, connect.CodePermissionDenied)
	update := connect.NewRequest(&circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new("evil")})
	foreign(update)
	_, err = super.projects.UpdateProject(ctx, update)
	wantCode(t, err, connect.CodePermissionDenied)
	del := connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId})
	foreign(del)
	_, err = super.projects.DeleteProject(ctx, del)
	wantCode(t, err, connect.CodePermissionDenied)
	// Reads follow the interceptor's rule too.
	list := connect.NewRequest(&circusv1.ListProjectsRequest{})
	foreign(list)
	_, err = super.projects.ListProjects(ctx, list)
	wantCode(t, err, connect.CodePermissionDenied)
	devices := connect.NewRequest(&circusv1.ListDevicesRequest{})
	foreign(devices)
	_, err = super.machines.ListDevices(ctx, devices)
	wantCode(t, err, connect.CodePermissionDenied)

	// Nothing changed; the right Origin works.
	if got := names(super.list(t)); len(got) != 1 || got[0] != "web" {
		t.Fatalf("projects = %v", got)
	}
	same := connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId})
	same.Header().Set("Origin", h.server.URL)
	if _, err := super.projects.DeleteProject(ctx, same); err != nil {
		t.Fatal(err)
	}
}

func TestProjectCRUD(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	h.device(t, "d2", "win", "windows")
	start := h.clock.Load()

	p := super.mustCreate(t, "  circus  ", loc("box", "/src/circus/"), loc("win", "d:/work/"))
	if p.Name != "circus" || p.LastUsedLocation != 0 || p.CreatedAtUnixMs != start || p.UpdatedAtUnixMs != start || p.ProjectId == "" {
		t.Fatalf("created = %v", p)
	}
	wantLocs := []*circusv1.Project_Location{loc("box", "/src/circus"), loc("win", `D:\work`)}
	if len(p.Locations) != 2 || p.Locations[0].Path != wantLocs[0].Path || p.Locations[1].Path != wantLocs[1].Path || p.Locations[1].Device != "win" {
		t.Fatalf("locations = %v", p.Locations)
	}
	got, err := super.projects.GetProject(t.Context(), connect.NewRequest(&circusv1.GetProjectRequest{ProjectId: p.ProjectId}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.Project.String() != p.String() {
		t.Fatalf("get = %v, want %v", got.Msg.Project, p)
	}

	// Ordered by name ignoring case, then id.
	super.mustCreate(t, "Alpha", loc("box", "/a"))
	super.mustCreate(t, "beta", loc("box", "/b"))
	if got := names(super.list(t)); strings.Join(got, ",") != "Alpha,beta,circus" {
		t.Fatalf("list = %v", got)
	}

	// Rename, also to its own name in another case.
	h.clock.Add(1000)
	up, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new(" CIRCUS ")})
	if err != nil {
		t.Fatal(err)
	}
	if up.Name != "CIRCUS" || up.UpdatedAtUnixMs != start+1000 || up.CreatedAtUnixMs != start || len(up.Locations) != 2 {
		t.Fatalf("renamed = %v", up)
	}
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new("ALPHA")})
	wantField(t, err, connect.CodeAlreadyExists, "name")

	// Explicit last used location.
	up, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, LastUsedLocation: new(uint32(1))})
	if err != nil || up.LastUsedLocation != 1 {
		t.Fatalf("last used = %v, %v", up, err)
	}
	for _, i := range []uint32{2, 16, 1 << 31} {
		_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, LastUsedLocation: new(i)})
		wantField(t, err, connect.CodeInvalidArgument, "last_used_location")
	}

	// Delete.
	if _, err := super.projects.DeleteProject(t.Context(), connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId})); err != nil {
		t.Fatal(err)
	}
	_, err = super.projects.GetProject(t.Context(), connect.NewRequest(&circusv1.GetProjectRequest{ProjectId: p.ProjectId}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = super.projects.DeleteProject(t.Context(), connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestLastUsedFollowsLocation(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "mbp", "darwin")
	p := super.mustCreate(t, "multi", loc("mbp", "/a"), loc("mbp", "/b"), loc("mbp", "/c"))
	if _, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, LastUsedLocation: new(uint32(2))}); err != nil {
		t.Fatal(err)
	}
	// The default location is gone: clamped into range.
	shrunk, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Locations: locations(loc("mbp", "/x"), loc("mbp", "/y"))})
	if err != nil || shrunk.LastUsedLocation != 1 {
		t.Fatalf("shrunk = %v, %v", shrunk, err)
	}
	// A surviving location stays the default even if it moved, also when
	// the request spells it differently.
	moved, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Locations: locations(loc("mbp", "/y/"), loc("mbp", "/z"), loc("mbp", "/w"))})
	if err != nil || moved.LastUsedLocation != 0 {
		t.Fatalf("moved = %v, %v", moved, err)
	}
	// An explicit index in the same request wins, and is checked against
	// the new locations.
	both, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Locations: locations(loc("mbp", "/y"), loc("mbp", "/z")), LastUsedLocation: new(uint32(1))})
	if err != nil || both.LastUsedLocation != 1 {
		t.Fatalf("explicit = %v, %v", both, err)
	}
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Locations: locations(loc("mbp", "/y")), LastUsedLocation: new(uint32(1))})
	wantField(t, err, connect.CodeInvalidArgument, "last_used_location")
	// Name and locations together, all or nothing.
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new("renamed"), Locations: locations(loc("ghost", "/y"))})
	wantField(t, err, connect.CodeInvalidArgument, "locations[0].device")
	if got := names(super.list(t)); got[0] != "multi" {
		t.Fatalf("names = %v", got)
	}
	// Absent fields leave the project unchanged.
	same, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId})
	if err != nil || same.String() != both.String() {
		t.Fatalf("empty update = %v, %v", same, err)
	}
}

func TestProjectValidation(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "mbp", "darwin")
	h.device(t, "d2", "eric-win", "windows")
	h.device(t, "d3", "plain", "")
	mbp := func(path string) *circusv1.Project_Location { return loc("mbp", path) }
	win := func(path string) *circusv1.Project_Location { return loc("eric-win", path) }
	existing := super.mustCreate(t, "circus", mbp("/src/circus"))

	type tc struct {
		name  string
		locs  []*circusv1.Project_Location
		code  connect.Code
		field string
	}
	long := strings.Repeat("x", 65)
	for _, c := range []tc{
		{"", []*circusv1.Project_Location{mbp("/a")}, connect.CodeInvalidArgument, "name"},
		{"   ", []*circusv1.Project_Location{mbp("/a")}, connect.CodeInvalidArgument, "name"},
		{long, []*circusv1.Project_Location{mbp("/a")}, connect.CodeInvalidArgument, "name"},
		{strings.Repeat("語", 65), []*circusv1.Project_Location{mbp("/a")}, connect.CodeInvalidArgument, "name"},
		{"a\tb", []*circusv1.Project_Location{mbp("/a")}, connect.CodeInvalidArgument, "name"},
		{"a\x7fb", []*circusv1.Project_Location{mbp("/a")}, connect.CodeInvalidArgument, "name"},
		{"Circus", []*circusv1.Project_Location{mbp("/a")}, connect.CodeAlreadyExists, "name"},
		{" CIRCUS ", []*circusv1.Project_Location{mbp("/a")}, connect.CodeAlreadyExists, "name"},
		{"n", []*circusv1.Project_Location{mbp("/a"), loc("ghost", "/a")}, connect.CodeInvalidArgument, "locations[1].device"},
		{"n", []*circusv1.Project_Location{loc("", "/a")}, connect.CodeInvalidArgument, "locations[0].device"},
		{"n", []*circusv1.Project_Location{loc(strings.Repeat("d", 129), "/a")}, connect.CodeInvalidArgument, "locations[0].device"},
		{"n", []*circusv1.Project_Location{mbp("rel")}, connect.CodeInvalidArgument, "locations[0].path"},
		{"n", []*circusv1.Project_Location{mbp("src/relative")}, connect.CodeInvalidArgument, "locations[0].path"},
		{"n", []*circusv1.Project_Location{mbp(""), mbp("  ")}, connect.CodeInvalidArgument, "locations[1].path"},
		{"n", []*circusv1.Project_Location{win(`\\`)}, connect.CodeInvalidArgument, "locations[0].path"},
		{"n", []*circusv1.Project_Location{win("/not/windows")}, connect.CodeInvalidArgument, "locations[0].path"},
		{"n", []*circusv1.Project_Location{mbp("/a"), mbp("/a")}, connect.CodeInvalidArgument, "locations[1].path"},
		{"n", []*circusv1.Project_Location{mbp("/src/a"), mbp("/src/a/")}, connect.CodeInvalidArgument, "locations[1].path"},
		{"w", []*circusv1.Project_Location{win(`D:\work`), win("d:/work/")}, connect.CodeInvalidArgument, "locations[1].path"},
		{"n", []*circusv1.Project_Location{mbp("/" + strings.Repeat("x", 4096))}, connect.CodeInvalidArgument, "locations[0].path"},
		{"n", []*circusv1.Project_Location{mbp("/a\x00")}, connect.CodeInvalidArgument, "locations[0].path"},
		// A device without a recorded OS uses POSIX rules.
		{"n", []*circusv1.Project_Location{loc("plain", `C:\x`)}, connect.CodeInvalidArgument, "locations[0].path"},
		// The first problem in request order: a device before a path.
		{"n", []*circusv1.Project_Location{mbp("rel"), loc("ghost", "/a")}, connect.CodeInvalidArgument, "locations[0].path"},
	} {
		_, err := super.create(t, c.name, c.locs...)
		if connect.CodeOf(err) != c.code || fieldOf(err) != c.field {
			t.Errorf("create %q %v: error %v (field %q), want %v %q", c.name, c.locs, err, fieldOf(err), c.code, c.field)
		}
	}
	var many []*circusv1.Project_Location
	for i := range 17 {
		many = append(many, mbp("/"+strings.Repeat("a", i+1)))
	}
	_, err := super.create(t, "n", many...)
	wantField(t, err, connect.CodeInvalidArgument, "locations")
	if _, err := super.create(t, "n", many[:16]...); err != nil {
		t.Fatal(err)
	}

	// What is accepted, normalized.
	if _, err := super.create(t, long[:64], mbp("/src")); err != nil {
		t.Fatal(err)
	}
	ok := super.mustCreate(t, "ok", mbp("/src/a/"), win(`D:\work`), win("c:/src/app"), win("//server/share/"), loc("plain", "/"), mbp("/src/b"))
	var paths []string
	for _, l := range ok.Locations {
		paths = append(paths, l.Device+":"+l.Path)
	}
	want := `mbp:/src/a|eric-win:D:\work|eric-win:C:\src\app|eric-win:\\server\share|plain:/|mbp:/src/b`
	if strings.Join(paths, "|") != want {
		t.Fatalf("paths = %v", paths)
	}

	// Update reports the same fields.
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: existing.ProjectId, Name: new("")})
	wantField(t, err, connect.CodeInvalidArgument, "name")
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: existing.ProjectId, Name: new("OK")})
	wantField(t, err, connect.CodeAlreadyExists, "name")
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: existing.ProjectId, Locations: locations(many...)})
	wantField(t, err, connect.CodeInvalidArgument, "locations")
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: existing.ProjectId, Locations: locations(mbp("/a"), win("x"))})
	wantField(t, err, connect.CodeInvalidArgument, "locations[1].path")
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: strings.Repeat("p", 129)})
	wantField(t, err, connect.CodeInvalidArgument, "project_id")
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: "", Name: new("x")})
	wantCode(t, err, connect.CodeNotFound)
}

func TestProjectsIsolated(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "secret", loc("box", "/s"))
	carol := h.signIn(t, "u-carol", "carol@example.com")
	ctx := t.Context()

	if got := carol.list(t); len(got) != 0 {
		t.Fatalf("carol sees %v", got)
	}
	_, err := carol.projects.GetProject(ctx, connect.NewRequest(&circusv1.GetProjectRequest{ProjectId: p.ProjectId}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = carol.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new("mine")})
	wantCode(t, err, connect.CodeNotFound)
	_, err = carol.projects.DeleteProject(ctx, connect.NewRequest(&circusv1.DeleteProjectRequest{ProjectId: p.ProjectId}))
	wantCode(t, err, connect.CodeNotFound)
	_, err = super.projects.GetProject(ctx, connect.NewRequest(&circusv1.GetProjectRequest{ProjectId: "unknown"}))
	wantCode(t, err, connect.CodeNotFound)

	// This user has enrolled no devices, so no location is valid.
	_, err = carol.create(t, "secret", loc("box", "/s"))
	wantField(t, err, connect.CodeInvalidArgument, "locations[0].device")
	if got := super.list(t); len(got) != 1 || got[0].Name != "secret" {
		t.Fatalf("super's projects = %v", got)
	}
}

func TestDevices(t *testing.T) {
	h, super := newHarness(t)
	ctx := t.Context()
	if got := super.devices(t); len(got) != 0 {
		t.Fatalf("devices before enrollment = %v", got)
	}
	h.device(t, "d1", "zeta", "linux")
	h.device(t, "d2", "alpha", "windows")
	h.device(t, "d3", "mid", "darwin")
	gone := h.device(t, "d4", "gone", "linux")
	if _, err := h.st.DeleteIdentity(ctx, store.KindDevice, gone.ID, h.clock.Load()); err != nil {
		t.Fatal(err)
	}
	// A device of another owner is never listed.
	if err := h.st.CreateUser(ctx, store.User{ID: "other-owner", Name: "other", CreatedAt: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}
	if err := h.st.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "d5", UserID: "other-owner", Name: "foreign", MachineID: "m5", CreatedAt: h.clock.Load()}); err != nil {
		t.Fatal(err)
	}

	// d1 connected and disconnected; d2 connected and is online now; d3
	// never connected. d2's stored times say disconnected, but the live
	// link decides.
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	t1 := h.clock.Load() - 60_000
	_, err := h.st.SetConnected(ctx, store.KindDevice, "d1", "boot-1", t1)
	must(err)
	must(h.st.SetDisconnected(ctx, store.KindDevice, "d1", t1+5_000))
	_, err = h.st.SetConnected(ctx, store.KindDevice, "d2", "boot-2", t1)
	must(err)
	must(h.st.SetDisconnected(ctx, store.KindDevice, "d2", t1+1_000))
	h.setOnline("d2", true)
	// A row left connected by a process that died: offline, last seen at
	// its connect.
	_, err = h.st.SetConnected(ctx, store.KindDevice, "d3", "boot-3", t1+7_000)
	must(err)

	now := h.clock.Load()
	got := super.devices(t)
	type row struct {
		name     string
		online   bool
		lastSeen int64
		os       string
	}
	var rows []row
	for _, d := range got {
		rows = append(rows, row{d.Name, d.Online, d.LastSeenUnixMs, d.Os})
		if d.Arch != "amd64" || d.Version != "0.1.0" {
			t.Fatalf("device = %v", d)
		}
	}
	want := []row{{"alpha", true, now, "windows"}, {"mid", false, t1 + 7_000, "darwin"}, {"zeta", false, t1 + 5_000, "linux"}}
	if len(rows) != len(want) {
		t.Fatalf("devices = %+v", rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("devices = %+v, want %+v", rows, want)
		}
	}
	h.setOnline("d2", false)
	h.device(t, "d6", "fresh", "linux")
	for _, d := range super.devices(t) {
		if d.Name == "alpha" && (d.Online || d.LastSeenUnixMs != t1+1_000) {
			t.Fatalf("alpha after disconnect = %v", d)
		}
		if d.Name == "fresh" && (d.Online || d.LastSeenUnixMs != 0) {
			t.Fatalf("never connected = %v", d)
		}
	}

	// A user who has enrolled no devices sees an empty list.
	carol := h.signIn(t, "u-carol", "carol@example.com")
	if got := carol.devices(t); len(got) != 0 {
		t.Fatalf("carol's devices = %v", got)
	}
}

func TestRevokedDevice(t *testing.T) {
	h, super := newHarness(t)
	d := h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "p", loc("box", "/a"))
	if _, err := h.st.DeleteIdentity(t.Context(), store.KindDevice, d.ID, h.clock.Load()); err != nil {
		t.Fatal(err)
	}
	// The project keeps its location; it can no longer be bound again.
	got := super.list(t)
	if len(got) != 1 || got[0].Locations[0].Device != "box" || !got[0].Locations[0].DeviceRevoked {
		t.Fatalf("projects = %v", got)
	}
	_, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Locations: locations(loc("box", "/a"))})
	wantField(t, err, connect.CodeInvalidArgument, "locations[0].device")
	// A re-enrolled device of the same name is a new device: saving the
	// same locations binds it.
	h.device(t, "d2", "box", "linux")
	up, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Locations: locations(loc("box", "/a"))})
	if err != nil || up.Locations[0].Device != "box" || up.Locations[0].DeviceRevoked || up.LastUsedLocation != 0 {
		t.Fatalf("rebound = %v, %v", up, err)
	}
}

func TestConcurrentCreate(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Go(func() {
			name := "Shared"
			if i%2 == 1 {
				name = "sHARED"
			}
			_, errs[i] = super.create(t, name, loc("box", "/a"))
		})
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if connect.CodeOf(err) != connect.CodeAlreadyExists || fieldOf(err) != "name" {
			t.Fatalf("create: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d creates won", wins)
	}
}

func TestRequestLimit(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	_, err := super.create(t, strings.Repeat(" ", 300<<10)+"x", loc("box", "/a"))
	wantCode(t, err, connect.CodeResourceExhausted)
	// The largest valid request fits.
	var locs []*circusv1.Project_Location
	for i := range 16 {
		locs = append(locs, loc("box", "/"+strings.Repeat(string(rune('a'+i)), 4095)))
	}
	if _, err := super.create(t, strings.Repeat("語", 64), locs...); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalParts(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "mbp", "darwin")
	h.device(t, "d2", "win", "windows")
	ctx := t.Context()

	// Only a name; only a repository URL.
	bare := super.mustCreate(t, "bare")
	if len(bare.Locations) != 0 || bare.LastUsedLocation != 0 || bare.RepoUrl != "" {
		t.Fatalf("bare = %v", bare)
	}
	resp, err := super.projects.CreateProject(ctx, connect.NewRequest(&circusv1.CreateProjectRequest{Name: "repo", RepoUrl: "  git@github.com:owner/repo.git "}))
	if err != nil || resp.Msg.Project.RepoUrl != "git@github.com:owner/repo.git" || len(resp.Msg.Project.Locations) != 0 {
		t.Fatalf("repo-only = %v, %v", resp, err)
	}
	_, err = super.projects.CreateProject(ctx, connect.NewRequest(&circusv1.CreateProjectRequest{Name: "bad", RepoUrl: "github.com/owner/repo"}))
	wantField(t, err, connect.CodeInvalidArgument, "repo_url")

	// Default locations resolve under the project id; an explicit path
	// resolves to itself. effective_path on input is ignored.
	in := loc("mbp", "")
	in.EffectivePath = "/ignored"
	p := super.mustCreate(t, "defaults", in, loc("mbp", "/src/x/"), loc("win", "  "))
	want := []string{"mbp||~/circus/projects/" + p.ProjectId, "mbp|/src/x|", "win||~/circus/projects/" + p.ProjectId}
	for i, l := range p.Locations {
		if got := l.Device + "|" + l.Path + "|" + l.EffectivePath; got != want[i] {
			t.Fatalf("location %d = %q, want %q", i, got, want[i])
		}
	}
	// One default per device; the later one is named.
	_, err = super.create(t, "twice", loc("mbp", "/a"), loc("mbp", ""), loc("win", ""), loc("mbp", " "))
	wantField(t, err, connect.CodeInvalidArgument, "locations[3].path")
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Locations: locations(loc("win", ""), loc("win", ""))})
	wantField(t, err, connect.CodeInvalidArgument, "locations[1].path")

	// Renaming keeps the default directory (it uses the id).
	up, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new("renamed")})
	if err != nil || up.Locations[0].EffectivePath != "~/circus/projects/"+p.ProjectId {
		t.Fatalf("renamed = %v, %v", up, err)
	}

	// All locations removed: last_used_location is 0 and only 0 is accepted.
	if _, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, LastUsedLocation: new(uint32(2))}); err != nil {
		t.Fatal(err)
	}
	up, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Locations: locations()})
	if err != nil || len(up.Locations) != 0 || up.LastUsedLocation != 0 {
		t.Fatalf("emptied = %v, %v", up, err)
	}
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, LastUsedLocation: new(uint32(1))})
	wantField(t, err, connect.CodeInvalidArgument, "last_used_location")
	if _, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, LastUsedLocation: new(uint32(0))}); err != nil {
		t.Fatal(err)
	}

	// Repository URL: set, kept when absent, cleared with "".
	up, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, RepoUrl: new("https://example.com/a.git")})
	if err != nil || up.RepoUrl != "https://example.com/a.git" {
		t.Fatalf("set = %v, %v", up, err)
	}
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, RepoUrl: new("https://user:pw@example.com/a.git")})
	wantField(t, err, connect.CodeInvalidArgument, "repo_url")
	up, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new("again")})
	if err != nil || up.RepoUrl != "https://example.com/a.git" {
		t.Fatalf("kept = %v, %v", up, err)
	}
	up, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, RepoUrl: new("")})
	if err != nil || up.RepoUrl != "" {
		t.Fatalf("cleared = %v, %v", up, err)
	}

	// A normal user may now create a project without locations.
	carol := h.signIn(t, "u-carol", "carol@example.com")
	if _, err := carol.create(t, "mine"); err != nil {
		t.Fatal(err)
	}
	_, err = carol.create(t, "theirs", loc("mbp", ""))
	wantField(t, err, connect.CodeInvalidArgument, "locations[0].device")
}

// Two tabs open the same project; the second save must not overwrite the
// first one's change unnoticed.
func TestTwoTabs(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "shared", loc("box", "/a"))
	tab1, tab2 := p, p // both read the same version

	// The clock does not move: the first save still moves updated_at.
	one, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new("from tab one"), ExpectedUpdatedAtUnixMs: new(tab1.UpdatedAtUnixMs)})
	if err != nil || one.UpdatedAtUnixMs <= p.UpdatedAtUnixMs {
		t.Fatalf("tab one = %v, %v", one, err)
	}
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Locations: locations(loc("box", "/b")), ExpectedUpdatedAtUnixMs: new(tab2.UpdatedAtUnixMs)})
	wantField(t, err, connect.CodeAborted, "")
	resp, err := super.projects.GetProject(t.Context(), connect.NewRequest(&circusv1.GetProjectRequest{ProjectId: p.ProjectId}))
	if err != nil || resp.Msg.Project.Name != "from tab one" || resp.Msg.Project.Locations[0].Path != "/a" {
		t.Fatalf("after the refused save = %v, %v", resp, err)
	}
	// Tab two reloads and saves again.
	two, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Locations: locations(loc("box", "/b")), ExpectedUpdatedAtUnixMs: new(resp.Msg.Project.UpdatedAtUnixMs)})
	if err != nil || two.Name != "from tab one" || two.Locations[0].Path != "/b" || two.UpdatedAtUnixMs <= one.UpdatedAtUnixMs {
		t.Fatalf("tab two = %v, %v", two, err)
	}
	// Without the field there is no check (last write wins).
	if _, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new("blind")}); err != nil {
		t.Fatal(err)
	}
}

// busyStore fails project reads and writes as a locked database does.
type busyStore struct{ store.Store }

func (busyStore) ListProjects(context.Context, string) ([]store.Project, error) {
	return nil, store.ErrBusy
}
func (busyStore) CreateProject(context.Context, store.Project) (store.Project, error) {
	return store.Project{}, store.ErrBusy
}

func TestBusyIsUnavailable(t *testing.T) {
	_, super := newHarness(t, func(c *webapi.Config) { c.Store = busyStore{c.Store} })
	_, err := super.projects.ListProjects(t.Context(), connect.NewRequest(&circusv1.ListProjectsRequest{}))
	wantCode(t, err, connect.CodeUnavailable)
	_, err = super.create(t, "x")
	wantCode(t, err, connect.CodeUnavailable)
}

func TestForbiddenCharacters(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	h.device(t, "d2", "win", "windows")
	for _, c := range []struct {
		name, repo string
		locs       []*circusv1.Project_Location
		field      string
	}{
		{"a\u202eb", "", nil, "name"},
		{"a\u200bb", "", nil, "name"},
		{"a\u0085b", "", nil, "name"},
		{"a\u200fb", "", nil, "name"},
		{"n", "git@host:a\u061cb", nil, "repo_url"},
		{"n", "", []*circusv1.Project_Location{loc("box", "/a\u2028b")}, "locations[0].path"},
		{"n", "https://host/a\u2066b", nil, "repo_url"},
		{"n", "", []*circusv1.Project_Location{loc("box", "/a\ufeffb")}, "locations[0].path"},
		{"n", "", []*circusv1.Project_Location{loc("box", "/a"), loc("box", "/b\u202d")}, "locations[1].path"},
		{"n", "", []*circusv1.Project_Location{loc("win", `\\?\C:\x`)}, "locations[0].path"},
		{"n", "", []*circusv1.Project_Location{loc("win", "//./pipe/x")}, "locations[0].path"},
		{"n", "git@[::1]:repo", nil, "repo_url"},
	} {
		_, err := super.projects.CreateProject(t.Context(), connect.NewRequest(&circusv1.CreateProjectRequest{Name: c.name, RepoUrl: c.repo, Locations: c.locs}))
		if connect.CodeOf(err) != connect.CodeInvalidArgument || fieldOf(err) != c.field {
			t.Errorf("%q %q %v: %v (field %q), want %q", c.name, c.repo, c.locs, err, fieldOf(err), c.field)
		}
	}
	// Collapsed POSIX paths are the same location.
	p := super.mustCreate(t, "collapsed", loc("box", "/a//./b/"))
	if p.Locations[0].Path != "/a/b" || p.Locations[0].EffectivePath != "" {
		t.Fatalf("collapsed = %v", p.Locations[0])
	}
	_, err := super.create(t, "dup", loc("box", "/a/b"), loc("box", "//a/./b"))
	wantField(t, err, connect.CodeInvalidArgument, "locations[1].path")
}

// TestListProjectsAtBound fills one user to every bound at once, with the
// strings that grow most in JSON, and checks that a full ListProjects is
// answered in both encodings; then that writes past the bounds are
// refused.
func TestListProjectsAtBound(t *testing.T) {
	if testing.Short() || raceEnabled {
		// It checks sizes, not concurrency; the fill of 17,000 rows takes
		// minutes under the race detector.
		t.Skip("fills a user to the project bounds")
	}
	h, super := newHarness(t)
	ctx := t.Context()
	var devices []store.Identity
	for i := range store.MaxProjectLocations {
		name := fmt.Sprintf("%02d", i) + strings.Repeat("d", store.MaxIDBytes-2)
		devices = append(devices, h.device(t, fmt.Sprintf("dev%02d", i), name, "windows"))
	}
	who, err := super.auth.WhoAmI(ctx, connect.NewRequest(&circusv1.WhoAmIRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	userID := who.Msg.GetUser().GetUserId()
	// Names of 64 bytes, each ending in a backslash (escaped in JSON);
	// the rest of the byte budget in paths made of backslashes, which the
	// store takes as given (the service would refuse them), on the first
	// projects; every other location a default one.
	budget := store.MaxProjectBytesPerUser
	err = h.st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		budget -= store.MaxProjectsPerUser * 64
		for i := range store.MaxProjectsPerUser {
			p := store.Project{ID: fmt.Sprintf("%036d", i), UserID: userID, Name: fmt.Sprintf("%04d", i) + strings.Repeat(`\`, 60),
				CreatedAt: h.clock.Load(), UpdatedAt: h.clock.Load()}
			for j, d := range devices {
				path := ""
				if budget > 0 {
					n := min(budget, store.MaxPathBytes)
					path = fmt.Sprintf("%04d%02d", i, j) + strings.Repeat(`\`, n-6)
					budget -= n
				}
				p.Locations = append(p.Locations, store.ProjectLocation{DeviceID: d.ID, Path: path})
			}
			if _, err := tx.CreateProject(ctx, p); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	list := super.list(t)
	if len(list) != store.MaxProjectsPerUser {
		t.Fatalf("listed %d projects", len(list))
	}
	resp := &circusv1.ListProjectsResponse{Projects: list}
	jsonBytes, err := protojson.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("full ListProjects: %d bytes binary, %d bytes JSON", proto.Size(resp), len(jsonBytes))
	if limit := 16 << 20; proto.Size(resp) > limit || len(jsonBytes) > limit {
		t.Fatalf("full list exceeds the %d-byte response limit", limit)
	}
	jsonClient := circusv1connect.NewProjectServiceClient(super.http, h.server.URL, connect.WithProtoJSON(), connect.WithReadMaxBytes(32<<20))
	got, err := jsonClient.ListProjects(ctx, connect.NewRequest(&circusv1.ListProjectsRequest{}))
	if err != nil || len(got.Msg.Projects) != store.MaxProjectsPerUser {
		t.Fatalf("JSON list: %v", err)
	}

	// Past the bounds: the project count, then the byte budget.
	_, err = super.create(t, "one more")
	wantCode(t, err, connect.CodeResourceExhausted)
	for _, p := range list[len(list)-2:] {
		if err := h.st.DeleteProject(ctx, userID, p.ProjectId, h.clock.Load()); err != nil {
			t.Fatal(err)
		}
	}
	// Two slots and 128 bytes are free: 64 + 65 bytes do not fit, 64 + 64
	// bytes do.
	if _, err := super.create(t, strings.Repeat("a", 64)); err != nil {
		t.Fatalf("a name within the freed bytes: %v", err)
	}
	create := func(name, repo string) error {
		_, err := super.projects.CreateProject(ctx, connect.NewRequest(&circusv1.CreateProjectRequest{Name: name, RepoUrl: repo}))
		return err
	}
	err = create(strings.Repeat("b", 60), "a@b:cd")
	wantCode(t, err, connect.CodeResourceExhausted)
	if !strings.Contains(err.Error(), "bytes") {
		t.Fatalf("refused for %v, want the byte budget", err)
	}
	if err := create(strings.Repeat("b", 59), "a@b:c"); err != nil {
		t.Fatalf("a project filling the budget exactly: %v", err)
	}
}

// A settings form stays current while sessions move last_used_location,
// and saving unchanged values does not move the version.
func TestInvisibleUpdates(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	p := super.mustCreate(t, "form", loc("box", "/a"), loc("box", "/b"))
	h.clock.Add(1000)

	// The form saves exactly what it shows (spelled as typed): no change.
	same, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new(" form "), RepoUrl: new(""),
		Locations: locations(loc("box", "/a/"), loc("box", "/b")), ExpectedUpdatedAtUnixMs: new(p.UpdatedAtUnixMs)})
	if err != nil || same.UpdatedAtUnixMs != p.UpdatedAtUnixMs {
		t.Fatalf("same-value save = %v, %v", same, err)
	}
	// A session start moves the default, without the form's version.
	moved, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, LastUsedLocation: new(uint32(1)), ExpectedUpdatedAtUnixMs: new(int64(1))})
	if err != nil || moved.LastUsedLocation != 1 || moved.UpdatedAtUnixMs != p.UpdatedAtUnixMs {
		t.Fatalf("last used only = %v, %v", moved, err)
	}
	// The open form, read before, still saves.
	saved, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new("renamed"), ExpectedUpdatedAtUnixMs: new(p.UpdatedAtUnixMs)})
	if err != nil || saved.Name != "renamed" || saved.LastUsedLocation != 1 || saved.UpdatedAtUnixMs != p.UpdatedAtUnixMs+1000 {
		t.Fatalf("form save = %v, %v", saved, err)
	}
	// Now the form's version is old: an edit is refused.
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: p.ProjectId, Name: new("again"), ExpectedUpdatedAtUnixMs: new(p.UpdatedAtUnixMs)})
	wantField(t, err, connect.CodeAborted, "")
}

type deviceListHooks struct{ gateway.Hooks }

func (deviceListHooks) OnConnected(context.Context, gateway.Connection) error    { return nil }
func (deviceListHooks) OnDisconnected(context.Context, gateway.Connection) error { return nil }

func TestDeviceListShowsDaemonHelloVersion(t *testing.T) {
	h, browser := newHarness(t)
	ctx := wait.Ctx(t)
	now := time.UnixMilli(h.clock.Load())
	token, _, err := auth.CreateEnrollToken(ctx, h.st, h.owner.ID, store.KindDevice, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	device, err := auth.Enroll(ctx, h.st, auth.EnrollRequest{
		Token: token, Kind: store.KindDevice, Name: "laptop", MachineID: "machine", ClientVersion: "0.1.0",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	version := func() string {
		devices := browser.devices(t)
		if len(devices) != 1 {
			t.Fatalf("ListDevices returned %d devices, want 1", len(devices))
		}
		return devices[0].GetVersion()
	}
	if got := version(); got != "0.1.0" {
		t.Fatalf("enrolled version = %q, want 0.1.0", got)
	}

	registry, err := gateway.NewRegistry(1)
	if err != nil {
		t.Fatal(err)
	}
	g, err := gateway.New(gateway.Config{
		InstanceID: "test", Clock: clock.Real{}, Auth: auth.NewAuthenticator(h.st, auth.Options{}), MetadataStore: h.st, Registry: registry, Hooks: deviceListHooks{},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(g)
	defer server.Close()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
		defer cancel()
		if err := g.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	}()
	conn, _, err := wire.DialWebSocket(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/link/daemon", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + device.Token}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, _, err := wire.DialHello(ctx, conn, wire.DaemonUplink, &circusv1.Hello{ProtocolVersion: wire.ProtocolVersion, BootId: "boot", ClientVersion: "0.2.0"}, device.Identity.ID, wire.Options{}); err != nil {
		t.Fatal(err)
	}
	var got string
	t.Cleanup(func() {
		if got != "0.2.0" {
			t.Errorf("ListDevices version = %q, want 0.2.0", got)
		}
	})
	wait.For(t, func() bool {
		got = version()
		return got == "0.2.0"
	})
}
