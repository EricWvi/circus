//go:build e2e && linux

package e2e

import (
	"errors"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/gen/go/circus/v1/circusv1connect"
	"github.com/EricWvi/circus/internal/credfile"
	testwait "github.com/EricWvi/circus/internal/testutil/wait"
)

// ledgerCLI runs `circus ledger` with the harness's environment and extra.
func (h *harness) ledgerCLI(extra []string, code int, args ...string) (string, string) {
	h.t.Helper()
	p := h.start(extra, "", append([]string{"ledger"}, args...)...)
	actual := h.waitCode(p, 30*time.Second)
	if actual != code {
		h.t.Fatalf("circus ledger %v: exit %d, want %d\nstdout:\n%s\nstderr:\n%s", args, actual, code, p.stdout.String(), p.stderr.String())
	}
	return p.stdout.String(), p.stderr.String()
}

var ledgerOpened = regexp.MustCompile(`(?m)^changeset: (\S+)\ndirectory: (.+)$`)

func (h *harness) ledgerOpen(extra []string, project string) (string, string) {
	h.t.Helper()
	out, _ := h.ledgerCLI(extra, 0, "open", project)
	m := ledgerOpened.FindStringSubmatch(out)
	if m == nil {
		h.t.Fatalf("open printed %q", out)
	}
	return m[1], m[2]
}

func ledgerDecision(number, title, body string) string {
	return "---\nnumber: " + number + "\ntitle: " + title + "\n---\n\n" + body
}

func writeLedgerFile(t *testing.T, path, content string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte(content), 0o644))
}

