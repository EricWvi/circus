package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/store"
)

// ledgerHarness runs `circus ledger` against an in-process control plane
// with a ledger, as an enrolled worker of the project's owner.
type ledgerHarness struct {
	*joinHarness
	state string
}

func newLedgerHarness(t *testing.T) *ledgerHarness {
	t.Helper()
	h := &ledgerHarness{joinHarness: newJoinHarness(t, func(h *joinHarness, cfg *controlplane.Config) { cfg.DBPath = h.db })}
	h.state = filepath.Join(t.TempDir(), "state")
	t.Setenv("XDG_STATE_HOME", h.state)
	if _, err := h.join(h.link("worker"), "--role", "worker", "--name", "agent"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := h.st.CreateProject(t.Context(), store.Project{ID: "p1", UserID: "owner", Name: "Notes", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *ledgerHarness) run(args ...string) (string, string, int) {
	h.t.Helper()
	var out, errOut bytes.Buffer
	err := ledgerCommand(args, &out, &errOut)
	code := 0
	var exit callExit
	switch {
	case errors.As(err, &exit):
		code = exit.code
	case err != nil:
		code = 1
		errOut.WriteString(err.Error())
	}
	return out.String(), errOut.String(), code
}

func (h *ledgerHarness) must(args ...string) string {
	h.t.Helper()
	out, errOut, code := h.run(args...)
	if code != 0 {
		h.t.Fatalf("circus ledger %v: exit %d\n%s%s", args, code, out, errOut)
	}
	return out
}

var openedRE = regexp.MustCompile(`(?m)^changeset: (\S+)\ndirectory: (.+)$`)

func (h *ledgerHarness) open(args ...string) (string, string) {
	h.t.Helper()
	m := openedRE.FindStringSubmatch(h.must(append([]string{"open", "Notes"}, args...)...))
	if m == nil {
		h.t.Fatal("open printed no changeset and directory")
	}
	return m[1], m[2]
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func decision(number, title, body string) string {
	return "---\nnumber: " + number + "\ntitle: " + title + "\n---\n\n" + body
}

func TestLedgerCommand(t *testing.T) {
	h := newLedgerHarness(t)

	// A first changeset adds two decisions to the empty ledger.
	id, dir := h.open()
	if want := filepath.Join(h.state, "circus", "ledger", "cli", id); dir != want {
		t.Fatalf("directory %s, want %s", dir, want)
	}
	if _, err := os.Stat(filepath.Join(h.state, "circus", "ledger", "cli", id+".json")); err != nil {
		t.Fatalf("record: %v", err)
	}
	writeFile(t, filepath.Join(dir, "D?a-Plugins.md"), decision("D?a", "Plugins", "Plugins talk JSON-RPC.\n"))
	writeFile(t, filepath.Join(dir, "core", "D?b-Transport.md"), decision("D?b", "Transport", "Details of D?a.\n"))
	out := h.must("check", id)
	if !strings.Contains(out, "changed files: 2") || !strings.Contains(out, "fast-forward: yes") {
		t.Fatalf("check:\n%s", out)
	}
	out = h.must("merge", id)
	if !strings.Contains(out, "D?a -> D0\nD?b -> D1\n") {
		t.Fatalf("merge:\n%s", out)
	}

	// Two changesets from one base: the second is behind once the first
	// merges.
	first, firstDir := h.open()
	elsewhere := t.TempDir()
	// A directory beside a --dir copy is not the CLI's to touch.
	unrelated := filepath.Join(elsewhere, "second.conflicts", "keep.txt")
	writeFile(t, unrelated, "mine")
	second, secondDir := h.open("--dir", filepath.Join(elsewhere, "second"))
	for _, d := range []string{firstDir, secondDir} {
		if got := readFile(t, filepath.Join(d, "core", "D1-Transport.md")); !strings.Contains(got, "Details of D0.") {
			t.Fatalf("base copy in %s: %q", d, got)
		}
		if got := readFile(t, filepath.Join(d, "INDEX.md")); !strings.Contains(got, "| D1 | core | Transport |") {
			t.Fatalf("index in %s: %q", d, got)
		}
	}
	// In place, a move and a new decision referred to from another file.
	plugins := filepath.Join(firstDir, "D0-Plugins.md")
	writeFile(t, plugins, strings.Replace(readFile(t, plugins), "JSON-RPC.", "JSON-RPC; see D?c.", 1))
	if err := os.Rename(filepath.Join(firstDir, "core", "D1-Transport.md"), filepath.Join(firstDir, "D1-Transport.md")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(firstDir, "core", "D?c-Framing.md"), decision("D?c", "Framing", "Frames for D0.\n"))
	out = h.must("merge", first, "--commit", strings.Repeat("a", 40))
	if !strings.Contains(out, "D?c -> D2\n") {
		t.Fatalf("merge:\n%s", out)
	}
	writeFile(t, filepath.Join(secondDir, "D0-Plugins.md"), decision("D0", "Plugins", "Plugins talk gRPC.\n"))
	out, _, code := h.run("check", second)
	if code != 3 || !strings.Contains(out, "fast-forward: no") || !strings.Contains(out, "conflict: D0-Plugins.md") {
		t.Fatalf("check behind: exit %d\n%s", code, out)
	}
	if readFile(t, unrelated) != "mine" {
		t.Fatal("check touched a directory it did not create")
	}
	conflicts := filepath.Join(h.state, "circus", "ledger", "cli", second+".conflicts")
	if !strings.Contains(readFile(t, filepath.Join(conflicts, "D0-Plugins.md.main")), "see D2.") ||
		!strings.Contains(readFile(t, filepath.Join(conflicts, "D0-Plugins.md.changeset")), "gRPC") ||
		!strings.Contains(readFile(t, filepath.Join(conflicts, "D0-Plugins.md.base")), "JSON-RPC.") {
		t.Fatal("conflict versions not written")
	}
	_, errOut, code := h.run("merge", second)
	if code != 1 || !strings.Contains(errOut, "integrator grant") || !strings.Contains(errOut, "/devices") {
		t.Fatalf("merge without the grant: exit %d\n%s", code, errOut)
	}
	w, _ := h.cred(credfile.RoleWorker)
	if _, err := h.st.SetWorkerIntegrator(t.Context(), "owner", w.PrincipalID, true); err != nil {
		t.Fatal(err)
	}
	_, errOut, code = h.run("merge", second)
	if code != 1 || !strings.Contains(errOut, "unresolved conflict: D0-Plugins.md") {
		t.Fatalf("merge without resolution: exit %d\n%s", code, errOut)
	}
	resolved := t.TempDir()
	writeFile(t, filepath.Join(resolved, "D0-Plugins.md"), decision("D0", "Plugins", "Plugins talk gRPC; see D2.\n"))
	out = h.must("merge", second, "--resolved", resolved)
	// Merging again, as after a lost response, gives the same result.
	if again := h.must("merge", second); again != mergeLines(out) {
		t.Fatalf("second merge:\n%s\nfirst:\n%s", again, out)
	}

	// A discarded changeset is listed with its reason.
	third, _ := h.open()
	_, errOut, code = h.run("discard", third)
	if code != 1 || !strings.Contains(errOut, "--reason") {
		t.Fatalf("discard without a reason: exit %d\n%s", code, errOut)
	}
	h.must("discard", third, "--reason", "only reading")
	out = h.must("list", "Notes")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], third+"\tdiscarded") || !strings.Contains(lines[0], "reason only reading") ||
		!strings.HasPrefix(lines[3], id+"\tmerged") {
		t.Fatalf("list:\n%s", out)
	}
	if out = h.must("list", "Notes", "--state", "discarded"); strings.Count(out, "\n") != 1 {
		t.Fatalf("discarded:\n%s", out)
	}
	if out = h.must("list", "Notes", "--state", "merged"); strings.Count(out, "\n") != 3 || !strings.Contains(out, "code aaaaaaaaaaaa") {
		t.Fatalf("merged:\n%s", out)
	}

	// Mistakes are reported before anything is sent.
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"check", "../x"}, "invalid changeset id"},
		{[]string{"check", "unknown"}, "no local copy"},
		{[]string{"merge", first, "--commit", "HEAD"}, "--commit"},
		{[]string{"list", "Notes", "--state", "closed"}, "--state"},
		{[]string{"open"}, "usage"},
		{[]string{"open", "Elsewhere"}, "not found"},
		{[]string{"check", third}, "discarded"},
	} {
		_, errOut, code := h.run(tc.args...)
		if code != 1 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%v: exit %d\n%s", tc.args, code, errOut)
		}
	}
}

