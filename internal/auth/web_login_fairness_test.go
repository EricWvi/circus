package auth

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
	"golang.org/x/crypto/bcrypt"
)

// Hold the first four password attempts after acquiring their CPU slots so
// the contention scenario does not depend on the machine's bcrypt speed.
type heldLoginStore struct {
	store.Store
	count   atomic.Int64
	entered chan struct{}
	release <-chan struct{}
}

func (s *heldLoginStore) GetUserByEmail(ctx context.Context, email string) (store.User, error) {
	if strings.HasPrefix(email, "attack-") && s.count.Add(1) <= 4 {
		s.entered <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return store.User{}, ctx.Err()
		}
	}
	return s.Store.GetUserByEmail(ctx, email)
}
func loginTestService(t *testing.T, wrap func(store.Store) store.Store) (*WebService, circusv1connect.AuthServiceClient) {
	t.Helper()
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	var backend store.Store = st
	if wrap != nil {
		backend = wrap(st)
	}
	svc, err := NewWebService(t.Context(), backend, WebConfig{BehindProxy: true, DevInsecure: true, BcryptCost: bcrypt.MinCost, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	svc.setupHash = HashToken("bootstrap")
	_, err = svc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: "bootstrap", Email: "admin@example.com", Password: "password-123"}))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	svc.Register(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	return svc, circusv1connect.NewAuthServiceClient(client, server.URL)
}
func passwordRequest(email, password, ip string) *connect.Request[circusv1.LoginRequest] {
	req := connect.NewRequest(&circusv1.LoginRequest{Email: email, Password: password})
	req.Header().Set("X-Forwarded-For", ip)
	return req
}

func TestLoginRotatingEmailsDoesNotStarveAnotherIP(t *testing.T) {
	entered := make(chan struct{}, 4)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	_, rpc := loginTestService(t, func(st store.Store) store.Store {
		return &heldLoginStore{Store: st, entered: entered, release: release}
	})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	var wg sync.WaitGroup
	results := make(chan connect.Code, 80)
	for range 4 {
		wg.Go(func() {
			for range 20 {
				token, err := NewToken()
				if err != nil {
					results <- connect.CodeInternal
					return
				}
				_, err = rpc.Login(ctx, passwordRequest("attack-"+token+"@example.com", "wrong-password", "192.0.2.1"))
				results <- connect.CodeOf(err)
			}
		})
	}
	defer func() { unblock(); cancel(); wg.Wait() }()
	for range 4 {
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("attack logins did not occupy all slots")
		}
	}
	// Keep all slots occupied long enough for the legitimate HTTP request to
	// queue. The old fail-fast path returns ResourceExhausted during this hold.
	timer := time.AfterFunc(200*time.Millisecond, unblock)
	defer timer.Stop()
	resp, err := rpc.Login(ctx, passwordRequest("admin@example.com", "password-123", "192.0.2.2"))
	if err != nil {
		t.Fatalf("another IP's correct login was denied under contention: %v", err)
	}
	if resp.Msg.User.GetEmail() != "admin@example.com" {
		t.Fatal("wrong signed-in user")
	}
	wg.Wait()
	close(results)
	failures := 0
	for code := range results {
		switch code {
		case connect.CodeUnauthenticated:
			failures++
		case connect.CodeResourceExhausted:
		default:
			t.Fatalf("attack result: %v", code)
		}
	}
	if failures != 50 {
		t.Fatalf("rotating emails reached %d comparisons, want 50", failures)
	}
}

func TestLoginIPFailureLimitIgnoresEmail(t *testing.T) {
	_, rpc := loginTestService(t, nil)
	for i := range 50 {
		_, err := rpc.Login(t.Context(), passwordRequest(fmt.Sprintf("attack-%d@example.com", i), "wrong-password", "192.0.2.1"))
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, err := rpc.Login(t.Context(), passwordRequest("attack-another@example.com", "wrong-password", "192.0.2.1"))
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("rotating emails bypassed the IP limit: %v", err)
	}
	if _, err = rpc.Login(t.Context(), passwordRequest("admin@example.com", "password-123", "192.0.2.2")); err != nil {
		t.Fatal("another IP was blocked", err)
	}
}