// editInPlace replaces old with new in the file without replacing the
// file, as an editor's Edit does.
func editInPlace(t *testing.T, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(path)
	must(t, err)
	if !strings.Contains(string(data), old) {
		t.Fatalf("%s lacks %q", path, old)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	must(t, err)
	_, err = f.WriteString(strings.Replace(string(data), old, new, 1))
	must(t, errors.Join(err, f.Close()))
}

// TestLedgerChangesets drives changesets through the CLI against a real
// control plane: fast-forward merges by a worker without the grant, a
// changeset behind the main line refused to it and merged by an
// integrator with its conflict resolved, and a discard.
func TestLedgerChangesets(t *testing.T) {
	h := newHarnessWithDaemon(t, false)
	ctx := testwait.Ctx(t)
	jar, err := cookiejar.New(nil)
	must(t, err)
	browser := &http.Client{Jar: jar}
	defer browser.CloseIdleConnections()
	_, err = circusv1connect.NewAuthServiceClient(browser, h.base).Login(ctx, connect.NewRequest(&circusv1.LoginRequest{
		Email: enrollmentEmail, Password: enrollmentPassword,
	}))
	must(t, err)
	project, err := circusv1connect.NewProjectServiceClient(browser, h.base).CreateProject(ctx, connect.NewRequest(&circusv1.CreateProjectRequest{Name: "Plans"}))
	must(t, err)
	projectID := project.Msg.GetProject().GetProjectId()

	// A second worker, on its own credential, will hold the grant.
	integratorConfig := filepath.Join(h.dir, "integrator-config")
	integratorEnv := []string{"XDG_CONFIG_HOME=" + integratorConfig}
	link := h.run("", 0, "server", "enroll-token", "create", "--user", enrollmentEmail, "--config", filepath.Join(filepath.Dir(h.db), "server.yaml"), "--role", "worker", "--url", h.base)
	p := h.start(integratorEnv, link, "join", "--role", "worker", "--name", "integrator")
	if code := h.waitCode(p, 10*time.Second); code != 0 {
		t.Fatalf("join: exit %d\n%s", code, p.stderr.String())
	}
	integrator, err := credfile.Load(filepath.Join(integratorConfig, "circus", "worker.json"))
	must(t, err)

	// Seed the empty ledger: two new decisions, one referring to the other.
	seed, seedDir := h.ledgerOpen(nil, "Plans")
	if want := filepath.Join(h.dir, "state", "circus", "ledger", "cli", seed); seedDir != want {
		t.Fatalf("directory %s, want %s", seedDir, want)
	}
	writeLedgerFile(t, filepath.Join(seedDir, "D?a-Storage.md"), ledgerDecision("D?a", "Storage", "Keep data in SQLite.\n"))
	writeLedgerFile(t, filepath.Join(seedDir, "api", "D?b-Transport.md"), ledgerDecision("D?b", "Transport", "Use Connect; storage per D?a.\n"))
	out, _ := h.ledgerCLI(nil, 0, "merge", seed)
	if !strings.Contains(out, "D?a -> D0\nD?b -> D1\n") {
		t.Fatalf("seed merge:\n%s", out)
	}

	// Change it: an in-place edit, a new decision referred to from another
	// file, and a move. The opener merges it, since it fast-forwards.
	change, dir := h.ledgerOpen(nil, projectID)
	editInPlace(t, filepath.Join(dir, "D0-Storage.md"), "SQLite.", "SQLite; see D?a for backups.")
	writeLedgerFile(t, filepath.Join(dir, "ops", "D?a-Backups.md"), ledgerDecision("D?a", "Backups", "Back up the volume nightly.\n"))
	must(t, os.MkdirAll(filepath.Join(dir, "net"), 0o755))
	must(t, os.Rename(filepath.Join(dir, "api", "D1-Transport.md"), filepath.Join(dir, "net", "D1-Transport.md")))
	out, _ = h.ledgerCLI(nil, 0, "check", change)
	if !strings.Contains(out, "changed files: 4") || !strings.Contains(out, "fast-forward: yes") || strings.Contains(out, "problem") {
		t.Fatalf("check:\n%s", out)
	}
	code := strings.Repeat("c", 40)
	out, _ = h.ledgerCLI(nil, 0, "merge", change, "--commit", code)
	if !strings.Contains(out, "D?a -> D2\n") {
		t.Fatalf("merge:\n%s", out)
	}
	ledgerAPI := circusv1connect.NewLedgerServiceClient(browser, h.base)
	listed, err := ledgerAPI.GetLedger(ctx, connect.NewRequest(&circusv1.GetLedgerRequest{ProjectId: projectID}))
	must(t, err)
	var paths []string
	for _, e := range listed.Msg.Entries {
		paths = append(paths, e.Path)
	}
	if strings.Join(paths, ",") != "D0-Storage.md,net/D1-Transport.md,ops/D2-Backups.md" || listed.Msg.NextNumber != 3 {
		t.Fatalf("ledger = %v", listed.Msg)
	}
	storage, err := ledgerAPI.GetDecision(ctx, connect.NewRequest(&circusv1.GetDecisionRequest{ProjectId: projectID, Number: 0}))
	must(t, err)
	if storage.Msg.Decision.Body != "Keep data in SQLite; see D2 for backups.\n" {
		t.Fatalf("D0 body = %q", storage.Msg.Decision.Body)
	}
	history, err := ledgerAPI.ListDecisionHistory(ctx, connect.NewRequest(&circusv1.ListDecisionHistoryRequest{ProjectId: projectID, Number: 1}))
	must(t, err)
	if len(history.Msg.Versions) != 2 || !history.Msg.Versions[0].Moved || history.Msg.Versions[0].CodeCommit != code {
		t.Fatalf("D1 history = %v", history.Msg.Versions)
	}

	// Two changesets from the same base, both editing D0.
	first, firstDir := h.ledgerOpen(nil, "Plans")
	second, secondDir := h.ledgerOpen(nil, "Plans")
	editInPlace(t, filepath.Join(firstDir, "ops", "D2-Backups.md"), "nightly", "hourly")
	editInPlace(t, filepath.Join(firstDir, "D0-Storage.md"), "SQLite;", "SQLite in WAL mode;")
	editInPlace(t, filepath.Join(secondDir, "D0-Storage.md"), "SQLite;", "SQLite with FULL sync;")
	h.ledgerCLI(nil, 0, "merge", first)
	out, _ = h.ledgerCLI(nil, 3, "check", second)
	if !strings.Contains(out, "fast-forward: no") || !strings.Contains(out, "conflict: D0-Storage.md") {
		t.Fatalf("check behind main:\n%s", out)
	}
	mainVersion, err := os.ReadFile(filepath.Join(h.dir, "state", "circus", "ledger", "cli", second+".conflicts", "D0-Storage.md.main"))
	must(t, err)
	if !strings.Contains(string(mainVersion), "WAL mode") {
		t.Fatalf("main version = %q", mainVersion)
	}
	_, errOut := h.ledgerCLI(nil, 1, "merge", second)
	if !strings.Contains(errOut, "integrator grant") || !strings.Contains(errOut, "/devices") {
		t.Fatalf("merge without the grant:\n%s", errOut)
	}
	granted, err := circusv1connect.NewMachineServiceClient(browser, h.base).SetWorkerGrant(ctx, connect.NewRequest(&circusv1.SetWorkerGrantRequest{
		WorkerId: integrator.PrincipalID, Integrator: true}))
	must(t, err)
	if !granted.Msg.Worker.Integrator {
		t.Fatalf("grant = %v", granted.Msg.Worker)
	}
	_, errOut = h.ledgerCLI(integratorEnv, 1, "merge", second)
	if !strings.Contains(errOut, "unresolved conflict: D0-Storage.md") {
		t.Fatalf("merge without a resolution:\n%s", errOut)
	}
	resolved := filepath.Join(h.dir, "resolved")
	writeLedgerFile(t, filepath.Join(resolved, "D0-Storage.md"), strings.Replace(string(mainVersion), "WAL mode;", "WAL mode with FULL sync;", 1))
	h.ledgerCLI(integratorEnv, 0, "merge", second, "--resolved", resolved)
	storage, err = ledgerAPI.GetDecision(ctx, connect.NewRequest(&circusv1.GetDecisionRequest{ProjectId: projectID, Number: 0}))
	must(t, err)
	if storage.Msg.Decision.Body != "Keep data in SQLite in WAL mode with FULL sync; see D2 for backups.\n" {
		t.Fatalf("resolved D0 body = %q", storage.Msg.Decision.Body)
	}

	// Discard one, and find it with its reason.
	dropped, _ := h.ledgerOpen(nil, "Plans")
	h.ledgerCLI(nil, 0, "discard", dropped, "--reason", "only reading")
	out, _ = h.ledgerCLI(nil, 0, "list", "Plans", "--state", "discarded")
	if !strings.HasPrefix(out, dropped+"\tdiscarded") || !strings.Contains(out, "reason only reading") {
		t.Fatalf("discarded:\n%s", out)
	}
	out, _ = h.ledgerCLI(nil, 0, "list", "Plans")
	if strings.Count(out, "\tmerged\t") != 4 {
		t.Fatalf("list:\n%s", out)
	}
}
