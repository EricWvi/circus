package workerlink_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/sessions"
	"github.com/EricWvi/circus/internal/controlplane/workerlink"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/store/sqlite"
	"github.com/EricWvi/circus/internal/testutil/wait"
	wc "github.com/EricWvi/circus/internal/testutil/workerclient"
	"github.com/EricWvi/circus/internal/wire"
	"github.com/google/uuid"
	"go.uber.org/goleak"
)

type nopBus struct{}

func (nopBus) Durable(string, uint64)                   {}
func (nopBus) Ephemeral(string, *circusv1.SessionEvent) {}

// sessionLink is a worker link with a session service on a SQLite store
// holding session s1 with one pending input, and a worker connection that
// has acquired s1 at epoch. hook, when set, runs before each SessionReply
// send attempt; configure runs on the handler before it serves.
type sessionLink struct {
	st    *sqlite.DB
	svc   *sessions.Service
	c     *wc.Client
	epoch uint64
}

func newSessionLink(t *testing.T, clk clock.Clock, hook func(*circusv1.WorkerDownlink) error, configure func(*workerlink.Handler)) *sessionLink {
	t.Helper()
	baseline := goleak.IgnoreCurrent()
	t.Cleanup(func() { goleak.VerifyNone(t, baseline) })
	ctx := t.Context()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "db"), sqlite.Options{})
	must(t, err)
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	u, err := auth.EnsureUser(ctx, st, "admin", now)
	must(t, err)
	enroll := func(k store.PrincipalKind, name string) auth.Enrollment {
		token, _, err := auth.CreateEnrollToken(ctx, st, u.ID, k, time.Hour, now)
		must(t, err)
		e, err := auth.Enroll(ctx, st, auth.EnrollRequest{Token: token, Kind: k, Name: name, MachineID: "m-" + name}, now)
		must(t, err)
		return e
	}
	device, worker := enroll(store.KindDevice, "desk"), enroll(store.KindWorker, "w")
	at := clk.Now().UnixMilli()
	_, err = st.CreateProject(ctx, store.Project{ID: "p", UserID: u.ID, Name: "p", CreatedAt: at, UpdatedAt: at}, "")
	must(t, err)
	must(t, st.CreateSession(ctx, store.Session{ID: "s1", UserID: u.ID, ProjectID: "p", ClaudeSessionID: "c", DefaultDeviceID: device.Identity.ID, CreatedAt: at}, nil, 10))
	_, err = st.SubmitInput(ctx, store.Input{InputID: "i1", SessionID: "s1", ClientMsgID: "m", Kind: store.InputUser, Text: "hi", Priority: store.InputNormal, CreatedAt: at})
	must(t, err)

	quiet := slog.New(slog.DiscardHandler)
	svc, err := sessions.New(sessions.Config{Store: st, Bus: nopBus{}, InstanceID: "test", Logger: quiet, MaxMessageBytes: 1 << 20, Clock: clk})
	must(t, err)
	must(t, svc.Start(ctx))
	h, err := workerlink.New(workerlink.Config{Auth: auth.NewAuthenticator(st, auth.Options{}), Router: &service{started: make(chan string, 1)},
		InstanceID: "test", Wire: wire.Options{MaxMessageBytes: 1 << 20}, Sessions: svc, Logger: quiet})
	must(t, err)
	if hook != nil {
		workerlink.SetSessionSend(h, hook)
	}
	if configure != nil {
		configure(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), wait.Timeout)
		defer cancel()
		must(t, h.Shutdown(ctx))
		must(t, svc.Close(ctx))
	})
	c, err := wc.Dial(wait.Ctx(t), wc.Options{URL: srv.URL, Token: worker.Token, PrincipalID: worker.Identity.ID, BootID: uuid.Must(uuid.NewV7()).String()})
	must(t, err)
	t.Cleanup(func() { _ = c.Close() })
	ev, err := c.Attach(wait.Ctx(t), &circusv1.Attach{MaxSessions: 1})
	must(t, err)
	e, err := ev.Next(wait.Ctx(t))
	must(t, err)
	if e.GetOffer().GetSessionId() != "s1" {
		t.Fatalf("hint %v", e)
	}
	return &sessionLink{st: st, svc: svc, c: c}
}

