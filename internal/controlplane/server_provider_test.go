package controlplane_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/fakeworker"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"google.golang.org/protobuf/proto"
)

// The fake secrets of the flow below.
const (
	logKey    = "sk-fake-log-key-AAAA1111"
	logValue  = "fake-log-value-BBBB2222"
	logKey2   = "sk-fake-log-key-CCCC3333"
	logValue2 = "fake-log-value-DDDD4444"
)

// lockedBuffer is a log sink shared by goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestProviderSecretsNeverLogged runs the whole provider flow with the
// server and the fake worker logging at debug level: a provider created
// and updated through the API, a session that runs with it (acquisition,
// GetLaunchEnv, a turn), and checks that no secret reaches either log,
// while the worker did receive them. It also checks that the key file was
// created with mode 0600 and that a group-readable key file stops a start.
func TestProviderSecretsNeverLogged(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "provider.key")
	h := newServer(t, controlplane.Config{ProviderKeyFile: keyFile}, nil)
	ctx := wait.Ctx(t)
	if info, err := os.Stat(keyFile); err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("key file %v, %v", info, err)
	}
	var token string
	for line := range strings.SplitSeq(h.logs.String(), "\n") {
		var rec struct {
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.URL != "" {
			u, _ := url.Parse(rec.URL)
			token = u.Query().Get("token")
		}
	}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	t.Cleanup(client.CloseIdleConnections)
	setup, err := circusv1connect.NewAuthServiceClient(client, h.base).Setup(ctx, connect.NewRequest(&circusv1.SetupRequest{Token: token, Email: "admin@example.com", Password: "password-123"}))
	if err != nil {
		t.Fatal(err)
	}
	owner := setup.Msg.GetUser().GetUserId()
	if err := h.st.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "d1", UserID: owner, Name: "box", MachineID: "m1", OS: "linux", CreatedAt: time.Now().UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	enrollToken, _, err := auth.CreateEnrollToken(ctx, h.st, owner, store.KindWorker, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	worker, err := auth.Enroll(ctx, h.st, auth.EnrollRequest{Token: enrollToken, Kind: store.KindWorker, Name: "w", MachineID: "m2"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	provs := circusv1connect.NewProviderServiceClient(client, h.base)
	created, err := provs.CreateProvider(ctx, connect.NewRequest(&circusv1.CreateProviderRequest{Provider: &circusv1.Provider{
		Name: "Gateway", Kind: circusv1.Provider_KIND_CLAUDE_CODE,
		Api: &circusv1.Provider_ApiEndpoint{BaseUrl: "https://gateway.example", Scheme: circusv1.Provider_ApiEndpoint_SCHEME_BEARER, Key: logKey, HaikuModel: "haiku-fake"},
		Env: []*circusv1.Provider_EnvVar{{Name: "FAKE_SECRET", Value: proto.String(logValue)}}, WorkerIds: []string{worker.Identity.ID},
	}}))
	if err != nil {
		t.Fatal(err)
	}
	pid := created.Msg.GetProvider().GetProviderId()
	// Update it, entering new secrets, so the flow seals twice.
	up := proto.CloneOf(created.Msg.GetProvider())
	up.Api.Key, up.Env[0].Value = logKey2, proto.String(logValue2)
	if _, err := provs.UpdateProvider(ctx, connect.NewRequest(&circusv1.UpdateProviderRequest{Provider: up})); err != nil {
		t.Fatal(err)
	}
	project, err := circusv1connect.NewProjectServiceClient(client, h.base).CreateProject(ctx, connect.NewRequest(&circusv1.CreateProjectRequest{
		Name: "Project", Locations: []*circusv1.Project_Location{{Device: "box", Path: "/work"}}, DefaultProviderId: pid,
	}))
	if err != nil {
		t.Fatal(err)
	}
	api := circusv1connect.NewSessionServiceClient(client, h.base)
	sess, err := api.CreateSession(ctx, connect.NewRequest(&circusv1.CreateSessionRequest{ClientRequestId: "c", ProjectId: project.Msg.GetProject().GetProjectId()}))
	if err != nil {
		t.Fatal(err)
	}
	id := sess.Msg.GetSession().GetSessionId()
	if sess.Msg.GetSession().GetProviderId() != pid {
		t.Fatalf("session provider %q", sess.Msg.GetSession().GetProviderId())
	}

	var mu sync.Mutex
	var got *circusv1.SessionReply
	workerLogs := &lockedBuffer{}
	wctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- fakeworker.Run(wctx, fakeworker.Options{
			Credential:  credfile.File{ControlPlaneURL: h.base, Role: credfile.RoleWorker, PrincipalID: worker.Identity.ID, Token: worker.Token},
			StreamDelay: time.Millisecond, Logger: slog.New(slog.NewTextHandler(workerLogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
			LaunchEnv: func(sessionID string, r *circusv1.SessionReply) {
				mu.Lock()
				defer mu.Unlock()
				got = r
			},
		})
	}()
	t.Cleanup(func() {
		cancel()
		if err := wait.Recv(t, done); !errors.Is(err, context.Canceled) {
			t.Errorf("fake worker: %v", err)
		}
	})
	if _, err := api.SendMessage(ctx, connect.NewRequest(&circusv1.SendMessageRequest{SessionId: id, ClientMsgId: "m1", Text: "hello"})); err != nil {
		t.Fatal(err)
	}
	wait.For(t, func() bool {
		s, err := api.GetSession(ctx, connect.NewRequest(&circusv1.GetSessionRequest{SessionId: id}))
		return err == nil && s.Msg.GetLastTurnOutcome() == circusv1.TurnOutcome_TURN_OUTCOME_COMPLETED
	})
	mu.Lock()
	env := got.GetLaunchEnv()
	mu.Unlock()
	values := map[string]string{}
	for _, v := range env.GetEnv() {
		values[v.GetName()] = v.GetValue()
	}
	if env.GetProviderId() != pid || values["ANTHROPIC_AUTH_TOKEN"] != logKey2 || values["FAKE_SECRET"] != logValue2 {
		t.Fatalf("the worker did not get the secrets: %v", env.GetProviderId())
	}
	// Listing and reading the provider at debug level too.
	if _, err := provs.ListProviders(ctx, connect.NewRequest(&circusv1.ListProvidersRequest{})); err != nil {
		t.Fatal(err)
	}
	for name, logs := range map[string]string{"server": h.logs.String(), "worker": workerLogs.String()} {
		if logs == "" {
			t.Fatalf("no %s log", name)
		}
		for _, secret := range []string{logKey, logValue, logKey2, logValue2} {
			if strings.Contains(logs, secret) {
				t.Fatalf("the %s log holds a secret", name)
			}
		}
	}

	// A key file readable by the group stops the next start.
	if err := os.Chmod(keyFile, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := controlplane.New(context.Background(), h.st, controlplane.Config{Listen: "127.0.0.1:0", DevInsecure: true,
		ProviderKeyFile: keyFile, LogOutput: &lockedBuffer{}}); err == nil || !strings.Contains(err.Error(), "group or others") {
		t.Fatalf("group-readable key file: %v", err)
	}
}
