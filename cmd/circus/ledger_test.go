package main

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/controlplane"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/store"
	"golang.org/x/sys/unix"
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

// TestLedgerCheckLocalCopy: open leaves nothing of its own in the copy;
// check reports what the ledger cannot hold as problems (exit 3) before
// sending anything, merge refuses it (exit 3); a copy without its
// manifest is refused.
func TestLedgerCheckLocalCopy(t *testing.T) {
	h := newLedgerHarness(t)
	seed, seedDir := h.open()
	writeFile(t, filepath.Join(seedDir, "D?a-Plugins.md"), decision("D?a", "Plugins", "x\n"))
	h.must("merge", seed)
	id, dir := h.open()
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 2 {
		t.Fatalf("copy after open: %v %v", entries, err)
	}
	if out := h.must("check", id); !strings.Contains(out, "changed files: 0") {
		t.Fatalf("check of the fresh copy:\n%s", out)
	}
	link := filepath.Join(dir, "link.md")
	if err := os.Symlink("D0-Plugins.md", link); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "D?a-Big.md")
	writeFile(t, big, decision("D?a", "Big", strings.Repeat("x", 300<<10)))
	out, _, code := h.run("check", id)
	if code != 3 || !strings.Contains(out, "changed files: 0\n") || !strings.Contains(out, "problem: link.md: a symbolic link") ||
		!strings.Contains(out, "problem: D?a-Big.md: 307") {
		t.Fatalf("check with a symlink and a big file: exit %d\n%s", code, out)
	}
	// As for the server's problems, merge stops with exit 3.
	if _, errOut, code := h.run("merge", id); code != 3 {
		t.Fatalf("merge with local problems: exit %d\n%s", code, errOut)
	}
	for _, p := range []string{link, big} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	// A copy whose manifest is gone cannot be compared: open it again.
	stateDir := filepath.Join(h.state, "circus", "ledger", "cli")
	if err := os.Remove(filepath.Join(stateDir, id+".manifest.json")); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := h.run("check", id); code == 0 || !strings.Contains(errOut, "manifest") {
		t.Fatalf("check without a manifest: exit %d\n%s", code, errOut)
	}
}

// TestLedgerOpenBelowReadOnlyParent: open --dir into a writable directory
// whose parent is read-only works, and leaves no probe file anywhere.
func TestLedgerOpenBelowReadOnlyParent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only directories")
	}
	h := newLedgerHarness(t)
	shared := filepath.Join(t.TempDir(), "shared")
	copyDir := filepath.Join(shared, "copy")
	if err := os.MkdirAll(copyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(shared, 0o755) })
	id, dir := h.open("--dir", copyDir)
	if dir != copyDir {
		t.Fatalf("directory %s", dir)
	}
	if out := h.must("check", id); !strings.Contains(out, "changed files: 0") {
		t.Fatalf("check:\n%s", out)
	}
	for _, d := range []string{shared, copyDir, filepath.Join(h.state, "circus", "ledger", "cli")} {
		entries, err := os.ReadDir(d)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".circus-clock") {
				t.Fatalf("probe left in %s", d)
			}
		}
	}
}

// TestLedgerSessionChangesets: list --session lists one session's
// changesets with the session instead of an opener; a changeset without a
// local copy that is not a session's is still refused; --session is for
// list only.
func TestLedgerSessionChangesets(t *testing.T) {
	h := newLedgerHarness(t)
	ctx := t.Context()
	now := time.Now().UnixMilli()
	if err := h.st.CreateLedgerChangeset(ctx, store.LedgerChangeset{ID: "scs-1", ProjectID: "p1", State: store.LedgerChangesetOpen,
		SessionID: "session-1", CreatedAt: now, UpdatedAt: now, Archived: true}); err != nil {
		t.Fatal(err)
	}
	id, _ := h.open()
	out := h.must("list", "Notes", "--session", "session-1")
	if !strings.HasPrefix(out, "scs-1\topen\t") || !strings.Contains(out, "\tsession session-1") || strings.Contains(out, id) {
		t.Fatalf("list --session:\n%s", out)
	}
	if out := h.must("list", "Notes"); !strings.Contains(out, id) || !strings.Contains(out, "scs-1") {
		t.Fatalf("list:\n%s", out)
	}
	if _, errOut, code := h.run("check", id, "--session", "session-1"); code != 1 || !strings.Contains(errOut, "--session is for list") {
		t.Fatalf("check --session: exit %d\n%s", code, errOut)
	}
	if err := os.Remove(filepath.Join(h.state, "circus", "ledger", "cli", id+".json")); err != nil {
		t.Fatal(err)
	}
	if _, errOut, code := h.run("check", id); code != 1 || !strings.Contains(errOut, "no local copy") {
		t.Fatalf("check without a local copy: exit %d\n%s", code, errOut)
	}
}

var exportedRE = regexp.MustCompile(`(?m)^commit: (.+)\ndirectory: (.+)$`)