// acquire takes s1's lease.
func (l *sessionLink) acquire(t *testing.T) {
	t.Helper()
	lease, err := l.c.Session(wait.Ctx(t), wc.Acquire("s1", true))
	must(t, err)
	if lease.GetLease() == nil {
		t.Fatalf("acquire %v", lease)
	}
	l.epoch = lease.GetLease().GetEpoch()
}

// TestBusySendPoolSessionReplies: while the shared send pool refuses some
// session replies, the receive loop keeps serving the connection (inline
// replies wait in the connection's reply queue) and stuck read replies do
// not keep the global read permits.
func TestBusySendPoolSessionReplies(t *testing.T) {
	ctx := t.Context()
	// Inputs and state replies are refused as if the pool were full until
	// release closes.
	release := make(chan struct{})
	var once sync.Once
	var stateAttempts atomic.Int32
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	l := newSessionLink(t, clock.Real{}, func(m *circusv1.WorkerDownlink) error {
		r := m.GetSessionReply()
		if r.GetInputs() == nil && r.GetState() == nil {
			return nil
		}
		if r.GetState() != nil {
			stateAttempts.Add(1)
		}
		select {
		case <-release:
			return nil
		default:
			return wire.ErrBusy
		}
	}, nil)
	l.acquire(t)
	st, c, epoch := l.st, l.c, l.epoch
	quick, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// An inline reply stuck on the pool does not stop the receive loop: a
	// later write commits (its reply waits behind the first, in order).
	take, err := c.Send(quick, wc.TakeInputs("s1", epoch))
	must(t, err)
	appended, err := c.Send(quick, wc.Append("s1", epoch, "", 1, &circusv1.TranscriptEntry{Json: `{}`}))
	must(t, err)
	wait.For(t, func() bool {
		s, err := st.GetSession(ctx, "s1", time.Now().UnixMilli())
		return err == nil && s.TranscriptBytes > 0
	})
	// Two read replies stuck on the pool leave the read permits free.
	var stuck []wc.Reply
	for range sessions.ReadConcurrency {
		s, err := c.Send(quick, wc.State("s1", epoch))
		must(t, err)
		stuck = append(stuck, s)
	}
	// Both have been built and refused by the pool at least once.
	wait.For(t, func() bool { return stateAttempts.Load() >= 2*int32(sessions.ReadConcurrency) })
	r, err := c.Session(quick, wc.Load("s1", epoch, "", 0, 1024))
	must(t, err)
	if r.GetTranscriptPage() == nil {
		t.Fatalf("load while reads were stuck %v", r)
	}
	once.Do(func() { close(release) })
	r, err = take.Wait(wait.Ctx(t))
	must(t, err)
	if len(r.GetInputs().GetInputs()) != 1 {
		t.Fatalf("take %v", r)
	}
	r, err = appended.Wait(wait.Ctx(t))
	must(t, err)
	if r.GetOk() == nil {
		t.Fatalf("append %v", r)
	}
	for _, s := range stuck {
		r, err := s.Wait(wait.Ctx(t))
		must(t, err)
		if r.GetState() == nil {
			t.Fatalf("state %v", r)
		}
	}
	if err := c.Err(); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

// TestInlineReplyQueueBound: the connection's queue of inline replies is
// bounded; once full, the receive loop waits, so a later write commits only
// when the stuck replies leave the queue.
func TestInlineReplyQueueBound(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	l := newSessionLink(t, clock.Real{}, func(m *circusv1.WorkerDownlink) error {
		if m.GetSessionReply().GetInputs() == nil {
			return nil
		}
		select {
		case <-release:
			return nil
		default:
			return wire.ErrBusy
		}
	}, func(h *workerlink.Handler) { workerlink.SetMaxInlineReplyBytes(h, 1) })
	l.acquire(t)
	ctx := wait.Ctx(t)
	var takes []wc.Reply
	for range 3 {
		r, err := l.c.Send(ctx, wc.TakeInputs("s1", l.epoch))
		must(t, err)
		takes = append(takes, r)
	}
	appended, err := l.c.Send(ctx, wc.Append("s1", l.epoch, "", 1, &circusv1.TranscriptEntry{Json: `{}`}))
	must(t, err)
	committed := func() bool {
		s, err := l.st.GetSession(ctx, "s1", time.Now().UnixMilli())
		return err == nil && s.TranscriptBytes > 0
	}
	time.Sleep(300 * time.Millisecond)
	if committed() {
		t.Fatal("the receive loop went on past a full reply queue")
	}
	once.Do(func() { close(release) })
	wait.For(t, committed)
	for _, r := range append(takes, appended) {
		_, err := r.Wait(ctx)
		must(t, err)
	}
}

// TestSmallReadReplyNeedsNoPushPermit: a small read reply the pool
// refuses is retried without a push permit, so it is sent once the pool
// frees even while every push permit is held (by large call updates).
func TestSmallReadReplyNeedsNoPushPermit(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	var attempts atomic.Int32
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var h *workerlink.Handler
	l := newSessionLink(t, clock.Real{}, func(m *circusv1.WorkerDownlink) error {
		if m.GetSessionReply().GetState() == nil {
			return nil
		}
		attempts.Add(1)
		select {
		case <-release:
			return nil
		default:
			return wire.ErrBusy
		}
	}, func(x *workerlink.Handler) { h = x })
	l.acquire(t)
	permits := workerlink.HoldPushPermits(h)
	defer permits()
	r, err := l.c.Send(wait.Ctx(t), wc.State("s1", l.epoch))
	must(t, err)
	wait.For(t, func() bool { return attempts.Load() > 0 })
	once.Do(func() { close(release) })
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	reply, err := r.Wait(ctx)
	must(t, err)
	if reply.GetState() == nil {
		t.Fatalf("state %v", reply)
	}
}

// TestLargeReadReplyWithoutPushPermit: a large read reply refused by the
// pool while every push permit is held gives its read permit back and waits
// holding nothing, so other reads go on; it is rebuilt and sent once a push
// permit frees.
func TestLargeReadReplyWithoutPushPermit(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	var attempts atomic.Int32
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var h *workerlink.Handler
	l := newSessionLink(t, clock.Real{}, func(m *circusv1.WorkerDownlink) error {
		if m.GetSessionReply().GetTranscriptPage() == nil {
			return nil
		}
		attempts.Add(1)
		select {
		case <-release:
			return nil
		default:
			return wire.ErrBusy
		}
	}, func(x *workerlink.Handler) { h = x })
	l.acquire(t)
	ctx := wait.Ctx(t)
	big := strings.Repeat("x", 300<<10) // a page over the small-update size
	r, err := l.c.Session(ctx, wc.Append("s1", l.epoch, "", 1, &circusv1.TranscriptEntry{Json: big}))
	must(t, err)
	if r.GetOk() == nil {
		t.Fatalf("append %v", r)
	}
	permits := workerlink.HoldPushPermits(h)
	var loads []wc.Reply
	for range sessions.ReadConcurrency {
		r, err := l.c.Send(ctx, wc.Load("s1", l.epoch, "", 0, 1<<20))
		must(t, err)
		loads = append(loads, r)
	}
	wait.For(t, func() bool { return attempts.Load() >= int32(sessions.ReadConcurrency) })
	quick, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	st, err := l.c.Session(quick, wc.State("s1", l.epoch))
	must(t, err)
	if st.GetState() == nil {
		t.Fatalf("state while large replies waited %v", st)
	}
	once.Do(func() { close(release) })
	permits()
	for _, r := range loads {
		p, err := r.Wait(ctx)
		must(t, err)
		if len(p.GetTranscriptPage().GetEntries()) != 1 {
			t.Fatalf("page %v", p)
		}
	}
}

// TestUnwatchBeforeLeaseDelivery: a worker unwatches an AcquireLease whose
// slot was already released while its reply waits for the send pool; the
// reply then gets out, but the lease stays abandoned (not extended).
func TestUnwatchBeforeLeaseDelivery(t *testing.T) {
	clk := clock.New()
	release := make(chan struct{})
	var once sync.Once
	var attempts atomic.Int32
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	l := newSessionLink(t, clk, func(m *circusv1.WorkerDownlink) error {
		if m.GetSessionReply().GetLease() == nil {
			return nil
		}
		attempts.Add(1)
		select {
		case <-release:
			return nil
		default:
			return wire.ErrBusy
		}
	}, nil)
	ctx := wait.Ctx(t)
	r, err := l.c.Send(ctx, wc.Acquire("s1", true))
	must(t, err)
	wait.For(t, func() bool { return attempts.Load() > 0 }) // built, slot released, retrying
	must(t, r.Unwatch(ctx))
	time.Sleep(50 * time.Millisecond) // the Unwatch is processed
	before, err := l.st.GetLease(ctx, "s1")
	must(t, err)
	clk.Advance(time.Second) // a delivery that extends would now move the expiry
	n := attempts.Load()
	once.Do(func() { close(release) })
	wait.For(t, func() bool { return attempts.Load() > n }) // the reply got out
	time.Sleep(100 * time.Millisecond)
	after, err := l.st.GetLease(ctx, "s1")
	must(t, err)
	if after.ExpiresAt != before.ExpiresAt {
		t.Fatal("an unwatched AcquireLease's lease was extended when its reply got out")
	}
}

// TestRebuildDeadline: a large read reply that can get neither the send
// pool nor a push permit gives up after sendBusyLimit and closes the link.
func TestRebuildDeadline(t *testing.T) {
	linkClk := clock.New()
	var attempts atomic.Int32
	var h *workerlink.Handler
	l := newSessionLink(t, clock.Real{}, func(m *circusv1.WorkerDownlink) error {
		if m.GetSessionReply().GetTranscriptPage() == nil {
			return nil
		}
		attempts.Add(1)
		return wire.ErrBusy
	}, func(x *workerlink.Handler) { h = x; workerlink.SetClock(x, linkClk) })
	l.acquire(t)
	ctx := wait.Ctx(t)
	r, err := l.c.Session(ctx, wc.Append("s1", l.epoch, "", 1, &circusv1.TranscriptEntry{Json: strings.Repeat("x", 300<<10)}))
	must(t, err)
	if r.GetOk() == nil {
		t.Fatalf("append %v", r)
	}
	permits := workerlink.HoldPushPermits(h)
	defer permits()
	_, err = l.c.Send(ctx, wc.Load("s1", l.epoch, "", 0, 1<<20))
	must(t, err)
	wait.For(t, func() bool { return attempts.Load() > 0 })
	time.Sleep(50 * time.Millisecond) // waiting for a push permit
	linkClk.Advance(11 * time.Second)
	select {
	case <-l.c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the link stayed open past the send deadline")
	}
}

// bigPage appends one transcript entry over the small-update size to s1, so
// a Load of it is a large read reply.
func (l *sessionLink) bigPage(t *testing.T) {
	t.Helper()
	r, err := l.c.Session(wait.Ctx(t), wc.Append("s1", l.epoch, "", 1, &circusv1.TranscriptEntry{Json: strings.Repeat("x", 300<<10)}))
	must(t, err)
	if r.GetOk() == nil {
		t.Fatalf("append %v", r)
	}
}

// holdPushPermits takes every push permit one by one; release gives back
// those still held.
func holdPushPermits(h *workerlink.Handler) (one func(), all func()) {
	var mu sync.Mutex
	var held []func()
	for range workerlink.PushPermits {
		held = append(held, workerlink.HoldPushPermit(h))
	}
	pop := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if len(held) == 0 {
			return false
		}
		held[len(held)-1]()
		held = held[:len(held)-1]
		return true
	}
	return func() { pop() }, func() {
		for pop() {
		}
	}
}

