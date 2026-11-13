package controlplane

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
)

type readinessStore struct {
	store.Store
	fail atomic.Bool
}

func (s *readinessStore) GetIdentityByName(ctx context.Context, kind store.PrincipalKind, userID, name string) (store.Identity, error) {
	if s.fail.Load() {
		return store.Identity{}, errors.New("device lookup unavailable")
	}
	return s.Store.GetIdentityByName(ctx, kind, userID, name)
}

func TestReadinessUsesRouterHealth(t *testing.T) {
	ctx := t.Context()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "db"), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	faulty := &readinessStore{Store: db}
	s, err := New(ctx, faulty, Config{Listen: "127.0.0.1:0", DevInsecure: true, TimerPollInterval: 10 * time.Millisecond, LogOutput: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	if err = db.CreateIdentity(ctx, store.Identity{Kind: store.KindDevice, ID: "device", UserID: s.User().ID, Name: "desk", MachineID: "machine", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureLinks(ctx, LinkConfig{}); err != nil {
		t.Fatal(err)
	}
	if err = s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		faulty.fail.Store(false)
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(cleanup)
		_ = db.Close()
	})
	ready := func() (int, string) {
		recorder := httptest.NewRecorder()
		s.serveReady(recorder, httptest.NewRequest("GET", "/readyz", nil))
		return recorder.Code, recorder.Body.String()
	}
	deadline := time.Now().Add(time.Second)
	for {
		status, body := ready()
		if status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became ready: %s", body)
		}
		time.Sleep(time.Millisecond)
	}
	faulty.fail.Store(true)
	_, err = s.router.Submit(ctx, s.User().ID, "desk", &circusv1.DeviceCall{CallId: "call", SessionId: "session", Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: "true"}}})
	if err == nil {
		t.Fatal("device lookup failure was not returned")
	}
	if s.router.Healthy() {
		t.Fatal("store write failure did not mark router unhealthy")
	}
	status, body := ready()
	if status != 503 || !strings.Contains(body, "calls") || strings.Contains(body, "timers") {
		t.Fatalf("readiness ignored router health: status=%d body=%q", status, body)
	}
	// No more call traffic: the router sweep's bounded read must recover /readyz.
	faulty.fail.Store(false)
	deadline = time.Now().Add(3 * time.Second)
	for {
		status, body = ready()
		if status == 200 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("readiness did not recover after the store probe: %s", body)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

type blockingWorkerShutdown struct{ ended chan time.Time }

func (f blockingWorkerShutdown) Shutdown(ctx context.Context) error {
	<-ctx.Done()
	f.ended <- time.Now()
	return ctx.Err()
}

type recordingGatewayShutdown struct{ started chan time.Time }

func (f recordingGatewayShutdown) Shutdown(context.Context) error {
	f.started <- time.Now()
	return nil
}

type recordingRouterClose struct{ closed chan time.Time }

func (f recordingRouterClose) Close(context.Context) error {
	f.closed <- time.Now()
	return nil
}

func TestShutdownSubbudgetsAndOrdering(t *testing.T) {
	const budget = 400 * time.Millisecond
	worker := blockingWorkerShutdown{ended: make(chan time.Time, 1)}
	gateway := recordingGatewayShutdown{started: make(chan time.Time, 1)}
	router := recordingRouterClose{closed: make(chan time.Time, 1)}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	begin := time.Now()
	err := closeCallPlane(ctx, worker, gateway, router, nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v", err)
	}
	workerEnd, gatewayStart, routerClose := <-worker.ended, <-gateway.started, <-router.closed
	if gatewayStart.Before(workerEnd) || routerClose.Before(gatewayStart) {
		t.Fatalf("steps out of order: worker=%v gateway=%v router=%v", workerEnd, gatewayStart, routerClose)
	}
	workerShare := workerEnd.Sub(begin)
	gatewayDelay := gatewayStart.Sub(begin)
	if workerShare < budget/10 || workerShare > budget/2 || gatewayDelay > 3*budget/4 {
		t.Fatalf("worker consumed %v and gateway started %v of %v budget", workerShare, gatewayDelay, budget)
	}
}
