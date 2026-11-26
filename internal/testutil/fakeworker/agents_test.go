package fakeworker_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
	"github.com/EricWvi/circus/internal/testutil/fakeworker"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"golang.org/x/crypto/bcrypt"
)

// TestFakeWorkerAgents: started without Claude Code, the fake worker
// reports NOT_INSTALLED at its target; it installs on the user's wish and
// uninstalls when the wish goes, reporting each state.
func TestFakeWorkerAgents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
	defer cancel()
	dbPath := filepath.Join(t.TempDir(), "circus.db")
	sqlitetest.Seed(t, dbPath)
	st, err := sqlite.Open(ctx, dbPath, sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logs := &syncBuffer{}
	// The whole test runs within wait.Timeout; the production bcrypt cost
	// alone (the dummy hash at New, the user's at Setup) takes most of it
	// under the race detector, and all of it on a loaded runner.
	srv, err := controlplane.New(ctx, st, controlplane.Config{Listen: "127.0.0.1:0", DevInsecure: true, LogOutput: logs,
		BcryptCost: bcrypt.MinCost})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	base := "http://" + srv.Addr().String()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	setup, err := circusv1connect.NewAuthServiceClient(client, base).Setup(ctx, connect.NewRequest(&pb.SetupRequest{
		Token: setupToken(t, logs.String()), Email: "web@example.com", Password: "password-123",
	}))
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := auth.CreateEnrollToken(ctx, st, setup.Msg.GetUser().GetUserId(), store.KindWorker, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e, err := auth.Enroll(ctx, st, auth.EnrollRequest{Token: token, Kind: store.KindWorker, Name: "worker", MachineID: "m"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		err := fakeworker.Run(runCtx, fakeworker.Options{
			Credential: credfile.File{ControlPlaneURL: base, Role: credfile.RoleWorker, PrincipalID: e.Identity.ID, Token: e.Token},
			ClaudeCode: pb.AgentStatus_STATE_NOT_INSTALLED, AgentTarget: "9.9", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("fake worker: %v", err)
		}
	})
	t.Cleanup(func() {
		stop()
		wg.Wait()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
		client.CloseIdleConnections()
	})

	machines := circusv1connect.NewMachineServiceClient(client, base)
	agent := func() *pb.WorkerAgent {
		res, err := machines.ListWorkers(ctx, connect.NewRequest(&pb.ListWorkersRequest{}))
		if err != nil || len(res.Msg.GetWorkers()) != 1 || len(res.Msg.GetWorkers()[0].GetAgents()) != 1 {
			return nil
		}
		return res.Msg.GetWorkers()[0].GetAgents()[0]
	}
	state := func(want pb.AgentStatus_State, installed string) {
		t.Helper()
		wait.For(t, func() bool {
			a := agent()
			return a != nil && a.GetStatus().GetState() == want && a.GetStatus().GetInstalledVersion() == installed &&
				a.GetStatus().GetTargetVersion() == "9.9"
		})
	}
	state(pb.AgentStatus_STATE_NOT_INSTALLED, "")
	set := func(install bool) {
		t.Helper()
		if _, err := machines.SetWorkerAgent(ctx, connect.NewRequest(&pb.SetWorkerAgentRequest{WorkerId: e.Identity.ID,
			Agent: pb.AgentKind_AGENT_KIND_CLAUDE_CODE, Install: install})); err != nil {
			t.Fatal(err)
		}
	}
	set(true)
	state(pb.AgentStatus_STATE_INSTALLED, "9.9")
	set(false)
	state(pb.AgentStatus_STATE_NOT_INSTALLED, "")
}
