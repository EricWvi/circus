package workerlink

import (
	"context"
	"errors"
	"testing"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/wire"
)

func TestSendWithBusyRetry(t *testing.T) {
	attempts := 0
	err := sendWithBusyRetry(t.Context(), func(context.Context) error {
		attempts++
		if attempts < 3 {
			return wire.ErrBusy
		}
		return nil
	})
	if err != nil || attempts != 3 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	attempts = 0
	nonbusy := errors.New("send failed")
	if err = sendWithBusyRetry(t.Context(), func(context.Context) error { attempts++; return nonbusy }); !errors.Is(err, nonbusy) || attempts != 1 {
		t.Fatalf("attempts=%d err=%v", attempts, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	attempts = 0
	if err = sendWithBusyRetry(ctx, func(context.Context) error { attempts++; return wire.ErrBusy }); !errors.Is(err, context.DeadlineExceeded) || attempts < 2 {
		t.Fatalf("busy retry was not bounded by context: attempts=%d err=%v", attempts, err)
	}
}

func TestBusyRetryStopsAtBound(t *testing.T) {
	attempts := 0
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- sendWithBusyRetryLimit(ctx, 25*time.Millisecond, func(context.Context) error {
			attempts++
			return wire.ErrBusy
		})
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) || attempts < 2 {
			t.Fatalf("perpetual ErrBusy did not stop at its bound: attempts=%d err=%v", attempts, err)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("perpetual ErrBusy did not stop at its bound")
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("retry exceeded its bound: %v", elapsed)
	}
}

func TestAcquireSendSlotWaits(t *testing.T) {
	slots := make(chan struct{}, 1)
	handlers := make(chan struct{}, 1)
	handlers <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		done <- acquireSendSlot(ctx, slots, handlers, func() { t.Error("logical cap was unexpectedly full") })
	}()
	select {
	case got := <-done:
		t.Fatalf("admission closed instead of waiting for a send slot: %v", got)
	case <-time.After(25 * time.Millisecond):
	}
	<-handlers
	select {
	case got := <-done:
		if !got {
			t.Fatal("admission failed after a send slot became available")
		}
	case <-time.After(time.Second):
		t.Fatal("admission did not resume after a send slot became available")
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