func TestLedgerMergeAgainReportsNumbers(t *testing.T) {
	h := newLedgerHarness(t)
	id, dir := h.open()
	writeFile(t, filepath.Join(dir, "D?a-Plugins.md"), decision("D?a", "Plugins", "x\n"))
	first := h.must("merge", id)
	if !strings.Contains(first, "D?a -> D0\n") {
		t.Fatalf("merge:\n%s", first)
	}
	if again := h.must("merge", id); again != mergeLines(first) {
		t.Fatalf("merge again:\n%s\nfirst:\n%s", again, first)
	}
}

// TestLedgerCheckWarnings prints references to temporary numbers no new
// decision has, without stopping the merge.
func TestLedgerCheckWarnings(t *testing.T) {
	h := newLedgerHarness(t)
	id, dir := h.open()
	writeFile(t, filepath.Join(dir, "D?a-Plugins.md"), decision("D?a", "Plugins", "See D?b.\n"))
	out := h.must("check", id)
	if !strings.Contains(out, "warning: D?a-Plugins.md: refers to D?b") || strings.Contains(out, "problem") {
		t.Fatalf("check:\n%s", out)
	}
	h.must("merge", id)
}

// behind merges a's edits and leaves b, from the same base, behind main.
func (h *ledgerHarness) behind(edit func(dir, side string)) (string, string) {
	h.t.Helper()
	w, _ := h.cred(credfile.RoleWorker)
	if _, err := h.st.SetWorkerIntegrator(h.t.Context(), "owner", w.PrincipalID, true); err != nil {
		h.t.Fatal(err)
	}
	a, aDir := h.open()
	b, bDir := h.open()
	edit(aDir, "a")
	edit(bDir, "b")
	h.must("merge", a)
	return b, bDir
}

