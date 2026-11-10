package bashtool

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type waitingContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *waitingContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestCollectionOwnerCancellationDoesNotFailWaiter(t *testing.T) {
	t.Parallel()
	root := unitTempDir(t)
	started := make(chan struct{})
	var attempts atomic.Int32
	e := &environment{options: Options{Home: root, StateDir: filepath.Join(root, "state"), Shell: "/bin/bash", Collector: func(ctx context.Context, cmd *exec.Cmd) error {
		if attempts.Add(1) == 1 {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		timer := time.NewTimer(40 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		_, err := waitProcess(ctx, cmd)
		return err
	}}}
	t.Cleanup(e.releaseInstance)
	if err := os.WriteFile(filepath.Join(root, ".bashrc"), []byte("export SHARED_RETRY=ok\n"), 0600); err != nil {
		t.Fatal(err)
	}
	aCtx, cancelA := context.WithCancel(context.Background())
	bBase, cancelB := context.WithTimeout(context.Background(), 2*time.Second)
	bCtx := &waitingContext{Context: bBase, waiting: make(chan struct{})}
	aResult := make(chan error, 1)
	bResult := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	t.Cleanup(func() {
		cancelA()
		cancelB()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("collection did not stop")
		}
	})
	go func() { defer wg.Done(); _, _, err := e.load(aCtx); aResult <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("collector A did not start")
	}
	go func() {
		defer wg.Done()
		snapshot, env, err := e.load(bCtx)
		if err == nil {
			e.releaseSnapshot(snapshot)
			if !strings.Contains(strings.Join(env, "\n"), "SHARED_RETRY=ok") {
				err = errors.New("waiter lost collected exports")
			}
		}
		bResult <- err
	}()
	// Done is observed when B enters load's pending-collection select, so A
	// cannot be cancelled before B is actually waiting on its collection.
	select {
	case <-bCtx.waiting:
	case <-time.After(time.Second):
		t.Fatal("B did not wait for A")
	}
	cancelA()
	if err := <-aResult; !errors.Is(err, context.Canceled) {
		t.Fatal("A cancellation", err)
	}
	if err := <-bResult; err != nil {
		t.Fatal("B inherited A cancellation", err)
	}
	if attempts.Load() != 2 {
		t.Fatal("B did not collect again", attempts.Load())
	}
}

func TestOutputMessageAfterNaturalExit(t *testing.T) {
	for _, tc := range []struct {
		name, message     string
		terminated, alive bool
		want              string
	}{
		{"total victim exited", totalOutputLimitMessage, false, false, ""},
		{"total victim still has children", totalOutputLimitMessage, false, true, totalOutputLimitMessage},
		{"total victim was killed", totalOutputLimitMessage, true, false, totalOutputLimitMessage},
		{"per-file limit remains enforced", outputLimitMessage, false, false, outputLimitMessage},
		{"no selection", "", false, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := outputMessageAfterExit(tc.message, tc.terminated, tc.alive); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStateRootDefaultsIgnoreRelativeXDG(t *testing.T) {
	root := unitTempDir(t)
	for _, xdg := range []string{"", "relative-state", filepath.Join(root, "absolute-state")} {
		t.Run(xdg, func(t *testing.T) {
			home := filepath.Join(root, "home-"+component(xdg))
			if err := privateDir(home); err != nil {
				t.Fatal(err)
			}
			t.Setenv("XDG_STATE_HOME", xdg)
			e := &environment{options: Options{Home: home, TempDir: root, Shell: "/bin/bash"}}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			snapshot, _, err := e.load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			e.releaseSnapshot(snapshot)
			defer e.instanceLock.Close()
			base := xdg
			if !filepath.IsAbs(base) {
				base = filepath.Join(home, ".local", "state")
			}
			want := filepath.Join(base, "circus", "daemon")
			if got := filepath.Dir(e.instanceDir); got != want {
				t.Fatalf("state root %s, want %s", got, want)
			}
		})
	}
}