// TestRebuiltReplyKeepsPermit: a large read reply dropped while every push
// permit was held is rebuilt under the one permit it then takes, and keeps
// that permit while the pool refuses the rebuilt reply again: no other
// permit is taken (none is free) and none is leaked once it is sent.
func TestRebuiltReplyKeepsPermit(t *testing.T) {
	var h *workerlink.Handler
	var attempts atomic.Int32
	inUse := make(chan int, 8)
	l := newSessionLink(t, clock.Real{}, func(m *circusv1.WorkerDownlink) error {
		if m.GetSessionReply().GetTranscriptPage() == nil {
			return nil
		}
		n := attempts.Add(1)
		if n > 1 {
			inUse <- workerlink.PushPermitsInUse(h)
		}
		if n < 3 {
			return wire.ErrBusy
		}
		return nil
	}, func(x *workerlink.Handler) { h = x })
	l.acquire(t)
	l.bigPage(t)
	one, all := holdPushPermits(h)
	defer all()
	ctx := wait.Ctx(t)
	r, err := l.c.Send(ctx, wc.Load("s1", l.epoch, "", 0, 1<<20))
	must(t, err)
	wait.For(t, func() bool { return attempts.Load() == 1 })
	one() // the dropped reply takes this permit and is rebuilt
	quick, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	p, err := r.Wait(quick)
	must(t, err)
	if len(p.GetTranscriptPage().GetEntries()) != 1 {
		t.Fatalf("page %v", p)
	}
	for i := range 2 {
		if n := <-inUse; n != workerlink.PushPermits {
			t.Fatalf("rebuilt attempt %d with %d push permits in use, want all %d (the kept one)", i+2, n, workerlink.PushPermits)
		}
	}
	wait.For(t, func() bool { return workerlink.PushPermitsInUse(h) == workerlink.PushPermits-1 })
	all()
	if n := workerlink.PushPermitsInUse(h); n != 0 {
		t.Fatalf("%d push permits leaked", n)
	}
}