// TestLedgerResolveToRemoval resolves a conflicting file to its absence:
// one side moved the decision the other edited.
func TestLedgerResolveToRemoval(t *testing.T) {
	h := newLedgerHarness(t)
	seed, seedDir := h.open()
	writeFile(t, filepath.Join(seedDir, "D?a-Plugins.md"), decision("D?a", "Plugins", "x\n"))
	h.must("merge", seed)
	b, bDir := h.behind(func(dir, side string) {
		if side == "a" {
			writeFile(t, filepath.Join(dir, "D0-Plugins.md"), decision("D0", "Plugins", "edited\n"))
			return
		}
		if err := os.Remove(filepath.Join(dir, "D0-Plugins.md")); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "core", "D0-Plugins.md"), decision("D0", "Plugins", "moved and edited\n"))
	})
	_ = bDir
	resolved := t.TempDir()
	writeFile(t, filepath.Join(resolved, "D0-Plugins.md.deleted"), "")
	h.must("merge", b, "--resolved", resolved)
	_, after := h.open()
	if _, err := os.Stat(filepath.Join(after, "D0-Plugins.md")); !os.IsNotExist(err) {
		t.Fatalf("old path after the merge: %v", err)
	}
	if got := readFile(t, filepath.Join(after, "core", "D0-Plugins.md")); !strings.HasSuffix(got, "moved and edited\n") {
		t.Fatalf("moved decision: %q", got)
	}
}

