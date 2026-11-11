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

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
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
	e := &snapshotStore{options: snapshotOptions{Home: root, StateDir: filepath.Join(root, "state"), Shell: "/bin/bash", Collector: func(ctx context.Context, cmd *exec.Cmd) error {
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
	go func() { defer wg.Done(); _, _, _, err := e.load(aCtx); aResult <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("collector A did not start")
	}
	go func() {
		defer wg.Done()
		_, env, snapshotLease, err := e.load(bCtx)
		if err == nil {
			snapshotLease.release()
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

func TestSnapshotInstancesShareStateSafely(t *testing.T) {
	t.Parallel()
	b, root, _ := reviewBash(t, 0)
	snapshot, _, snapshotLease, err := b.snapshots.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshotLease.release()
	// Hold an actual command lease while a second daemon creates its snapshot.
	call := &circusv1.DeviceCall{Tool: &circusv1.DeviceCall_Bash{Bash: &circusv1.BashRequest{Command: `echo ready > ready; while [ ! -f release ]; do sleep 0.02; done; echo A-done`, Cwd: root}}}
	returned := make(chan *circusv1.DeviceResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	go func() { r, _ := b.Run(ctx, call); returned <- r }()
	await(t, func() bool { _, err := os.Stat(filepath.Join(root, "ready")); return err == nil })
	second := &snapshotStore{options: b.snapshots.options}
	path, _, pathLease, err := second.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pathLease.release()
	if filepath.Dir(filepath.Dir(path)) == filepath.Dir(filepath.Dir(snapshot)) {
		t.Fatal("instances share a directory")
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatal("second instance removed live snapshot", err)
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-returned:
		if r == nil || r.Output.IsError || r.Output.Content[0].GetText() != "A-done" {
			t.Fatal(r)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("first daemon did not finish")
	}
	// Releasing only the second instance lock permits the next startup to clean
	// that instance, while the first instance remains protected by its lease.
	// Unlock explicitly, like a process exit: Close alone keeps the lock while a
	// concurrently forked child of this process still shares the descriptor.
	if err := unlockAndClose(second.instanceLock); err != nil {
		t.Fatal(err)
	}
	third := &snapshotStore{options: b.snapshots.options}
	t.Cleanup(third.releaseInstance)
	_, _, newPathLease, err := third.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	newPathLease.release()
	if _, err := os.Stat(second.instanceDir); !os.IsNotExist(err) {
		t.Fatal("unlocked instance not cleaned", err)
	}
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRefreshFailureKeepsOld(t *testing.T) {
	t.Parallel()
	root := unitTempDir(t)
	attempts := 0
	e := &snapshotStore{options: snapshotOptions{Home: root, StateDir: filepath.Join(root, "state"), Shell: "/bin/bash", Collector: func(ctx context.Context, cmd *exec.Cmd) error {
		attempts++
		if err := cmd.Start(); err != nil {
			return err
		}
		_, err := waitProcess(ctx, cmd)
		return err
	}}}
	t.Cleanup(e.releaseInstance)
	rc := filepath.Join(root, ".bashrc")
	if err := os.WriteFile(rc, []byte("export MARK=old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	old, _, oldLease, err := e.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldLease.release()
	if err := os.WriteFile(rc, []byte("exit 3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		path, env, pathLease, err := e.load(context.Background())
		if err != nil || path != old || !strings.Contains(strings.Join(env, "\n"), "MARK=old") {
			t.Fatalf("lost old snapshot: %s %v %v", path, env, err)
		}
		pathLease.release()
	}
	if attempts != 3 {
		t.Fatalf("retried unchanged failed startup: attempts=%d", attempts)
	}
	if err := os.WriteFile(rc, []byte("export MARK=new\n"), 0600); err != nil {
		t.Fatal(err)
	}
	path, env, pathLease, err := e.load(context.Background())
	if err != nil || path == old || !strings.Contains(strings.Join(env, "\n"), "MARK=new") {
		t.Fatalf("did not retry changed startup: %s %v %v", path, env, err)
	}
	pathLease.release()
	if attempts != 4 {
		t.Fatal(attempts)
	}
	// No usable snapshot must still fail, and be retried by the next call.
	if err := os.WriteFile(rc, []byte("exit 3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fresh := &snapshotStore{options: e.options}
	t.Cleanup(fresh.releaseInstance)
	for range 2 {
		if _, _, _, err := fresh.load(context.Background()); err == nil {
			t.Fatal("cold failure succeeded")
		}
	}
	if attempts != 8 {
		t.Fatal(attempts)
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
			e := &snapshotStore{options: snapshotOptions{Home: home, Shell: "/bin/bash"}}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, _, snapshotLease, err := e.load(ctx)
			if err != nil {
				t.Fatal(err)
			}
			snapshotLease.release()
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
