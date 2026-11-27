package webapi_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/controlplane/providers"
	"github.com/EricWvi/circus/internal/store"
	"github.com/google/uuid"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// The fake secrets of these tests; no response may ever contain them.
const (
	fakeKey   = "sk-fake-key-0123456789"
	fakeValue = "fake-secret-value-XYZW"
	fakeKey2  = "sk-fake-other-key-9876"
)

func fakeProvider(name string, workers ...string) *circusv1.Provider {
	return &circusv1.Provider{
		Name: name, Kind: circusv1.Provider_KIND_CLAUDE_CODE,
		Api: &circusv1.Provider_ApiEndpoint{BaseUrl: "https://gateway.example/v1", Scheme: circusv1.Provider_ApiEndpoint_SCHEME_BEARER,
			Key: fakeKey, FableModel: "fable-fake", OpusModel: "opus-fake", SonnetModel: "sonnet-fake"},
		Env:       []*circusv1.Provider_EnvVar{{Name: "FAKE_TOKEN", Value: proto.String(fakeValue)}, {Name: "EMPTY", Value: proto.String("")}},
		WorkerIds: workers, MaxSessions: 2,
	}
}

func (c *client) createProvider(t *testing.T, p *circusv1.Provider) (*circusv1.Provider, error) {
	t.Helper()
	resp, err := c.provs.CreateProvider(t.Context(), connect.NewRequest(&circusv1.CreateProviderRequest{Provider: p}))
	if err != nil {
		return nil, err
	}
	noSecrets(t, resp.Msg)
	return resp.Msg.GetProvider(), nil
}