// tree reads every regular file below dir by its slash path; anything
// else fails the test.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	files := make(map[string]string)
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case d.IsDir():
		case d.Type().IsRegular():
			files[filepath.ToSlash(rel)] = readFile(t, p)
		default:
			t.Errorf("%s is not a regular file", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// names lists the entries of dir.
func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestLedgerExport(t *testing.T) {
	h := newLedgerHarness(t)
	export := func(args ...string) (string, string) {
		t.Helper()
		m := exportedRE.FindStringSubmatch(h.must(append([]string{"export", "Notes"}, args...)...))
		if m == nil {
			t.Fatal("export printed no commit and directory")
		}
		return m[1], m[2]
	}
	root := t.TempDir()
	// refused runs an export that must fail with message and leave root's
	// entries as they were: no temporary directory remains.
	refused := func(message string, args ...string) {
		t.Helper()
		before := names(t, root)
		_, errOut, code := h.run(append([]string{"export"}, args...)...)
		if code != 1 || !strings.Contains(errOut, message) {
			t.Fatalf("export %v: exit %d\n%s", args, code, errOut)
		}
		if after := names(t, root); !slices.Equal(after, before) {
			t.Fatalf("export %v left %v, before %v", args, after, before)
		}
	}

	// The empty ledger exports its index alone.
	commit, dir := export("--dir", filepath.Join(root, "empty"))
	if files := tree(t, dir); commit != "none (the ledger is empty)" || len(files) != 1 || files["INDEX.md"] == "" {
		t.Fatalf("empty export %q: %v", commit, files)
	}

	id, open := h.open()
	writeFile(t, filepath.Join(open, "D?a-Plugins.md"), decision("D?a", "Plugins", "Plugins talk JSON-RPC.\n"))
	writeFile(t, filepath.Join(open, "core", "D?b-Transport.md"), decision("D?b", "Transport", "Details of D?a.\n"))
	h.must("merge", id)
	changesets := h.must("list", "Notes")

	// A new directory, with the usual mode, and nothing left beside it.
	fresh := filepath.Join(root, "fresh")
	commit, dir = export("--dir", fresh)
	if !store.IsLedgerCommitID(commit) || dir != fresh {
		t.Fatalf("export printed commit %q, directory %q", commit, dir)
	}
	want := tree(t, fresh)
	if len(want) != 3 || !strings.Contains(want["core/D1-Transport.md"], "Details of D0.") || !strings.Contains(want["INDEX.md"], "| D1 | core | Transport |") {
		t.Fatalf("export: %v", want)
	}
	if got := names(t, root); !slices.Equal(got, []string{"empty", "fresh"}) {
		t.Fatalf("beside the export: %v", got)
	}
	if again, _ := export("--dir", filepath.Join(root, "again")); again != commit || !maps.Equal(tree(t, filepath.Join(root, "again")), want) {
		t.Fatalf("second export read %s, first %s", again, commit)
	}
	// Exports create no changeset.
	if got := h.must("list", "Notes"); got != changesets {
		t.Fatalf("changesets after exports:\n%s\nbefore:\n%s", got, changesets)
	}

	// DIR must be new: anything there, even an empty directory or a
	// symlink to nothing, is refused and left as it was.
	writeFile(t, filepath.Join(fresh, "notes.txt"), "mine")
	refused("exists", "Notes", "--dir", fresh)
	if got := tree(t, fresh); len(got) != 4 || got["notes.txt"] != "mine" {
		t.Fatalf("a refused export changed the directory: %v", got)
	}
	if err := os.Mkdir(filepath.Join(root, "kept"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "file"), "a file")
	if err := os.Symlink(filepath.Join(root, "nowhere"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"kept", "file", "link"} {
		refused("exists", "Notes", "--dir", filepath.Join(root, name))
	}
	if st, err := os.Stat(filepath.Join(root, "kept")); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("kept: %v, %v", st, err)
	}
	refused("no such file", "Notes", "--dir", filepath.Join(root, "missing", "dir"))
	// A refusal of the server creates nothing, and says so.
	refused(filepath.Join(root, "elsewhere")+" was not created", "Elsewhere", "--dir", filepath.Join(root, "elsewhere"))

	// Usage.
	if _, errOut, code := h.run("export", "Notes"); code != 1 || !strings.Contains(errOut, "--dir") {
		t.Fatalf("export without --dir: exit %d\n%s", code, errOut)
	}
	if _, _, code := h.run("export", "Notes", "--dir", filepath.Join(root, "r"), "--replace"); code != 1 {
		t.Fatalf("export --replace: exit %d", code)
	}
}

// mainLineFake serves two ReadMainLine pages; second runs before the
// second page is answered, with commit as its commit, or err.
type mainLineFake struct {
	circusv1connect.UnimplementedLedgerChangesetServiceHandler
	commit string
	err    error
	second func()
}

func (f *mainLineFake) ReadMainLine(_ context.Context, req *connect.Request[circusv1.ReadMainLineRequest]) (*connect.Response[circusv1.ReadMainLineResponse], error) {
	first := strings.Repeat("a", 40)
	if req.Msg.PageToken == "" {
		return connect.NewResponse(&circusv1.ReadMainLineResponse{Commit: first, ProjectId: "p1", NextPageToken: "next",
			Files: []*circusv1.LedgerFile{{Path: "D0-One.md", Content: "one"}, {Path: "core/net/D2-Three.md", Content: "three"}}}), nil
	}
	if f.second != nil {
		f.second()
	}
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&circusv1.ReadMainLineResponse{Commit: cmp.Or(f.commit, first), ProjectId: "p1",
		Files: []*circusv1.LedgerFile{{Path: "core/net/D3-Two.md", Content: "two"}}}), nil
}

