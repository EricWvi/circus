package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/auth"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/store"
)

// agent enrolls another worker of the owner and returns its credential
// file.
func (h *ledgerHarness) agent(name string) string {
	h.t.Helper()
	ctx := h.t.Context()
	now := time.Now().UnixMilli()
	if err := h.st.CreateIdentity(ctx, store.Identity{Kind: store.KindWorker, ID: "w-" + name, UserID: "owner", Name: name, MachineID: "m-" + name, CreatedAt: now}); err != nil {
		h.t.Fatal(err)
	}
	token, err := auth.NewToken()
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.st.CreateCredential(ctx, store.Credential{ID: "c-" + name, Kind: store.KindWorker, PrincipalID: "w-" + name, TokenHash: auth.HashToken(token), CreatedAt: now}); err != nil {
		h.t.Fatal(err)
	}
	path := filepath.Join(h.t.TempDir(), name+".json")
	if err := credfile.Save(path, credfile.File{ControlPlaneURL: h.base, Role: credfile.RoleWorker, PrincipalID: "w-" + name, Token: token}); err != nil {
		h.t.Fatal(err)
	}
	return path
}

// lockedBuffer is a bytes.Buffer safe for a command writing while the test
// reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

// holdWriteSlot holds the user's check and merge slot with a check whose
// body never comes, until the returned function: the slot is held once a
// check with the same credential is refused. A held check that arrived
// while a probe held the slot was refused, and is sent again.
func (h *ledgerHarness) holdWriteSlot(token string) func() {
	h.t.Helper()
	probe := circusv1connect.NewLedgerChangesetServiceClient(http.DefaultClient, h.base, connect.WithInterceptors(
		connect.UnaryInterceptorFunc(func(next connect.UnaryFunc) connect.UnaryFunc {
			return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
				req.Header().Set("Authorization", "Bearer "+token)
				return next(ctx, req)
			}
		})))
	start := func() (chan struct{}, func()) {
		body, upload := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			req, err := http.NewRequest(http.MethodPost, h.base+circusv1connect.LedgerChangesetServiceCheckChangesetProcedure, body)
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/proto")
			req.Header.Set("Authorization", "Bearer "+token)
			if res, err := http.DefaultClient.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
			}
		}()
		go func() { _, _ = upload.Write([]byte{0x0a}) }()
		return done, func() { _ = upload.CloseWithError(io.ErrUnexpectedEOF); <-done }
	}
	done, release := start()
	for deadline := time.Now().Add(30 * time.Second); ; {
		_, err := probe.CheckChangeset(h.t.Context(), connect.NewRequest(&circusv1.CheckChangesetRequest{ChangesetId: "0190a0a0-0000-7000-8000-000000000000"}))
		if connect.CodeOf(err) == connect.CodeResourceExhausted {
			return release
		}
		if connect.CodeOf(err) != connect.CodeNotFound || time.Now().After(deadline) {
			release()
			h.t.Fatalf("holding the write slot: %v", err)
		}
		select {
		case <-done:
			done, release = start()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestLedgerWaitsWhileBusy: an agent's check waits, saying so, while
// another agent of the same user holds the user's check and merge slot,
// and goes through once it is free.
func TestLedgerWaitsWhileBusy(t *testing.T) {
	h := newLedgerHarness(t)
	w, _ := h.cred(credfile.RoleWorker)
	id, dir := h.open()
	writeFile(t, filepath.Join(dir, "D?a-New.md"), decision("D?a", "New", "x\n"))
	release := h.holdWriteSlot(w.Token)
	var out, errOut lockedBuffer
	result := make(chan error, 1)
	go func() { result <- ledgerCommand([]string{"check", id}, &out, &errOut) }()
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(errOut.String(), "the ledger is busy"); {
		select {
		case err := <-result:
			release()
			t.Fatalf("check ended while the slot was held: %v\n%s%s", err, out.String(), errOut.String())
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			release()
			t.Fatal("the check never waited")
		}
	}
	release()
	if err := <-result; err != nil {
		t.Fatalf("check after the slot was freed: %v\n%s", err, errOut.String())
	}
	if !strings.Contains(out.String(), "fast-forward: yes") || strings.Count(errOut.String(), "busy") != 1 {
		t.Fatalf("check:\n%s%s", out.String(), errOut.String())
	}
}

// TestLedgerAgentsCheckConcurrently: two agents of one user check their
// changesets at the same time, repeatedly; every check succeeds.
func TestLedgerAgentsCheckConcurrently(t *testing.T) {
	h := newLedgerHarness(t)
	credentials := []string{"", h.agent("other")}
	var ids []string
	for i := range credentials {
		args := []string{"open", "Notes"}
		if credentials[i] != "" {
			args = append(args, "--credentials", credentials[i])
		}
		m := openedRE.FindStringSubmatch(h.must(args...))
		ids = append(ids, m[1])
		// About 1 MiB each: the package's tests share one process's memory
		// limit with the in-process server.
		for j := range 8 {
			writeFile(t, filepath.Join(m[2], "big", "D?"+string(rune('a'+j))+"-File.md"),
				decision("D?"+string(rune('a'+j)), "File", strings.Repeat("x", 128<<10)+"\n"))
		}
	}
	for round := range 5 {
		var wg sync.WaitGroup
		errs := make([]error, len(ids))
		outs := make([]*lockedBuffer, len(ids))
		for i := range ids {
			outs[i] = &lockedBuffer{}
			args := []string{"check", ids[i]}
			if credentials[i] != "" {
				args = append(args, "--credentials", credentials[i])
			}
			wg.Go(func() { errs[i] = ledgerCommand(args, io.Discard, outs[i]) })
		}
		wg.Wait()
		for i, err := range errs {
			var exit callExit
			if err != nil && !(errors.As(err, &exit) && exit.code == 0) {
				t.Fatalf("round %d, agent %d: %v\n%s", round, i, err, outs[i].String())
			}
		}
	}
}

// TestLedgerRetriesOnlyBusy: a refusal a retry cannot pass, such as the
// project's open changesets at their limit, ends the command at once.
func TestLedgerRetriesOnlyBusy(t *testing.T) {
	h := newLedgerHarness(t)
	for range store.MaxOpenLedgerChangesets {
		h.open()
	}
	start := time.Now()
	_, errOut, code := h.run("open", "Notes")
	if code != 1 || !strings.Contains(errOut, "open changesets") || strings.Contains(errOut, "busy") || time.Since(start) > 5*time.Second {
		t.Fatalf("open at the limit: exit %d after %v\n%s", code, time.Since(start), errOut)
	}
}

// TestLedgerRetryStops: a check still refused when its retry time is up
// fails with the refusal.
func TestLedgerRetryStops(t *testing.T) {
	h := newLedgerHarness(t)
	w, _ := h.cred(credfile.RoleWorker)
	id, dir := h.open()
	writeFile(t, filepath.Join(dir, "D?a-New.md"), decision("D?a", "New", "x\n"))
	release := h.holdWriteSlot(w.Token)
	defer release()
	saved := busyRetry
	busyRetry = time.Second
	defer func() { busyRetry = saved }()
	start := time.Now()
	_, errOut, code := h.run("check", id)
	elapsed := time.Since(start)
	if code != 1 || !strings.Contains(errOut, "ledger is busy") || strings.Count(errOut, "retrying") != 1 || elapsed < time.Second || elapsed > 15*time.Second {
		t.Fatalf("check while the slot stays held: exit %d after %v\n%s", code, elapsed, errOut)
	}
}