// TestLedgerOmittedConflicts: conflicts whose contents the check left out
// are written from the base, the local copy and the main line, and do not
// stop a merge with their resolutions.
func TestLedgerOmittedConflicts(t *testing.T) {
	h := newLedgerHarness(t)
	seed, seedDir := h.open()
	body := strings.Repeat("x", 240<<10)
	const n = 12
	name := func(i int) string { return fmt.Sprintf("D%d-T%d.md", i, i) }
	for i := range n {
		writeFile(t, filepath.Join(seedDir, fmt.Sprintf("D?%c-T%d.md", 'a'+i, i)), decision(fmt.Sprintf("D?%c", 'a'+i), fmt.Sprintf("T%d", i), body))
	}
	h.must("merge", seed)
	b, _ := h.behind(func(dir, side string) {
		for i := range n {
			writeFile(t, filepath.Join(dir, name(i)), decision(fmt.Sprintf("D%d", i), fmt.Sprintf("T%d", i), side+body))
		}
	})
	listed := h.must("list", "Notes")
	out, _, code := h.run("check", b)
	if code != 3 || strings.Contains(out, "problem") || strings.Count(out, "conflict: ") != n || strings.Contains(out, "not written") {
		t.Fatalf("check: exit %d\n%s", code, out[:min(len(out), 2000)])
	}
	conflicts := filepath.Join(h.state, "circus", "ledger", "cli", b+".conflicts")
	for i := range n {
		for side, prefix := range map[string]string{"base": "x", "changeset": "b", "main": "a"} {
			got := readFile(t, filepath.Join(conflicts, name(i)+"."+side))
			if !strings.Contains(got, "\n\n"+prefix+"x") {
				t.Fatalf("%s.%s starts %q", name(i), side, got[:min(len(got), 80)])
			}
		}
	}
	// Reading them left the changesets as they were.
	if again := h.must("list", "Notes"); again != listed {
		t.Fatalf("changesets after the check:\n%s\nbefore:\n%s", again, listed)
	}
	resolved := t.TempDir()
	for i := range n {
		writeFile(t, filepath.Join(resolved, name(i)), decision(fmt.Sprintf("D%d", i), fmt.Sprintf("T%d", i), "r"+body))
	}
	// The merge sends the resolutions and reads no contents.
	out = h.must("merge", b, "--resolved", resolved)
	if !strings.Contains(out, "versions not read: the merge sends the --resolved files") || !strings.Contains(out, "merged: ") {
		t.Fatalf("merge:\n%s", out[:min(len(out), 2000)])
	}
}

// mergeLines keeps what merge prints of its own, after the check's lines.
func mergeLines(out string) string {
	_, merge, _ := strings.Cut(out, "merged: ")
	return "merged: " + merge
}

// TestLedgerConflictsAfterMainMoved: contents read after the main line
// moved past the check are not written, and say why.
func TestLedgerConflictsAfterMainMoved(t *testing.T) {
	h := newLedgerHarness(t)
	seed, seedDir := h.open()
	writeFile(t, filepath.Join(seedDir, "D?a-Plugins.md"), decision("D?a", "Plugins", "x\n"))
	h.must("merge", seed)
	b, _ := h.behind(func(dir, side string) {
		writeFile(t, filepath.Join(dir, "D0-Plugins.md"), decision("D0", "Plugins", side+"\n"))
	})
	out, _, _ := h.run("check", b)
	m := regexp.MustCompile(`the main line is at ([0-9a-f]{40})`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("check:\n%s", out)
	}
	c, cDir := h.open()
	writeFile(t, filepath.Join(cDir, "D?a-Other.md"), decision("D?a", "Other", "y\n"))
	h.must("merge", c)
	cli, err := newLedgerCLI("", "", io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	full, why := cli.readConflicts(t.Context(), b, m[1])
	if len(full) != 0 || !strings.Contains(why, "the main line moved after the check") {
		t.Fatalf("read after the main line moved: %v, %q", full, why)
	}
	full, why = cli.readConflicts(t.Context(), b, strings.Repeat("0", 40))
	if len(full) != 0 || !strings.Contains(why, "moved") {
		t.Fatalf("read against another commit: %v, %q", full, why)
	}
}