// exportWith runs export to dir against f.
func exportWith(t *testing.T, f *mainLineFake, dir string) (string, error) {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(circusv1connect.NewLedgerChangesetServiceHandler(f))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cred := filepath.Join(t.TempDir(), "worker.json")
	if err := credfile.Save(cred, credfile.File{ControlPlaneURL: srv.URL, Role: credfile.RoleWorker, PrincipalID: "w1", Token: "token"}); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	err := ledgerCommand([]string{"export", "Notes", "--dir", dir, "--credentials", cred}, &out, &errOut)
	return out.String(), err
}

// temporary returns the one export directory in parent.
func temporary(t *testing.T, parent string) string {
	t.Helper()
	got := names(t, parent)
	if len(got) != 1 || !strings.HasPrefix(got[0], ".dir.export-") {
		t.Fatalf("in %s: %v", parent, got)
	}
	return got[0]
}

// A server that answers another commit on a later page fails the export,
// and DIR is not made.
func TestLedgerExportCommitChanges(t *testing.T) {
	root := t.TempDir()
	_, err := exportWith(t, &mainLineFake{commit: strings.Repeat("b", 40)}, filepath.Join(root, "dir"))
	if err == nil || !strings.Contains(err.Error(), "answered commit") || !strings.Contains(err.Error(), "was not created") {
		t.Fatalf("export = %v", err)
	}
	if got := names(t, root); len(got) != 0 {
		t.Fatalf("left %v", got)
	}
}

// DIR made by someone else during the export is not replaced: the export
// fails and its temporary directory is removed.
func TestLedgerExportDirAppears(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	f := &mainLineFake{second: func() {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Error(err)
		}
	}}
	_, err := exportWith(t, f, dir)
	if err == nil || !strings.Contains(err.Error(), "appeared during the export") {
		t.Fatalf("export = %v", err)
	}
	if got := names(t, root); !slices.Equal(got, []string{"dir"}) {
		t.Fatalf("left %v", got)
	}
	if got := names(t, dir); len(got) != 0 {
		t.Fatalf("dir holds %v", got)
	}
}

// Cleanup goes through the parent opened at the start: when the parent is
// moved away during the export and another directory takes its place with
// a directory of the temporary one's name, the moved temporary directory
// is removed and the other one is left alone.
func TestLedgerExportCleanupThroughParent(t *testing.T) {
	root := t.TempDir()
	parent, moved := filepath.Join(root, "parent"), filepath.Join(root, "moved")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	f := &mainLineFake{err: connect.NewError(connect.CodeUnavailable, errors.New("gone")), second: func() {
		tmp := temporary(t, parent)
		if err := os.Rename(parent, moved); err != nil {
			t.Error(err)
		}
		writeFile(t, filepath.Join(parent, tmp, "keep.md"), "keep")
	}}
	_, err := exportWith(t, f, filepath.Join(parent, "dir"))
	if err == nil || !strings.Contains(err.Error(), "was not created") {
		t.Fatalf("export = %v", err)
	}
	if got := names(t, moved); len(got) != 0 {
		t.Fatalf("moved parent holds %v", got)
	}
	if got := tree(t, filepath.Join(parent, temporary(t, parent))); got["keep.md"] != "keep" {
		t.Fatalf("decoy: %v", got)
	}
}

// Modes follow the umask: with 077 the export is private.
func TestLedgerExportUmask(t *testing.T) {
	old := unix.Umask(0o077)
	defer unix.Umask(old)
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	if _, err := exportWith(t, &mainLineFake{}, dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", "core", "core/net"} {
		if st, err := os.Stat(filepath.Join(dir, p)); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("%q: %v, %v", p, st, err)
		}
	}
	for _, p := range []string{"D0-One.md", "core/net/D2-Three.md", "core/net/D3-Two.md"} {
		if st, err := os.Stat(filepath.Join(dir, p)); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%q: %v, %v", p, st, err)
		}
	}
	unix.Umask(0o022)
	other := filepath.Join(root, "other")
	if _, err := exportWith(t, &mainLineFake{}, other); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(other); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("umask 022: %v, %v", st, err)
	}
	if st, err := os.Stat(filepath.Join(other, "D0-One.md")); err != nil || st.Mode().Perm() != 0o644 {
		t.Fatalf("umask 022 file: %v, %v", st, err)
	}
}
