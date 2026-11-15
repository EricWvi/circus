package calls

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/controlplane/gateway"
	"github.com/EricWvi/circus/internal/store"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
)

func TestQueryAnswerTransferKeepsCharge(t *testing.T) {
	r := &router{config: Config{
		Clock: clock.New(), MaxSubscriptions: 2, MaxUpdatesPerSubscription: 2,
		MaxMessageBytes: 64 << 10, MaxSubscriptionBytes: 1 << 20, MaxBufferedUpdateBytes: 1 << 20,
	}, subs: make(map[*Subscription]struct{})}
	u, c, err := r.own(dispatched("call"))
	if err != nil || !r.acquireCharge(c) {
		t.Fatalf("own reply: %v", err)
	}
	// A transfer must keep existing ownership, even with a budget tightened
	// below current usage. Releasing first would require fresh admission.
	r.config.MaxBufferedUpdateBytes = c.cost - 1
	r.mu.Lock()
	s, err := r.onceAnswer(t.Context(), "call", u, c)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if c.refs != 1 || r.bufferedBytes != c.cost {
		t.Fatalf("transfer dropped ownership: refs=%d buffered=%d", c.refs, r.bufferedBytes)
	}
	got, err := s.Next(t.Context())
	if err != nil || got != u {
		t.Fatalf("transfer replaced or rejected the answer: %v", err)
	}
	if c.refs != 0 || r.bufferedBytes != 0 {
		t.Fatalf("consumption retained ownership: refs=%d buffered=%d", c.refs, r.bufferedBytes)
	}
}

// cancelSelectionContext pauses Err once cancellation has selected the query's
// cancellation arm, letting the test deliver a reply before it retakes r.mu.
type cancelSelectionContext struct {
	context.Context
	selected, resume chan struct{}
	once             sync.Once
}

func (c *cancelSelectionContext) Err() error {
	err := c.Context.Err()
	if err != nil {
		c.once.Do(func() { close(c.selected) })
		<-c.resume
	}
	return err
}

type emptyTimersStore struct{ store.Store }

func (emptyTimersStore) ListTimers(context.Context, store.TimerFilter) ([]store.Timer, error) {
	return nil, nil
}

func TestQueryCancellationReleasesRacedReply(t *testing.T) {
	for _, reply := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_reply", true: "raced_reply"}[reply], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			paused := &cancelSelectionContext{Context: ctx, selected: make(chan struct{}), resume: make(chan struct{})}
			defer func() {
				select {
				case <-paused.resume:
				default:
					close(paused.resume)
				}
			}()
			r := queryTestRouter(t)
			unlocked := make(chan struct{}, 2)
			r.config.lockObserver = func(string, time.Duration) { unlocked <- struct{}{} }
			r.operations <- struct{}{}
			done := make(chan error, 1)
			go func() {
				r.mu.Lock()
				op := &operation{r: r, ctx: paused, locked: true, slot: true, kind: "query", since: time.Now()}
				u, c, err := r.query(op, store.Call{CallID: "call", DeviceID: "d", DispatchedBootID: "boot", State: store.CallCompleted})
				if u != nil || c != nil {
					t.Error("cancelled query passed on reply ownership")
				}
				op.unlock()
				done <- err
			}()
			wait.Recv(t, unlocked)
			cancel()
			wait.Recv(t, paused.selected)
			if reply {
				r.mu.Lock()
				r.answer("call", dispatched("call"), nil)
				r.mu.Unlock()
				if r.Stats().BufferedBytes == 0 {
					t.Fatal("raced reply was not charged")
				}
			}
			close(paused.resume)
			if err := wait.Recv(t, done); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if stats := r.Stats(); stats.BufferedBytes != 0 || stats.QueryWaiters != 0 {
				t.Fatalf("cancellation retained reply ownership: %+v", stats)
			}
		})
	}
}

// queryTestRouter isolates query and subscription ownership without starting
// timers or sockets. All store operations used here are bounded stub reads.
func queryTestRouter(t *testing.T) *router {
	t.Helper()
	registry, err := gateway.NewRegistry(1)
	if err != nil {
		t.Fatal(err)
	}
	conn := gateway.Connection{Target: gateway.Target{InstanceID: "i", ConnectionID: "c", DeviceID: "d", BootID: "boot"}, UserID: "u", DeviceName: "desk"}
	if _, err = registry.Register(t.Context(), conn); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.Activate(t.Context(), conn); err != nil {
		t.Fatal(err)
	}
	return &router{
		config: Config{Store: emptyTimersStore{}, Clock: clock.New(), Registry: registry,
			MaxSubscriptions: 8, MaxUpdatesPerSubscription: 8, MaxMessageBytes: 64 << 10,
			MaxSubscriptionBytes: 1 << 20, MaxBufferedUpdateBytes: 1 << 20,
			Sender: SenderFunc(func(context.Context, gateway.Target, *circusv1.DaemonDownlink) error { return nil }),
		},
		ctx: context.Background(), subs: make(map[*Subscription]struct{}),
		queries: make(map[string][]*queryWaiter), operations: make(chan struct{}, 1),
	}
}

