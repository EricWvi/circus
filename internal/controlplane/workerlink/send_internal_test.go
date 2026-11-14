package workerlink

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/testutil/wait"
	"github.com/EricWvi/circus/internal/wire"
)

// retryOnFake runs sendWithBusyRetryLimit on a fake clock and reports the
// result, the attempts made and the fake time that passed. It advances the
// clock by the largest backoff only while the retry waits on both its bound
// and a backoff timer, so the bound is overshot by at most one backoff.
func retryOnFake(t *testing.T, ctx context.Context, limit time.Duration, send func(int) error) (error, int, time.Duration) {
	t.Helper()
	clk := clock.New()
	start := clk.Now()
	var attempts atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- sendWithBusyRetryLimit(ctx, clk, limit, func(context.Context) error { return send(int(attempts.Add(1))) })
	}()
	var err error
	wait.For(t, func() bool {
		select {
		case err = <-done:
			return true
		default:
			if clk.Waiters() == 2 {
				clk.Advance(100 * time.Millisecond)
			}
			return false
		}
	})
	return err, int(attempts.Load()), clk.Now().Sub(start)
}

func TestSendWithBusyRetry(t *testing.T) {
	err, attempts, _ := retryOnFake(t, t.Context(), sendBusyLimit, func(n int) error {
		if n < 3 {
			return wire.ErrBusy
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	nonbusy := errors.New("send failed")
	if err, attempts, _ = retryOnFake(t, t.Context(), sendBusyLimit, func(int) error { return nonbusy }); !errors.Is(err, nonbusy) || attempts != 1 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	// The caller's context bounds the retries too, and its error is kept.
	ctx, cancel := context.WithCancel(t.Context())
	err, attempts, _ = retryOnFake(t, ctx, sendBusyLimit, func(n int) error {
		if n == 2 {
			cancel()
		}
		return wire.ErrBusy
	})
	if !errors.Is(err, context.Canceled) || attempts != 2 {
		t.Fatalf("busy retry was not bounded by context: attempts=%d err=%v", attempts, err)
	}
}

func TestBusyRetryStopsAtBound(t *testing.T) {
	const limit = 2 * time.Second
	err, attempts, elapsed := retryOnFake(t, wait.Ctx(t), limit, func(int) error { return wire.ErrBusy })
	if !errors.Is(err, context.DeadlineExceeded) || attempts < 2 {
		t.Fatalf("perpetual ErrBusy did not stop at its bound: attempts=%d err=%v", attempts, err)
	}
	if elapsed < limit || elapsed > limit+100*time.Millisecond {
		t.Fatalf("retry stopped after %v, want its %v bound", elapsed, limit)
	}
}

func TestOperationTable(t *testing.T) {
	ops := newOperations(2)
	ctx := t.Context()
	if _, reason := ops.open(ctx, 0); reason != "zero request id" {
		t.Fatalf("zero ID: %q", reason)
	}
	a, reason := ops.open(ctx, 1)
	if reason != "" {
		t.Fatal(reason)
	}
	if _, reason = ops.open(ctx, 1); reason != "duplicate request id" {
		t.Fatalf("duplicate ID: %q", reason)
	}
	if _, reason = ops.open(ctx, 2); reason != "" {
		t.Fatal(reason)
	}
	if _, reason = ops.open(ctx, 3); reason != "per-connection operation cap full" {
		t.Fatalf("cap: %q", reason)
	}
	// Unwatch frees the slot at once and stops the operation's work; its own
	// later end reports that nothing may be sent.
	ops.unwatch(1)
	if a.ctx.Err() == nil {
		t.Fatal("unwatch did not cancel the operation")
	}
	again, reason := ops.open(ctx, 1)
	if reason != "" {
		t.Fatalf("slot not free after unwatch: %q", reason)
	}
	if ops.end(1, a) {
		t.Fatal("unwatched operation ended a reused request ID")
	}
	ops.unwatch(99) // unknown: no-op
	if !ops.end(1, again) || ops.end(1, again) {
		t.Fatal("end must release exactly once")
	}
}

func TestBusyRetryFitsDefaultConsumerStall(t *testing.T) {
	opts, err := wire.FillWelcome(&circusv1.Welcome{}, wire.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sendBusyLimit >= opts.MaxConsumerStall {
		t.Fatalf("send busy limit %v must be below consumer stall %v", sendBusyLimit, opts.MaxConsumerStall)
	}
}
