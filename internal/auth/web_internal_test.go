package auth

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/store/sqlite/sqlitetest"
	"golang.org/x/crypto/bcrypt"
)

func TestWebTokenHashAndProductionDefaults(t *testing.T) {
	st, err := sqlite.Open(t.Context(), sqlitetest.Path(t), sqlite.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc, err := NewWebService(t.Context(), st, WebConfig{SetupURL: "https://circus.example.com", Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	cost, err := bcrypt.Cost(svc.dummy)
	if err != nil || cost != 12 {
		t.Fatalf("default bcrypt cost: %d %v", cost, err)
	}
	if len(svc.setupHash) != sha256.Size {
		t.Fatal("setup token must be retained as a SHA-256 hash")
	}
	// Both sides of setup's constant-time comparison always have 32 bytes,
	// regardless of the candidate token's length. Wall-clock timing assertions
	// would measure scheduler noise rather than the comparison primitive.
	candidate := "setup-token-for-test"
	svc.setupHash = HashToken(candidate)
	for _, value := range []string{"", "short", candidate + "extra"} {
		if sameHash(HashToken(value), svc.setupHash) {
			t.Fatal("invalid token matched")
		}
	}
	if !sameHash(HashToken(candidate), svc.setupHash) {
		t.Fatal("valid token did not match")
	}
	// Saturating anonymous login comparisons must not rate-limit setup.
	for range cap(svc.bcryptSlots) {
		svc.bcryptSlots <- struct{}{}
	}
	defer func() {
		for range cap(svc.bcryptSlots) {
			<-svc.bcryptSlots
		}
	}()
	resp, err := svc.Setup(t.Context(), connect.NewRequest(&circusv1.SetupRequest{Token: candidate, Email: "a@b", Password: "12345678"}))
	if err != nil {
		t.Fatal(err)
	}
	cookie := svc.sessionCookie("token")
	if !cookie.Secure || !cookie.HttpOnly {
		t.Fatal("production cookie flags")
	}
	u, err := st.GetUser(t.Context(), resp.Msg.User.UserId)
	if err != nil {
		t.Fatal(err)
	}
	cost, err = bcrypt.Cost([]byte(u.PasswordHash))
	if err != nil || cost != 12 {
		t.Fatalf("password bcrypt cost: %d %v", cost, err)
	}
	if svc.setupHash != nil {
		t.Fatal("setup hash not consumed")
	}
}
func TestLoginFailureLimiterPairs(t *testing.T) {
	var l failureLimiter
	now := time.Now()
	key := "alice\x00127.0.0.1"
	for range 5 {
		attempt := l.reserve(key, now)
		if attempt == nil {
			t.Fatal("limited before five failures")
		}
		attempt.finish(true, now)
	}
	if l.reserve(key, now) != nil {
		t.Fatal("missing backoff")
	}
	for _, other := range []string{"alice\x00127.0.0.2", "bob\x00127.0.0.1"} {
		a := l.reserve(other, now)
		if a == nil {
			t.Fatal("unrelated pair was limited")
		}
		a.finish(false, now)
	}
	a := l.reserve(key, now.Add(3*time.Minute))
	if a == nil {
		t.Fatal("backoff never ended")
	}
	a.finish(false, now.Add(3*time.Minute))
	if l.reserve(key, now.Add(3*time.Minute)) == nil {
		t.Fatal("success did not release its reservation")
	}
}

func TestLoginLimiterConcurrentReservations(t *testing.T) {
	var l failureLimiter
	now := time.Now()
	var wg sync.WaitGroup
	attempts := make(chan *loginAttempt, 100)
	start := make(chan struct{})
	for range 100 {
		wg.Go(func() {
			<-start
			if a := l.reserve("same-pair", now); a != nil {
				attempts <- a
			}
		})
	}
	close(start)
	wg.Wait()
	close(attempts)
	var accepted []*loginAttempt
	for a := range attempts {
		accepted = append(accepted, a)
	}
	if len(accepted) != 5 {
		t.Fatalf("concurrent accepted attempts = %d, want 5", len(accepted))
	}
	accepted[0].finish(false, now)
	if a := l.reserve("same-pair", now); a == nil {
		t.Fatal("successful attempt did not release its reservation")
	} else {
		a.finish(false, now)
	}
	for _, a := range accepted[1:] {
		a.finish(true, now)
	}
}

func TestLoginLimiterEvictsOldest(t *testing.T) {
	var l failureLimiter
	now := time.Now()
	oldest := l.reserve("oldest", now)
	for i := range maxFailureKeys {
		if l.reserve(fmt.Sprint(i), now) == nil {
			t.Fatal("capacity rejected a new key")
		}
	}
	if len(l.entries) != maxFailureKeys || l.entries["oldest"] != nil {
		t.Fatal("oldest key was not evicted")
	}
	// Finishing evicted work must not mutate a replacement with the same key.
	replacement := l.reserve("oldest", now)
	oldest.finish(true, now)
	if replacement.entry.pending != 1 || replacement.entry.failures != 0 {
		t.Fatal("evicted reservation corrupted replacement")
	}
	replacement.finish(false, now)
}

func TestWebBcryptCapacity(t *testing.T) {
	s := &WebService{now: time.Now, bcryptSlots: make(chan struct{}, 2)}
	first, err := s.acquireBcrypt(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.acquireBcrypt(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	defer second()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = s.acquireBcrypt(ctx); connect.CodeOf(err) != connect.CodeCanceled {
		t.Fatal("canceled wait", err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err = s.Login(ctx, connect.NewRequest(&circusv1.LoginRequest{Email: "a@b", Password: "password-123"}))
	if connect.CodeOf(err) != connect.CodeDeadlineExceeded {
		t.Fatal("request deadline was ignored", err)
	}
	start := time.Now()
	_, err = s.Login(t.Context(), connect.NewRequest(&circusv1.LoginRequest{Email: "a@b", Password: "password-123"}))
	if connect.CodeOf(err) != connect.CodeResourceExhausted || time.Since(start) < bcryptWaitTimeout {
		t.Fatal("queue did not wait until its timeout", err)
	}
	if a := s.loginLimit.reserve("a@b\x00", s.now()); a == nil {
		t.Fatal("queue timeout consumed a pair failure reservation")
	} else {
		a.finish(false, s.now())
	}
	if a := s.loginIPLimit.reserve("", s.now()); a == nil {
		t.Fatal("queue timeout consumed an IP failure reservation")
	} else {
		a.finish(false, s.now())
	}
}

func TestClientIP(t *testing.T) {
	for _, tc := range []struct {
		proxy      bool
		values     []string
		peer, want string
	}{
		{true, []string{"198.51.100.1, 192.0.2.1"}, "127.0.0.1:123", "192.0.2.1"},
		{true, []string{"198.51.100.1", "192.0.2.2"}, "127.0.0.1:123", "192.0.2.2"},
		{true, []string{"198.51.100.1, invalid"}, "127.0.0.1:123", "127.0.0.1"},
		{true, nil, "[::1]:123", "::1"},
		{true, []string{"::ffff:192.0.2.1"}, "127.0.0.1:123", "192.0.2.1"},
		{false, []string{"192.0.2.1"}, "127.0.0.1:123", "127.0.0.1"},
	} {
		s := &WebService{behindProxy: tc.proxy, logger: slog.New(slog.DiscardHandler)}
		header := http.Header{}
		for _, v := range tc.values {
			header.Add("X-Forwarded-For", v)
		}
		if got := s.clientIP(header, tc.peer); got != tc.want {
			t.Fatalf("proxy=%v values=%v: got %q want %q", tc.proxy, tc.values, got, tc.want)
		}
	}
}

func TestLoginIPLimiterAtomicWindow(t *testing.T) {
	l := failureLimiter{maxAttempts: loginIPFailureLimit, backoff: 10 * time.Minute}
	now := time.Now()
	var wg sync.WaitGroup
	accepted := make(chan *loginAttempt, 100)
	start := make(chan struct{})
	for range 100 {
		wg.Go(func() {
			<-start
			if a := l.reserve("192.0.2.1", now); a != nil {
				accepted <- a
			}
		})
	}
	close(start)
	wg.Wait()
	close(accepted)
	count := 0
	for a := range accepted {
		count++
		a.finish(true, now)
	}
	if count != 50 {
		t.Fatalf("concurrent IP reservations = %d, want 50", count)
	}
	if l.reserve("192.0.2.1", now.Add(5*time.Minute)) != nil {
		t.Fatal("IP window ended too early")
	}
	if a := l.reserve("192.0.2.1", now.Add(10*time.Minute)); a == nil {
		t.Fatal("IP window never ended")
	} else {
		a.finish(false, now.Add(10*time.Minute))
	}
}