// pauseQuery blocks the first unlock until the test delivers its interleaving.
func pauseQuery(t *testing.T, r *router) (<-chan struct{}, func()) {
	t.Helper()
	unlocked, resume := make(chan struct{}), make(chan struct{})
	var first, release sync.Once
	r.config.lockObserver = func(string, time.Duration) {
		first.Do(func() { close(unlocked); <-resume })
	}
	continueQuery := func() { release.Do(func() { close(resume) }) }
	t.Cleanup(continueQuery)
	return unlocked, continueQuery
}

type queryRowStore struct {
	emptyTimersStore
	row store.Call // accessed under the router lock
}

func (s *queryRowStore) GetCall(context.Context, string) (store.Call, error) { return s.row, nil }

func TestQueryCompletedBeforeRelock(t *testing.T) {
	for _, method := range []string{"query", "query_once"} {
		for _, failure := range []string{"disconnect", "unapplied"} {
			t.Run(method+"/"+failure, func(t *testing.T) {
				r := queryTestRouter(t)
				s := &queryRowStore{row: store.Call{CallID: "call", UserID: "u", DeviceID: "d", DispatchedBootID: "boot", State: store.CallRunning}}
				r.config.Store = s
				unlocked, resume := pauseQuery(t, r)
				done := make(chan error, 1)
				go func() {
					if method == "query" {
						_, err := r.Query(t.Context(), "u", "call")
						done <- err
					} else {
						sub, err := r.QueryOnce(t.Context(), "u", "call")
						if sub != nil {
							_ = sub.Close()
						}
						done <- err
					}
				}()
				wait.Recv(t, unlocked)
				r.mu.Lock()
				if failure == "disconnect" {
					r.failQueries("d", "c", true)
				} else {
					err := error(store.ErrBusy)
					r.unapplied(gateway.Connection{Target: gateway.Target{ConnectionID: "c"}}, "call", &err)
				}
				s.row.State = store.CallCompleted
				r.mu.Unlock()
				resume()
				if err := wait.Recv(t, done); !errors.Is(err, ErrDeviceOffline) {
					t.Fatalf("completed call without its result must report offline: %v", err)
				}
				if stats := r.Stats(); stats.BufferedBytes != 0 || stats.QueryWaiters != 0 || stats.Subscribers != 0 {
					t.Fatalf("failed query retained ownership: %+v", stats)
				}
			})
		}
	}
}

func TestQueryClosedBeforeRelockReleasesAnswer(t *testing.T) {
	r := queryTestRouter(t)
	r.config.Store = &queryRowStore{row: store.Call{CallID: "call", UserID: "u", DeviceID: "d", DispatchedBootID: "boot", State: store.CallRunning}}
	unlocked, resume := pauseQuery(t, r)
	done := make(chan error, 1)
	go func() { _, err := r.Query(t.Context(), "u", "call"); done <- err }()
	wait.Recv(t, unlocked)
	r.mu.Lock()
	r.answer("call", dispatched("call"), nil)
	r.closed = true
	r.mu.Unlock()
	if r.Stats().BufferedBytes == 0 {
		t.Fatal("reply was not charged before close")
	}
	resume()
	if err := wait.Recv(t, done); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	if stats := r.Stats(); stats.BufferedBytes != 0 || stats.QueryWaiters != 0 {
		t.Fatalf("closed query retained ownership: %+v", stats)
	}
}

