package auth_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
)

// armedClock reports every recheck alarm (a duration up to one interval)
// so a test advances the fake clock only while Watch waits on it.
type armedClock struct {
	*clock.Fake
	interval time.Duration
	armed    chan struct{}
}

func (c *armedClock) After(d time.Duration) <-chan time.Time {
	ch := c.Fake.After(d)
	if d <= c.interval {
		c.armed <- struct{}{}
	}
	return ch
}

type answer struct {
	ok  bool
	err error
}

type closed struct {
	definitive bool
	at         time.Time
	err        error
}

// watch runs p.Watch with a valid func the test answers one check at a time.
type watch struct {
	clk     *armedClock
	start   time.Time
	checks  chan context.Context
	answers chan answer
	closed  chan closed
	done    chan struct{}
	cancel  context.CancelFunc
}

func startWatch(t *testing.T, p auth.RecheckPolicy) *watch {
	t.Helper()
	p, err := p.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	w := &watch{clk: &armedClock{Fake: clock.New(), interval: p.Interval, armed: make(chan struct{}, 1)}, checks: make(chan context.Context), answers: make(chan answer), closed: make(chan closed, 1), done: make(chan struct{})}
	w.start = w.clk.Now()
	ctx, cancel := context.WithCancel(t.Context())
	w.cancel = cancel
	go func() {
		defer close(w.done)
		p.Watch(ctx, w.clk, func(ctx context.Context) (bool, error) {
			w.checks <- ctx
			a := <-w.answers
			return a.ok, a.err
		}, func(definitive bool, lastValid time.Time, err error) {
			w.closed <- closed{definitive, w.clk.Now(), err}
		})
	}()
	t.Cleanup(func() { cancel(); <-w.done })
	return w
}

// next advances to the next check and returns its context.
func (w *watch) next(t *testing.T, step time.Duration) context.Context {
	t.Helper()
	wait.Recv(t, w.clk.armed)
	w.clk.Advance(step)
	return wait.Recv(t, w.checks)
}

func (w *watch) isClosed() (closed, bool) {
	select {
	case c := <-w.closed:
		return c, true
	default:
		return closed{}, false
	}
}

func TestWatchClosesAtOnceOnDefinitiveRevoke(t *testing.T) {
	for name, a := range map[string]answer{"invalid": {false, nil}, "revoked": {false, auth.ErrRevoked}} {
		t.Run(name, func(t *testing.T) {
			w := startWatch(t, auth.RecheckPolicy{})
			w.next(t, time.Second) // the random phase is below one interval
			w.answers <- answer{true, nil}
			w.next(t, time.Second)
			w.answers <- a
			c := wait.Recv(t, w.closed)
			if !c.definitive || c.at.Sub(w.start) > 2*time.Second {
				t.Fatalf("closed %+v after %s", c, c.at.Sub(w.start))
			}
			wait.Recv(t, w.done)
		})
	}
}

func TestWatchKeepsLinkUntilMaxStale(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "store error", true: "check timeout"}[timeout], func(t *testing.T) {
			p := auth.RecheckPolicy{Interval: time.Second, Timeout: 5 * time.Second, MaxStale: 30 * time.Second}
			w := startWatch(t, p)
			last := w.start
			for {
				ctx := w.next(t, time.Second)
				if timeout {
					w.clk.Advance(p.Timeout)
					<-ctx.Done()
					// An answer after the timeout is no answer, even "invalid".
					w.answers <- answer{false, nil}
				} else {
					w.answers <- answer{false, errors.New("store busy")}
				}
				// Each check either arms the next alarm or closes.
				select {
				case c := <-w.closed:
					if c.definitive || c.at.Sub(w.start) < p.MaxStale || c.at.Sub(last) > p.Interval+p.Timeout {
						t.Fatalf("closed %+v after %s", c, c.at.Sub(w.start))
					}
					return
				case <-w.clk.armed:
					w.clk.armed <- struct{}{} // hand it back to next
				}
				if c, ok := w.isClosed(); ok {
					t.Fatalf("closed early %+v", c)
				}
				last = w.clk.Now()
			}
		})
	}
}

func TestWatchValidCheckResetsStaleness(t *testing.T) {
	p := auth.RecheckPolicy{Interval: time.Second, MaxStale: 3 * time.Second}
	w := startWatch(t, p)
	for i := range 10 {
		w.next(t, time.Second)
		// Two failures in three seconds never reach MaxStale.
		w.answers <- answer{i%3 == 2, map[bool]error{false: errors.New("busy"), true: nil}[i%3 == 2]}
	}
	if c, ok := w.isClosed(); ok {
		t.Fatalf("closed %+v", c)
	}
	w.cancel()
	wait.Recv(t, w.done)
	if c, ok := w.isClosed(); ok {
		t.Fatalf("cancellation reported a close: %+v", c)
	}
}

func TestRecheckPolicyNormalize(t *testing.T) {
	p, err := auth.RecheckPolicy{}.Normalize()
	if err != nil || p != (auth.RecheckPolicy{Interval: time.Second, Timeout: 5 * time.Second, MaxStale: 30 * time.Second}) {
		t.Fatal(p, err)
	}
	if _, err := (auth.RecheckPolicy{Timeout: -1}).Normalize(); err == nil {
		t.Fatal("negative timeout accepted")
	}
}

type stubAuth struct {
	p   auth.Principal
	err error
}

func (a stubAuth) Authenticate(context.Context, *http.Request) (auth.Principal, error) {
	return a.p, a.err
}
func (stubAuth) Valid(context.Context, string) (bool, error) { return true, nil }

func TestAdmitStatusMapping(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := auth.Principal{Kind: store.KindWorker, ID: "w"}
	for _, tc := range []struct {
		a      stubAuth
		status int
		body   string
	}{
		{stubAuth{err: auth.ErrUnauthenticated}, 401, "unauthenticated"},
		{stubAuth{err: auth.ErrRevoked}, 403, "revoked"},
		{stubAuth{err: errors.New("store down")}, 503, "unavailable"},
		{stubAuth{p: worker}, 403, "use a daemon credential"},
		{stubAuth{p: auth.Principal{Kind: store.KindDevice, ID: "d"}}, 200, ""},
	} {
		rec := httptest.NewRecorder()
		p, ok := auth.Admit(rec, httptest.NewRequest("GET", "/v1/link/daemon", nil), tc.a, logger, store.KindDevice)
		if tc.status == 200 {
			if !ok || p.ID != "d" || rec.Code != 200 {
				t.Fatalf("device refused: %d", rec.Code)
			}
			continue
		}
		if ok || rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.body) {
			t.Fatalf("%+v: %d %q", tc.a, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("WWW-Authenticate"); (tc.status == 401) != (got == "Bearer") {
			t.Fatalf("%d: WWW-Authenticate %q", tc.status, got)
		}
	}
}