// TestRebuildReadWaitBounded: a dropped reply that got its push permit but
// cannot get a read permit closes the link at the send deadline instead of
// holding the push permit, and gives the permit back.
func TestRebuildReadWaitBounded(t *testing.T) {
	linkClk := clock.New()
	var attempts atomic.Int32
	var h *workerlink.Handler
	l := newSessionLink(t, clock.Real{}, func(m *circusv1.WorkerDownlink) error {
		if m.GetSessionReply().GetTranscriptPage() == nil {
			return nil
		}
		attempts.Add(1)
		return wire.ErrBusy
	}, func(x *workerlink.Handler) { h = x; workerlink.SetClock(x, linkClk) })
	l.acquire(t)
	l.bigPage(t)
	one, all := holdPushPermits(h)
	defer all()
	ctx := wait.Ctx(t)
	_, err := l.c.Send(ctx, wc.Load("s1", l.epoch, "", 0, 1<<20))
	must(t, err)
	wait.For(t, func() bool { return attempts.Load() == 1 })
	// Hold every read permit, then free a push permit for the rebuild.
	block := make(chan struct{})
	var readers sync.WaitGroup
	defer func() { close(block); readers.Wait() }()
	var holding atomic.Int32
	for range sessions.ReadConcurrency {
		readers.Go(func() { l.svc.Read(context.Background(), func() { holding.Add(1); <-block }) })
	}
	wait.For(t, func() bool { return holding.Load() == int32(sessions.ReadConcurrency) })
	one()
	wait.For(t, func() bool { return workerlink.PushPermitsInUse(h) == workerlink.PushPermits })
	time.Sleep(50 * time.Millisecond) // waiting for a read permit
	linkClk.Advance(11 * time.Second)
	select {
	case <-l.c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the link stayed open past the send deadline while waiting for a read permit")
	}
	wait.For(t, func() bool { return workerlink.PushPermitsInUse(h) == workerlink.PushPermits-1 })
	if attempts.Load() != 1 {
		t.Fatalf("%d attempts, want 1", attempts.Load())
	}
}