func TestSharedSubscriptionByteLimit(t *testing.T) {
	r := queryTestRouter(t)
	r.mu.Lock()
	fast, err := r.newSub(t.Context(), "call")
	if err != nil {
		t.Fatal(err)
	}
	slow, err := r.newSub(t.Context(), "call")
	if err != nil {
		t.Fatal(err)
	}
	_, first, err := r.own(accepted("call"))
	if err != nil {
		t.Fatal(err)
	}
	_, second, err := r.own(dispatched("call"))
	if err != nil {
		t.Fatal(err)
	}
	r.config.MaxSubscriptionBytes = first.cost + second.cost - 1
	r.config.MaxBufferedUpdateBytes = first.cost + second.cost
	r.publish(accepted("call"))
	r.mu.Unlock()
	defer fast.Close()
	defer slow.Close()
	if _, err = fast.Next(t.Context()); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.publish(dispatched("call"))
	r.mu.Unlock()
	if _, err = slow.Next(t.Context()); !errors.Is(err, ErrOverflow) {
		t.Fatalf("subscriber exceeding its byte limit was not detached: %v", err)
	}
	if stats := r.Stats(); stats.BufferedBytes != second.cost {
		t.Fatalf("shared buffer: %+v", stats)
	}
	if _, err = fast.Next(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stats := r.Stats(); stats.BufferedBytes != 0 {
		t.Fatalf("consumed buffer: %+v", stats)
	}
}

func TestChargeAdmissionBoundary(t *testing.T) {
	for _, short := range []int64{0, 1} {
		t.Run(map[int64]string{0: "exact_fit", 1: "one_byte_over"}[short], func(t *testing.T) {
			r := queryTestRouter(t)
			r.mu.Lock()
			defer r.mu.Unlock()
			_, held, err := r.own(accepted("held"))
			if err != nil {
				t.Fatal(err)
			}
			_, next, err := r.own(dispatched("next"))
			if err != nil {
				t.Fatal(err)
			}
			r.config.MaxBufferedUpdateBytes = held.cost + next.cost - short
			if !r.acquireCharge(held) {
				t.Fatal("initial charge did not fit")
			}
			admitted := r.acquireCharge(next)
			if admitted != (short == 0) {
				t.Fatalf("admitted=%v with %d bytes short", admitted, short)
			}
			if admitted {
				if r.bufferedBytes != r.config.MaxBufferedUpdateBytes {
					t.Fatal("exact fit did not fill budget")
				}
				r.releaseCharge(next)
			} else if next.refs != 0 || r.bufferedBytes != held.cost {
				t.Fatal("failed admission changed ownership")
			}
			r.releaseCharge(held)
			if r.bufferedBytes != 0 {
				t.Fatal("charges not released")
			}
		})
	}
}

func TestOnceAnswerRejectsNil(t *testing.T) {
	r := queryTestRouter(t)
	r.mu.Lock()
	_, c, err := r.own(dispatched("call"))
	if err != nil || !r.acquireCharge(c) {
		t.Fatalf("own reply: %v", err)
	}
	s, err := r.onceAnswer(t.Context(), "call", nil, c)
	r.mu.Unlock()
	if err == nil || s != nil {
		t.Fatal("nil query answer did not return an internal error")
	}
	if stats := r.Stats(); stats.BufferedBytes != 0 || stats.Subscribers != 0 {
		t.Fatalf("nil query answer retained ownership: %+v", stats)
	}
}

func TestQueryOnceAnswerExceedsGlobalBudget(t *testing.T) {
	r := queryTestRouter(t)
	r.config.Store = &queryRowStore{row: store.Call{CallID: "call", UserID: "u", DeviceID: "d", DispatchedBootID: "boot", State: store.CallRunning}}
	u := dispatched("call")
	_, c, err := r.own(u)
	if err != nil {
		t.Fatal(err)
	}
	r.config.MaxBufferedUpdateBytes = c.cost - 1
	unlocked, resume := pauseQuery(t, r)
	type answer struct {
		s   *Subscription
		err error
	}
	done := make(chan answer, 1)
	go func() { s, err := r.QueryOnce(t.Context(), "u", "call"); done <- answer{s, err} }()
	wait.Recv(t, unlocked)
	r.mu.Lock()
	r.answer("call", u, nil)
	r.mu.Unlock()
	resume()
	a := wait.Recv(t, done)
	if a.s != nil {
		defer a.s.Close()
	}
	if !errors.Is(a.err, ErrOverflow) || a.s != nil {
		t.Fatalf("answer exceeding global budget: subscription=%v err=%v", a.s != nil, a.err)
	}
	if stats := r.Stats(); stats.BufferedBytes != 0 || stats.QueryWaiters != 0 || stats.Subscribers != 0 {
		t.Fatalf("rejected answer retained ownership: %+v", stats)
	}
}

func TestPublishAndAnswerChargeFailureAnswersAllWaiters(t *testing.T) {
	r := queryTestRouter(t)
	r.config.MaxMessageBytes = 64
	u := resultUpdate(&circusv1.DeviceResult{CallId: "call", Output: &circusv1.ToolOutput{
		Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: strings.Repeat("x", 128)}}},
	}})
	if _, _, err := r.own(u); !errors.Is(err, wire.ErrTooLarge) {
		t.Fatalf("test update must exceed the message limit: %v", err)
	}
	waiters := []*queryWaiter{
		{reply: make(chan queryReply, 1)},
		{reply: make(chan queryReply, 1)},
		{reply: make(chan queryReply, 1)},
	}
	r.mu.Lock()
	r.queries["call"] = append([]*queryWaiter(nil), waiters...)
	r.queryCount = len(waiters)
	r.publishAndAnswer(u)
	for i, q := range waiters {
		select {
		case reply := <-q.reply:
			if !q.answered || !errors.Is(reply.err, ErrOverflow) || reply.update != nil || reply.charge != nil {
				t.Errorf("waiter %d received an invalid failure reply: %+v", i, reply)
			}
		default:
			t.Errorf("waiter %d was left unanswered after charge failure", i)
		}
		r.removeWaiter("call", q)
	}
	r.mu.Unlock()
	if stats := r.Stats(); stats.BufferedBytes != 0 || stats.QueryWaiters != 0 {
		t.Fatalf("unchargeable update retained ownership: %+v", stats)
	}
}
