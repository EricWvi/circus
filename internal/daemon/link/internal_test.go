package link

import (
	"context"
	"testing"
)

func TestFinishedWorkerWinsExpiredBudget(t *testing.T) {
	done := make(chan struct{})
	close(done)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range 100 {
		if err := waitDone(ctx, done); err != nil {
			t.Fatal(err)
		}
	}
}