func (c *client) mustProvider(t *testing.T, p *circusv1.Provider) *circusv1.Provider {
	t.Helper()
	got, err := c.createProvider(t, p)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (c *client) updateProvider(t *testing.T, p *circusv1.Provider, expected *int64) (*circusv1.Provider, error) {
	t.Helper()
	resp, err := c.provs.UpdateProvider(t.Context(), connect.NewRequest(&circusv1.UpdateProviderRequest{Provider: p, ExpectedUpdatedAtUnixMs: expected}))
	if err != nil {
		return nil, err
	}
	noSecrets(t, resp.Msg)
	return resp.Msg.GetProvider(), nil
}

func (c *client) getProvider(t *testing.T, id string) (*circusv1.Provider, error) {
	t.Helper()
	resp, err := c.provs.GetProvider(t.Context(), connect.NewRequest(&circusv1.GetProviderRequest{ProviderId: id}))
	if err != nil {
		return nil, err
	}
	noSecrets(t, resp.Msg)
	return resp.Msg.GetProvider(), nil
}

// noSecrets fails when a response holds any fake secret, in binary or JSON.
func noSecrets(t *testing.T, m proto.Message) {
	t.Helper()
	bin, err := proto.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	js, err := protojson.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{fakeKey, fakeValue, fakeKey2} {
		if bytes.Contains(bin, []byte(secret)) || bytes.Contains(js, []byte(secret)) {
			t.Fatalf("response holds a secret: %s", js)
		}
	}
}

func TestProviderCRUD(t *testing.T) {
	h, super := newHarness(t)
	h.addWorker(t, localWorker{id: "w1", machine: "m1"})
	h.addWorker(t, localWorker{id: "w2", machine: "m2"})

	created := super.mustProvider(t, fakeProvider("  Gateway  ", "w2", "w1"))
	want := &circusv1.Provider{
		ProviderId: created.GetProviderId(), Name: "Gateway", Kind: circusv1.Provider_KIND_CLAUDE_CODE,
		Api: &circusv1.Provider_ApiEndpoint{BaseUrl: "https://gateway.example/v1", Scheme: circusv1.Provider_ApiEndpoint_SCHEME_BEARER,
			KeyLast4: "6789", FableModel: "fable-fake", OpusModel: "opus-fake", SonnetModel: "sonnet-fake"},
		Env:       []*circusv1.Provider_EnvVar{{Name: "FAKE_TOKEN", ValueLast4: "XYZW"}, {Name: "EMPTY"}},
		WorkerIds: []string{"w2", "w1"}, MaxSessions: 2,
		CreatedAtUnixMs: h.clock.Load(), UpdatedAtUnixMs: h.clock.Load(),
	}
	if !proto.Equal(created, want) {
		t.Fatalf("created %v\nwant %v", created, want)
	}
	got, err := super.getProvider(t, created.GetProviderId())
	if err != nil || !proto.Equal(got, want) {
		t.Fatalf("get %v, %v", got, err)
	}
	// The stored blob holds no plaintext.
	stored, err := h.st.GetProvider(t.Context(), h.owner.ID, created.GetProviderId(), h.clock.Load())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.Secrets, []byte(fakeKey)) || bytes.Contains(stored.Secrets, []byte(fakeValue)) {
		t.Fatal("secrets stored in the clear")
	}

	// List by name ignoring case, then id.
	second := super.mustProvider(t, fakeProvider("alpha"))
	resp, err := super.provs.ListProviders(t.Context(), connect.NewRequest(&circusv1.ListProvidersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	noSecrets(t, resp.Msg)
	if ps := resp.Msg.GetProviders(); len(ps) != 2 || ps[0].GetName() != "alpha" || ps[1].GetName() != "Gateway" {
		t.Fatalf("list %v", ps)
	}

	// Update: an empty key keeps the stored key, a variable without a value
	// keeps its value, and a new value replaces it.
	h.clock.Add(10)
	up := fakeProvider("Gateway", "w1")
	up.ProviderId, up.Api.Key, up.IsDefault = created.GetProviderId(), "", true
	up.Env = []*circusv1.Provider_EnvVar{{Name: "EMPTY", Value: proto.String("now-set")}, {Name: "FAKE_TOKEN"}}
	updated, err := super.updateProvider(t, up, proto.Int64(created.GetUpdatedAtUnixMs()))
	if err != nil {
		t.Fatal(err)
	}
	if updated.GetApi().GetKeyLast4() != "6789" || updated.GetUpdatedAtUnixMs() != h.clock.Load() || !updated.GetIsDefault() ||
		len(updated.GetEnv()) != 2 || updated.GetEnv()[0].GetValueLast4() != "-set" || updated.GetEnv()[1].GetValueLast4() != "XYZW" {
		t.Fatalf("updated %v", updated)
	}
	wantCalls(t, "provider changes", h.sched.take(), h.owner.ID+":"+created.GetProviderId())
	stored, err = h.st.GetProvider(t.Context(), h.owner.ID, created.GetProviderId(), h.clock.Load())
	if err != nil {
		t.Fatal(err)
	}
	secrets, err := h.box.OpenProvider(stored)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := secrets.Value("FAKE_TOKEN"); secrets.Key != fakeKey || v != fakeValue {
		t.Fatal("kept secrets changed")
	}
	if u, err := h.st.GetUser(t.Context(), h.owner.ID); err != nil || u.DefaultProviderID != created.GetProviderId() {
		t.Fatalf("user default %q, %v", u.DefaultProviderID, err)
	}

	// The same request again changes nothing, not even the version, and
	// tells the scheduler nothing.
	h.clock.Add(10)
	again, err := super.updateProvider(t, up, nil)
	if err != nil || again.GetUpdatedAtUnixMs() != updated.GetUpdatedAtUnixMs() {
		t.Fatalf("repeat moved the version: %v, %v", again, err)
	}
	wantCalls(t, "no change", h.sched.take())
	// A stale version is ABORTED.
	_, err = super.updateProvider(t, up, proto.Int64(created.GetUpdatedAtUnixMs()))
	wantCode(t, err, connect.CodeAborted)
	// A new variable needs a value.
	up.Env = append(up.Env, &circusv1.Provider_EnvVar{Name: "NEW_ONE"})
	_, err = super.updateProvider(t, up, nil)
	wantField(t, err, connect.CodeInvalidArgument, "env[2].value")

	// Delete; the scheduler re-evaluates its sessions.
	if _, err := super.provs.DeleteProvider(t.Context(), connect.NewRequest(&circusv1.DeleteProviderRequest{ProviderId: second.GetProviderId()})); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, "delete", h.sched.take(), h.owner.ID+":"+second.GetProviderId())
	_, err = super.getProvider(t, second.GetProviderId())
	wantCode(t, err, connect.CodeNotFound)
	_, err = super.provs.DeleteProvider(t.Context(), connect.NewRequest(&circusv1.DeleteProviderRequest{ProviderId: second.GetProviderId()}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestProviderValidation(t *testing.T) {
	h, super := newHarness(t)
	h.addWorker(t, localWorker{id: "w1", machine: "m1"})
	h.addWorker(t, localWorker{id: "wx", machine: "mx", other: true})
	h.addWorker(t, localWorker{id: "wd", machine: "md", deleted: true})
	manyEnv := make([]*circusv1.Provider_EnvVar, store.MaxProviderEnv+1)
	for i := range manyEnv {
		manyEnv[i] = &circusv1.Provider_EnvVar{Name: fmt.Sprintf("V%d", i), Value: proto.String("")}
	}
	bigEnv := []*circusv1.Provider_EnvVar{
		{Name: "A", Value: proto.String(strings.Repeat("a", 40<<10))}, {Name: "B", Value: proto.String(strings.Repeat("b", 24<<10))},
	}
	manyWorkers := make([]string, store.MaxProviderWorkers+1)
	for i := range manyWorkers {
		manyWorkers[i] = fmt.Sprintf("w%d", i)
	}
	for _, tc := range []struct {
		field  string
		mutate func(*circusv1.Provider)
	}{
		{"name", func(p *circusv1.Provider) { p.Name = "  " }},
		{"name", func(p *circusv1.Provider) { p.Name = strings.Repeat("n", 65) }},
		{"name", func(p *circusv1.Provider) { p.Name = "a\tb" }},
		{"kind", func(p *circusv1.Provider) { p.Kind = circusv1.Provider_KIND_UNSPECIFIED }},
		{"api", func(p *circusv1.Provider) { p.Api = nil }},
		{"api.base_url", func(p *circusv1.Provider) { p.Api.BaseUrl = "ftp://host/" }},
		{"api.base_url", func(p *circusv1.Provider) { p.Api.BaseUrl = "/relative" }},
		{"api.base_url", func(p *circusv1.Provider) { p.Api.BaseUrl = "https://user:pw@host/" }},
		{"api.base_url", func(p *circusv1.Provider) { p.Api.BaseUrl = "https://host/?q=1" }},
		{"api.base_url", func(p *circusv1.Provider) { p.Api.BaseUrl = "https://host/#frag" }},
		{"api.base_url", func(p *circusv1.Provider) { p.Api.BaseUrl = "https://host/" + strings.Repeat("p", 2048) }},
		{"api.scheme", func(p *circusv1.Provider) { p.Api.Scheme = circusv1.Provider_ApiEndpoint_SCHEME_UNSPECIFIED }},
		{"api.key", func(p *circusv1.Provider) { p.Api.Key = "" }},
		{"api.key", func(p *circusv1.Provider) { p.Api.Key = strings.Repeat("k", 4097) }},
		{"api.key", func(p *circusv1.Provider) { p.Api.Key = "sk-fake\n" }},
		{"api.base_url", func(p *circusv1.Provider) { p.Api.BaseUrl = "https://host:99999" }},
		{"env[0].name", func(p *circusv1.Provider) { p.Env[0].Name = "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST" }},
		{"api.fable_model", func(p *circusv1.Provider) { p.Api.FableModel = "tab\tsep" }},
		{"api.opus_model", func(p *circusv1.Provider) { p.Api.OpusModel = "has space" }},
		{"api.sonnet_model", func(p *circusv1.Provider) { p.Api.SonnetModel = strings.Repeat("m", 257) }},
		{"api.haiku_model", func(p *circusv1.Provider) { p.Api.HaikuModel = "ctl\x01" }},
		{"env", func(p *circusv1.Provider) { p.Env = manyEnv }},
		{"env", func(p *circusv1.Provider) { p.Env = bigEnv }},
		{"env[1].name", func(p *circusv1.Provider) { p.Env[1].Name = "lower" }},
		{"env[0].name", func(p *circusv1.Provider) { p.Env[0].Name = "ANTHROPIC_API_KEY" }},
		{"env[0].name", func(p *circusv1.Provider) { p.Env[0].Name = "CIRCUS_X" }},
		{"env[0].name", func(p *circusv1.Provider) { p.Env[0].Name = "HOME" }},
		{"env[1].name", func(p *circusv1.Provider) { p.Env[1].Name = p.Env[0].Name }},
		{"env[0].value", func(p *circusv1.Provider) { p.Env[0].Value = nil }},
		{"env[0].value", func(p *circusv1.Provider) { p.Env[0].Value = proto.String("nul\x00") }},
		{"worker_ids", func(p *circusv1.Provider) { p.WorkerIds = manyWorkers }},
		{"worker_ids[0]", func(p *circusv1.Provider) { p.WorkerIds = []string{"nope"} }},
		{"worker_ids[1]", func(p *circusv1.Provider) { p.WorkerIds = []string{"w1", "wx"} }},
		{"worker_ids[0]", func(p *circusv1.Provider) { p.WorkerIds = []string{"wd"} }},
		{"worker_ids[1]", func(p *circusv1.Provider) { p.WorkerIds = []string{"w1", "w1"} }},
		{"max_sessions", func(p *circusv1.Provider) { p.MaxSessions = 1025 }},
	} {
		p := fakeProvider("Valid", "w1")
		tc.mutate(p)
		_, err := super.createProvider(t, p)
		wantField(t, err, connect.CodeInvalidArgument, tc.field)
	}
	// The largest valid provider passes.
	big := fakeProvider("Big", "w1")
	big.Api.Key, big.MaxSessions = strings.Repeat("k", 4096), 1024
	big.Env = []*circusv1.Provider_EnvVar{{Name: "A", Value: proto.String(strings.Repeat("a", 64<<10-2))}, {Name: "B", Value: proto.String("")}}
	super.mustProvider(t, big)

	// provider_id.
	_, err := super.getProvider(t, strings.Repeat("p", 129))
	wantField(t, err, connect.CodeInvalidArgument, "provider_id")
	_, err = super.getProvider(t, "")
	wantCode(t, err, connect.CodeNotFound)
	up := fakeProvider("Valid")
	up.ProviderId = strings.Repeat("p", 129)
	_, err = super.updateProvider(t, up, nil)
	wantField(t, err, connect.CodeInvalidArgument, "provider_id")
	_, err = super.provs.DeleteProvider(t.Context(), connect.NewRequest(&circusv1.DeleteProviderRequest{ProviderId: strings.Repeat("p", 129)}))
	wantField(t, err, connect.CodeInvalidArgument, "provider_id")

	// Names are unique ignoring case; at most 32 providers.
	super.mustProvider(t, fakeProvider("Dup"))
	_, err = super.createProvider(t, fakeProvider("DUP"))
	wantField(t, err, connect.CodeAlreadyExists, "name")
	for i := 2; i < store.MaxProvidersPerUser; i++ {
		super.mustProvider(t, fakeProvider(fmt.Sprintf("P%d", i)))
	}
	_, err = super.createProvider(t, fakeProvider("One too many"))
	wantCode(t, err, connect.CodeResourceExhausted)
}

func TestProviderOwnership(t *testing.T) {
	h, super := newHarness(t)
	h.addWorker(t, localWorker{id: "w1", machine: "m1"})
	bob := h.signIn(t, "bob", "bob@example.com")
	mine := super.mustProvider(t, fakeProvider("Mine", "w1"))

	// Another user's provider does not exist for bob.
	_, err := bob.getProvider(t, mine.GetProviderId())
	wantCode(t, err, connect.CodeNotFound)
	up := fakeProvider("Mine")
	up.ProviderId = mine.GetProviderId()
	_, err = bob.updateProvider(t, up, nil)
	wantCode(t, err, connect.CodeNotFound)
	_, err = bob.provs.DeleteProvider(t.Context(), connect.NewRequest(&circusv1.DeleteProviderRequest{ProviderId: mine.GetProviderId()}))
	wantCode(t, err, connect.CodeNotFound)
	// Nor can bob assign the owner's worker.
	_, err = bob.createProvider(t, fakeProvider("Bob", "w1"))
	wantField(t, err, connect.CodeInvalidArgument, "worker_ids[0]")
	resp, err := bob.provs.ListProviders(t.Context(), connect.NewRequest(&circusv1.ListProvidersRequest{}))
	if err != nil || len(resp.Msg.GetProviders()) != 0 {
		t.Fatalf("bob lists %v, %v", resp, err)
	}

	// Nor use it for a project default or a session.
	bobsProject, err := bob.projects.CreateProject(t.Context(), connect.NewRequest(&circusv1.CreateProjectRequest{Name: "B", DefaultProviderId: mine.GetProviderId()}))
	wantField(t, err, connect.CodeInvalidArgument, "default_provider_id")
	_ = bobsProject
}

func TestProviderUnreadableSecrets(t *testing.T) {
	h, super := newHarness(t)
	p := super.mustProvider(t, fakeProvider("Gateway"))
	// Reseal the stored secrets with another key, as after the key file
	// was replaced.
	other, err := providers.NewBox(bytes.Repeat([]byte{9}, providers.KeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	stored, err := h.st.GetProvider(t.Context(), h.owner.ID, p.GetProviderId(), h.clock.Load())
	if err != nil {
		t.Fatal(err)
	}
	stored.Secrets, err = other.Seal(stored.ID, providers.Secrets{Key: "x", Env: []providers.EnvValue{{Name: "FAKE_TOKEN"}, {Name: "EMPTY"}}})
	if err != nil {
		t.Fatal(err)
	}
	stored.UpdatedAt = h.clock.Load()
	if _, err := h.st.UpdateProvider(t.Context(), h.owner.ID, stored, nil); err != nil {
		t.Fatal(err)
	}
	got, err := super.getProvider(t, p.GetProviderId())
	if err != nil || !got.GetSecretsUnreadable() {
		t.Fatalf("unreadable not reported: %v, %v", got, err)
	}
	// An update must enter the key and every value again.
	up := fakeProvider("Gateway")
	up.ProviderId, up.Api.Key = p.GetProviderId(), ""
	_, err = super.updateProvider(t, up, nil)
	wantField(t, err, connect.CodeInvalidArgument, "api.key")
	up.Api.Key = fakeKey2
	up.Env[1].Value = nil
	_, err = super.updateProvider(t, up, nil)
	wantField(t, err, connect.CodeInvalidArgument, "env[1].value")
	up.Env[1].Value = proto.String("")
	got, err = super.updateProvider(t, up, nil)
	if err != nil || got.GetSecretsUnreadable() || got.GetApi().GetKeyLast4() != "9876" {
		t.Fatalf("recovered %v, %v", got, err)
	}
}

func TestProviderDeleteWhileLeased(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	h.addWorker(t, localWorker{id: "w1", machine: "m1"})
	p := super.mustProvider(t, fakeProvider("Gateway", "w1"))
	project := super.mustCreate(t, "Project", loc("box", "/work"))
	sess, err := super.createSession(t, &circusv1.CreateSessionRequest{ClientRequestId: "c1", ProjectId: project.ProjectId, ProviderId: proto.String(p.GetProviderId())})
	if err != nil {
		t.Fatal(err)
	}
	now := h.clock.Load()
	r, err := h.st.AcquireLease(t.Context(), store.LeaseRequest{SessionID: sess.GetSessionId(), WorkerID: "w1", BootID: "b", ConnID: "c", Now: now, ExpiresAt: now + 60_000})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := super.getProvider(t, p.GetProviderId()); got.GetLeasedSessions() != 1 {
		t.Fatalf("leased sessions %d", got.GetLeasedSessions())
	}
	_, err = super.provs.DeleteProvider(t.Context(), connect.NewRequest(&circusv1.DeleteProviderRequest{ProviderId: p.GetProviderId()}))
	wantCode(t, err, connect.CodeFailedPrecondition)
	if _, err := h.st.ReleaseLease(t.Context(), store.Fence{SessionID: sess.GetSessionId(), Epoch: r.Lease.Epoch, WorkerID: "w1", BootID: "b", ConnID: "c", Now: now}); err != nil {
		t.Fatal(err)
	}
	if _, err := super.provs.DeleteProvider(t.Context(), connect.NewRequest(&circusv1.DeleteProviderRequest{ProviderId: p.GetProviderId()})); err != nil {
		t.Fatal(err)
	}
	// The session keeps naming the deleted provider.
	if got := super.getSession(t, sess.GetSessionId()); got.GetProviderId() != p.GetProviderId() {
		t.Fatalf("session provider %q", got.GetProviderId())
	}
}

// TestCreateSessionProviderResolution: an absent provider_id resolves to
// the project's default, else the user's, else local login; a default
// naming a deleted provider counts as unset; "" is local login; the
// requested provider is part of the idempotent repeat.
func TestCreateSessionProviderResolution(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	bob := h.signIn(t, "bob", "bob@example.com")
	bobs := bob.mustProvider(t, fakeProvider("Bob's"))
	userDefault := fakeProvider("User default")
	userDefault.IsDefault = true
	ud := super.mustProvider(t, userDefault)
	pd := super.mustProvider(t, fakeProvider("Project default"))
	explicit := super.mustProvider(t, fakeProvider("Explicit"))
	project, err := super.projects.CreateProject(t.Context(), connect.NewRequest(&circusv1.CreateProjectRequest{
		Name: "With default", Locations: []*circusv1.Project_Location{loc("box", "/a")}, DefaultProviderId: pd.GetProviderId()}))
	if err != nil {
		t.Fatal(err)
	}
	withDefault := project.Msg.GetProject().GetProjectId()
	if project.Msg.GetProject().GetDefaultProviderId() != pd.GetProviderId() {
		t.Fatalf("project default %v", project.Msg.GetProject())
	}
	plain := super.mustCreate(t, "Plain", loc("box", "/b")).ProjectId

	create := func(project string, provider *string) (*circusv1.Session, error) {
		return super.createSession(t, &circusv1.CreateSessionRequest{ClientRequestId: uuid.NewString(), ProjectId: project, ProviderId: provider})
	}
	for _, tc := range []struct {
		name     string
		project  string
		provider *string
		want     string
	}{
		{"explicit", withDefault, proto.String(explicit.GetProviderId()), explicit.GetProviderId()},
		{"local login", withDefault, proto.String(""), ""},
		{"project default", withDefault, nil, pd.GetProviderId()},
		{"user default", plain, nil, ud.GetProviderId()},
	} {
		s, err := create(tc.project, tc.provider)
		if err != nil || s.GetProviderId() != tc.want {
			t.Fatalf("%s: provider %q, %v; want %q", tc.name, s.GetProviderId(), err, tc.want)
		}
	}
	for _, id := range []string{"nope", bobs.GetProviderId()} {
		_, err := create(plain, proto.String(id))
		wantField(t, err, connect.CodeInvalidArgument, "provider_id")
	}
	_, err = create(plain, proto.String(strings.Repeat("p", 129)))
	wantField(t, err, connect.CodeInvalidArgument, "provider_id")

	// Deleted defaults count as unset.
	for _, id := range []string{pd.GetProviderId(), ud.GetProviderId()} {
		if _, err := super.provs.DeleteProvider(t.Context(), connect.NewRequest(&circusv1.DeleteProviderRequest{ProviderId: id})); err != nil {
			t.Fatal(err)
		}
	}
	s, err := create(withDefault, nil)
	if err != nil || s.GetProviderId() != "" {
		t.Fatalf("deleted defaults: %q, %v", s.GetProviderId(), err)
	}

	// The idempotent repeat: an absent provider accepts the resolved one,
	// a different one conflicts.
	req := &circusv1.CreateSessionRequest{ClientRequestId: "repeat", ProjectId: plain, ProviderId: proto.String(explicit.GetProviderId())}
	first, err := super.createSession(t, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := super.createSession(t, req)
	if err != nil || again.GetSessionId() != first.GetSessionId() {
		t.Fatalf("repeat %v, %v", again, err)
	}
	req.ProviderId = nil
	if again, err = super.createSession(t, req); err != nil || again.GetSessionId() != first.GetSessionId() {
		t.Fatalf("repeat without provider %v, %v", again, err)
	}
	req.ProviderId = proto.String("")
	_, err = super.createSession(t, req)
	wantField(t, err, connect.CodeAlreadyExists, "client_request_id")
}

func TestUpdateSessionProvider(t *testing.T) {
	h, super := newHarness(t)
	_, sess := sessionSetup(t, h, super)
	id := sess.GetSessionId()
	h.addWorker(t, localWorker{id: "w1", machine: "m1"})
	p := super.mustProvider(t, fakeProvider("Gateway", "w1"))
	bob := h.signIn(t, "bob", "bob@example.com")
	bobs := bob.mustProvider(t, fakeProvider("Bob's"))
	update := func(req *circusv1.UpdateSessionRequest) (*circusv1.Session, error) {
		req.SessionId = id
		resp, err := super.sessions.UpdateSession(t.Context(), connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}

	_, err := update(&circusv1.UpdateSessionRequest{})
	wantCode(t, err, connect.CodeInvalidArgument)
	for _, bad := range []string{"nope", bobs.GetProviderId()} {
		_, err = update(&circusv1.UpdateSessionRequest{ProviderId: proto.String(bad)})
		wantField(t, err, connect.CodeInvalidArgument, "provider_id")
	}
	// Only the provider, with an idle lease: the lease is released and the
	// scheduler woken.
	f := h.lease(t, id)
	h.waker.take()
	got, err := update(&circusv1.UpdateSessionRequest{ProviderId: proto.String(p.GetProviderId())})
	if err != nil || got.GetProviderId() != p.GetProviderId() || got.GetTitle() != "" {
		t.Fatalf("update provider %v, %v", got, err)
	}
	if l, err := h.st.GetLease(t.Context(), id); err != nil || l.WorkerID != "" {
		t.Fatalf("lease kept: %+v, %v", l, err)
	}
	wantCalls(t, "wakes", h.waker.take(), id+":")
	if _, err := h.st.TakeInputs(t.Context(), f); err == nil {
		t.Fatal("the revoked lease still passes its fence")
	}
	// The same provider again changes nothing and wakes nobody; a title
	// alone still works.
	if _, err := update(&circusv1.UpdateSessionRequest{ProviderId: proto.String(p.GetProviderId()), Title: proto.String("Named")}); err != nil {
		t.Fatal(err)
	}
	wantCalls(t, "no wakes", h.waker.take())
	// A running turn refuses.
	f = h.lease(t, id)
	h.startTurn(t, f, 1)
	_, err = update(&circusv1.UpdateSessionRequest{ProviderId: proto.String("")})
	wantCode(t, err, connect.CodeFailedPrecondition)
	if got := super.getSession(t, id); got.GetProviderId() != p.GetProviderId() || got.GetTitle() != "Named" {
		t.Fatalf("refused update changed %v", got)
	}
	// The current provider again is no change: allowed while running, the
	// title applies, the lease stays.
	h.waker.take()
	if got, err := update(&circusv1.UpdateSessionRequest{ProviderId: proto.String(p.GetProviderId()), Title: proto.String("Running")}); err != nil || got.GetTitle() != "Running" {
		t.Fatalf("same provider while running: %v, %v", got, err)
	}
	if l, err := h.st.GetLease(t.Context(), id); err != nil || l.Epoch != f.Epoch || l.WorkerID == "" {
		t.Fatalf("same provider touched the lease: %+v, %v", l, err)
	}
	wantCalls(t, "no wake for no change", h.waker.take())
	// A stopped session refuses.
	if _, err := super.sessions.StopSession(t.Context(), connect.NewRequest(&circusv1.StopSessionRequest{SessionId: id})); err != nil {
		t.Fatal(err)
	}
	_, err = update(&circusv1.UpdateSessionRequest{ProviderId: proto.String("")})
	wantCode(t, err, connect.CodeFailedPrecondition)
	if got, err := update(&circusv1.UpdateSessionRequest{ProviderId: proto.String(p.GetProviderId()), Title: proto.String("Stopped")}); err != nil || got.GetTitle() != "Stopped" {
		t.Fatalf("same provider while stopped: %v, %v", got, err)
	}
	// Another user's session does not exist.
	_, err = bob.sessions.UpdateSession(t.Context(), connect.NewRequest(&circusv1.UpdateSessionRequest{SessionId: id, ProviderId: proto.String("")}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestSessionProviderAndQueueReason(t *testing.T) {
	h, super := newHarness(t)
	p, sess := sessionSetup(t, h, super)
	prov := super.mustProvider(t, fakeProvider("Gateway"))
	queued, err := super.createSession(t, &circusv1.CreateSessionRequest{ClientRequestId: "q", ProjectId: p.ProjectId, ProviderId: proto.String(prov.GetProviderId())})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := super.send(t, queued.GetSessionId(), "m1", "hello"); err != nil {
		t.Fatal(err)
	}
	h.sched.mu.Lock()
	h.sched.reasons[queued.GetSessionId()] = circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL
	h.sched.reasons[sess.GetSessionId()] = circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL // not queued: not shown
	h.sched.mu.Unlock()
	got := super.getSession(t, queued.GetSessionId())
	if got.GetState() != circusv1.Session_STATE_QUEUED || got.GetQueueReason() != circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL || got.GetProviderId() != prov.GetProviderId() {
		t.Fatalf("queued session %v", got)
	}
	if got := super.getSession(t, sess.GetSessionId()); got.GetQueueReason() != 0 || got.GetProviderId() != "" {
		t.Fatalf("idle session %v", got)
	}
	resp, err := super.sessions.ListSessions(t.Context(), connect.NewRequest(&circusv1.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range resp.Msg.GetSessions() {
		if s.GetSessionId() == queued.GetSessionId() && (s.GetQueueReason() != circusv1.QueueReason_QUEUE_REASON_PROVIDER_FULL || s.GetProviderId() != prov.GetProviderId()) {
			t.Fatalf("listed %v", s)
		}
	}
}

func TestProjectDefaultProvider(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux")
	prov := super.mustProvider(t, fakeProvider("Gateway"))
	project := super.mustCreate(t, "Project", loc("box", "/w"))
	_, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: project.ProjectId, DefaultProviderId: proto.String("nope")})
	wantField(t, err, connect.CodeInvalidArgument, "default_provider_id")
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: project.ProjectId, DefaultProviderId: proto.String(strings.Repeat("p", 129))})
	wantField(t, err, connect.CodeInvalidArgument, "default_provider_id")
	h.clock.Add(5)
	got, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: project.ProjectId, DefaultProviderId: proto.String(prov.GetProviderId()),
		ExpectedUpdatedAtUnixMs: proto.Int64(project.UpdatedAtUnixMs)})
	if err != nil || got.GetDefaultProviderId() != prov.GetProviderId() || got.GetUpdatedAtUnixMs() != h.clock.Load() {
		t.Fatalf("set default %v, %v", got, err)
	}
	// Stale, then no change.
	_, err = super.update(t, &circusv1.UpdateProjectRequest{ProjectId: project.ProjectId, DefaultProviderId: proto.String(""),
		ExpectedUpdatedAtUnixMs: proto.Int64(project.UpdatedAtUnixMs)})
	wantCode(t, err, connect.CodeAborted)
	h.clock.Add(5)
	same, err := super.update(t, &circusv1.UpdateProjectRequest{ProjectId: project.ProjectId, DefaultProviderId: proto.String(prov.GetProviderId())})
	if err != nil || same.GetUpdatedAtUnixMs() != got.GetUpdatedAtUnixMs() {
		t.Fatalf("same default moved the version: %v, %v", same, err)
	}
	if ps := super.list(t); len(ps) != 1 || ps[0].GetDefaultProviderId() != prov.GetProviderId() {
		t.Fatalf("list %v", ps)
	}
	resp, err := super.projects.GetProject(t.Context(), connect.NewRequest(&circusv1.GetProjectRequest{ProjectId: project.ProjectId}))
	if err != nil || resp.Msg.GetProject().GetDefaultProviderId() != prov.GetProviderId() {
		t.Fatalf("get %v, %v", resp, err)
	}
	// A deleted default stays named.
	if _, err := super.provs.DeleteProvider(t.Context(), connect.NewRequest(&circusv1.DeleteProviderRequest{ProviderId: prov.GetProviderId()})); err != nil {
		t.Fatal(err)
	}
	if ps := super.list(t); ps[0].GetDefaultProviderId() != prov.GetProviderId() {
		t.Fatalf("dangling default %v", ps[0])
	}
	_, err = super.projects.CreateProject(t.Context(), connect.NewRequest(&circusv1.CreateProjectRequest{Name: "Other", DefaultProviderId: prov.GetProviderId()}))
	wantField(t, err, connect.CodeInvalidArgument, "default_provider_id")
}

func TestListWorkers(t *testing.T) {
	h, super := newHarness(t)
	h.device(t, "d1", "box", "linux") // machine m-d1
	h.device(t, "d2", "attic", "linux")
	h.addWorker(t, localWorker{id: "w-b", machine: "m-d1"})
	h.addWorker(t, localWorker{id: "w-a", machine: "m-none", connectedAt: h.clock.Load() - 100})
	h.addWorker(t, localWorker{id: "w-gone", machine: "m-d1", deleted: true})
	h.addWorker(t, localWorker{id: "w-other", machine: "m-d1", other: true})
	h.mu.Lock()
	h.workers["w-b"] = true
	h.mu.Unlock()
	resp, err := super.machines.ListWorkers(t.Context(), connect.NewRequest(&circusv1.ListWorkersRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	want := []*circusv1.Worker{
		{WorkerId: "w-a", Name: "w-a", LastSeenUnixMs: h.clock.Load() - 100},
		{WorkerId: "w-b", Name: "w-b", Online: true, LastSeenUnixMs: h.clock.Load(), LocalDevices: []string{"box"}},
	}
	if got := resp.Msg.GetWorkers(); len(got) != len(want) || !proto.Equal(got[0], want[0]) || !proto.Equal(got[1], want[1]) {
		t.Fatalf("workers %v\nwant %v", got, want)
	}
	anon := h.browser(t)
	_, err = anon.machines.ListWorkers(t.Context(), connect.NewRequest(&circusv1.ListWorkersRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
}

func TestProviderServiceAuth(t *testing.T) {
	h, super := newHarness(t)
	anon := h.browser(t)
	ctx := t.Context()
	_, err := anon.provs.ListProviders(ctx, connect.NewRequest(&circusv1.ListProvidersRequest{}))
	wantCode(t, err, connect.CodeUnauthenticated)
	_, err = anon.provs.CreateProvider(ctx, connect.NewRequest(&circusv1.CreateProviderRequest{Provider: fakeProvider("x")}))
	wantCode(t, err, connect.CodeUnauthenticated)
	req := connect.NewRequest(&circusv1.CreateProviderRequest{Provider: fakeProvider("x")})
	req.Header().Set("Origin", "https://evil.example")
	_, err = super.provs.CreateProvider(ctx, req)
	wantCode(t, err, connect.CodePermissionDenied)
	list, err := super.provs.ListProviders(context.Background(), connect.NewRequest(&circusv1.ListProvidersRequest{}))
	if err != nil || len(list.Msg.GetProviders()) != 0 {
		t.Fatalf("a refused create wrote: %v, %v", list, err)
	}
}

// TestUpdateSessionProviderAnnouncesInterruptedTurn: a broken session can
// have a turn open (the holder started one to repair it); changing its
// provider releases the lease, which closes that turn with a
// TURN_INTERRUPTED boundary, and live watchers see it at once.
func TestUpdateSessionProviderAnnouncesInterruptedTurn(t *testing.T) {
	h, super := newHarness(t)
	_, sess := sessionSetup(t, h, super)
	id := sess.GetSessionId()
	p := super.mustProvider(t, fakeProvider("Gateway"))
	f := h.lease(t, id)
	ctx := t.Context()
	report := func(r store.TurnReport) {
		t.Helper()
		r.At = h.clock.Load()
		if r.Origin == "" {
			r.Origin = store.TurnFromInput
		}
		if _, err := h.st.ReportTurn(ctx, f, r); err != nil {
			t.Fatal(err)
		}
	}
	report(store.TurnReport{LocalSeq: 1, Phase: store.TurnStarted})
	report(store.TurnReport{LocalSeq: 1, Phase: store.TurnEnded, Outcome: store.TurnCompleted, TranscriptIncomplete: true})
	report(store.TurnReport{LocalSeq: 2, Phase: store.TurnStarted})
	before, err := h.st.GetSession(ctx, id, h.clock.Load())
	if err != nil || before.State != store.SessionBroken {
		t.Fatalf("session %+v, %v", before, err)
	}
	stream, _ := super.watch(t, id, uint64(before.LastEventSeq))
	if _, err := super.sessions.UpdateSession(ctx, connect.NewRequest(&circusv1.UpdateSessionRequest{SessionId: id, ProviderId: proto.String(p.GetProviderId())})); err != nil {
		t.Fatal(err)
	}
	after, err := h.st.GetSession(ctx, id, h.clock.Load())
	if err != nil || after.LastEventSeq <= before.LastEventSeq {
		t.Fatalf("no boundary recorded: %+v, %v", after, err)
	}
	collect(t, stream, after.LastEventSeq)
}

// TestSessionWorkerDisconnected: GetSession and ListSessions carry the
// scheduler's disconnect time of the session's lease holder, whatever the
// session's state, and zero for a session it does not mark.
func TestSessionWorkerDisconnected(t *testing.T) {
	h, super := newHarness(t)
	_, sess := sessionSetup(t, h, super)
	h.sched.mu.Lock()
	h.sched.gone[sess.GetSessionId()] = 1234
	h.sched.mu.Unlock()
	if got := super.getSession(t, sess.GetSessionId()); got.GetWorkerDisconnectedAtUnixMs() != 1234 {
		t.Fatalf("session %v, want disconnected at 1234", got)
	}
	resp, err := super.sessions.ListSessions(t.Context(), connect.NewRequest(&circusv1.ListSessionsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range resp.Msg.GetSessions() {
		found = found || s.GetSessionId() == sess.GetSessionId()
		if want := h.sched.gone[s.GetSessionId()]; s.GetWorkerDisconnectedAtUnixMs() != want {
			t.Fatalf("listed %v, want disconnected at %d", s, want)
		}
	}
	if !found {
		t.Fatal("session not listed")
	}
	h.sched.mu.Lock()
	delete(h.sched.gone, sess.GetSessionId())
	h.sched.mu.Unlock()
	if got := super.getSession(t, sess.GetSessionId()); got.GetWorkerDisconnectedAtUnixMs() != 0 {
		t.Fatalf("cleared session %v", got)
	}
}