// TestRebuiltSendSharesDeadline: the rebuilt reply's busy retries end at the
// deadline that started with the first attempt, not 10 seconds after the
// rebuild.
func TestRebuiltSendSharesDeadline(t *testing.T) {
	linkClk := clock.New()
	var attempts atomic.Int32
	var h *workerlink.Handler
	l := newSessionLink(t, clock.Real{}, func(m *circusv1.WorkerDownlink) error {
		if m.GetSessionReply().GetTranscriptPage() == nil {
			return nil
		}
		attempts.Add(1)
		return wire.ErrBusy
	}, func(x *workerlink.Handler) { h = x; workerlink.SetClock(x, linkClk) })
	l.acquire(t)
	l.bigPage(t)
	one, all := holdPushPermits(h)
	defer all()
	ctx := wait.Ctx(t)
	_, err := l.c.Send(ctx, wc.Load("s1", l.epoch, "", 0, 1<<20))
	must(t, err)
	wait.For(t, func() bool { return attempts.Load() == 1 })
	time.Sleep(50 * time.Millisecond) // waiting for a push permit
	linkClk.Advance(6 * time.Second)
	one()
	wait.For(t, func() bool { return attempts.Load() == 3 }) // rebuilt, refused twice, backing off
	time.Sleep(50 * time.Millisecond)
	linkClk.Advance(5 * time.Second) // 11 s after the first attempt, 5 s after the rebuild
	select {
	case <-l.c.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the rebuilt reply got a fresh send deadline")
	}
	wait.For(t, func() bool { return workerlink.PushPermitsInUse(h) == workerlink.PushPermits-1 })
}

// TestRebuildDeadlineStartsAtDrop: a read reply that waited longer than the
// send deadline for its first read permit, and is then dropped because the
// pool and every push permit are busy, still waits for a push permit and
// gets sent: the deadline starts at the drop.
func TestRebuildDeadlineStartsAtDrop(t *testing.T) {
	linkClk := clock.New()
	var attempts atomic.Int32
	var h *workerlink.Handler
	l := newSessionLink(t, clock.Real{}, func(m *circusv1.WorkerDownlink) error {
		if m.GetSessionReply().GetTranscriptPage() == nil {
			return nil
		}
		if attempts.Add(1) == 1 {
			return wire.ErrBusy
		}
		return nil
	}, func(x *workerlink.Handler) { h = x; workerlink.SetClock(x, linkClk) })
	l.acquire(t)
	l.bigPage(t)
	// Hold every read permit, so the read waits for one.
	block := make(chan struct{})
	var readers sync.WaitGroup
	var holding atomic.Int32
	for range sessions.ReadConcurrency {
		readers.Go(func() { l.svc.Read(context.Background(), func() { holding.Add(1); <-block }) })
	}
	wait.For(t, func() bool { return holding.Load() == int32(sessions.ReadConcurrency) })
	unblocked := false
	unblock := func() {
		if !unblocked {
			unblocked = true
			close(block)
			readers.Wait()
		}
	}
	defer unblock()
	one, all := holdPushPermits(h)
	defer all()
	ctx := wait.Ctx(t)
	r, err := l.c.Send(ctx, wc.Load("s1", l.epoch, "", 0, 1<<20))
	must(t, err)
	time.Sleep(50 * time.Millisecond) // waiting for a read permit
	linkClk.Advance(11 * time.Second)
	unblock()
	wait.For(t, func() bool { return attempts.Load() == 1 })
	time.Sleep(50 * time.Millisecond) // dropped, waiting for a push permit
	one()
	quick, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	p, err := r.Wait(quick)
	must(t, err)
	if len(p.GetTranscriptPage().GetEntries()) != 1 {
		t.Fatalf("page %v", p)
	}
}
