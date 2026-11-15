package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"log"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type result struct {
	ID         int    `json:"id"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Column     int    `json:"column"`
	Operator   string `json:"operator"`
	Original   string `json:"original"`
	Mutated    string `json:"mutated"`
	Status     string `json:"status"`
	DurationMS int64  `json:"duration_ms"`
	Detail     string `json:"detail,omitempty"`
}

type mutant struct {
	result
	path        string
	source      []byte
	start, end  int
	replacement string
}

type summary struct {
	Total   int `json:"total"`
	Killed  int `json:"killed"`
	Lived   int `json:"lived"`
	Invalid int `json:"invalid"`
	Timeout int `json:"timeout"`
}

type report struct {
	Package string   `json:"package"`
	Mutants []result `json:"mutants"`
	Summary summary  `json:"summary"`
}

var generated = regexp.MustCompile(`(?m)^// Code generated .* DO NOT EDIT\.$`)

// sandbox isolates every test run in a bubblewrap PID namespace so that a
// mutant which breaks process reaping cannot leak processes into the host:
// when the sandboxed go test exits or is killed, the namespace collapses and
// every descendant dies with it.
type sandbox struct {
	bwrap    string
	cacheDir string
	// repoTmp is the absolute path of the repository's .tmp directory. Each
	// per-mutant scratch directory is mounted there, because the daemon test
	// suites create their own scratch directories under the repository .tmp
	// by design. Everything else in the repository stays read-only.
	repoTmp string
}

// newSandbox locates bubblewrap and the Go build cache to bind writable.
func newSandbox() (*sandbox, error) {
	const path = "/usr/bin/bwrap"
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("bubblewrap not available at %s: %w", path, err)
	}
	// Some hosts install bubblewrap but forbid unprivileged user namespaces
	// (for example AppArmor on recent Ubuntu); treat that as unavailable.
	if out, err := exec.Command(path, "--unshare-pid", "--die-with-parent", "--ro-bind", "/", "/", "--", "true").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("bubblewrap cannot create a sandbox here: %w: %s", err, strings.TrimSpace(string(out)))
	}
	out, err := exec.Command("go", "env", "GOCACHE").Output()
	if err != nil {
		return nil, fmt.Errorf("go env GOCACHE: %w", err)
	}
	cache := strings.TrimSpace(string(out))
	if cache == "" {
		return nil, fmt.Errorf("go env GOCACHE returned an empty path")
	}
	tmp, err := filepath.Abs(".tmp")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	return &sandbox{bwrap: path, cacheDir: cache, repoTmp: tmp}, nil
}

// command wraps argv in bubblewrap: a new PID namespace and session, a
// read-only bind of the root filesystem, a fresh tmpfs for /tmp, and writable
// binds only for the Go build cache and the per-mutant scratch directory,
// which is mounted at the repository .tmp path and exported as TMPDIR. The
// repository's tracked files are read-only inside.
func (s *sandbox) command(argv []string, scratch string) *exec.Cmd {
	args := []string{
		"--unshare-pid",
		"--die-with-parent",
		"--new-session",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--bind", s.cacheDir, s.cacheDir,
		"--bind", scratch, s.repoTmp,
	}
	full := append(append(append([]string{}, args...), "--"), argv...)
	cmd := exec.Command(s.bwrap, full...)
	cmd.Env = append(os.Environ(), "TMPDIR="+s.repoTmp)
	return cmd
}

func main() {
	var run, out, files string
	var workers int
	var timeout time.Duration
	var noSandbox bool
	flag.StringVar(&run, "run", "", "optional go test -run filter")
	flag.DurationVar(&timeout, "timeout", 30*time.Second, "per-mutant test timeout")
	flag.IntVar(&workers, "workers", defaultWorkers(), "parallel test processes (default max(1, nproc/4))")
	flag.StringVar(&out, "out", "mutation-report.json", "JSON report path")
	flag.StringVar(&files, "files", "", "optional source file glob, relative to the package")
	flag.BoolVar(&noSandbox, "no-sandbox", false, "debugging only: run go test without the bwrap sandbox")
	flag.Parse()
	if flag.NArg() != 1 || workers < 1 || timeout <= 0 {
		flag.Usage()
		os.Exit(2)
	}
	var sb *sandbox
	if !noSandbox {
		var err error
		if sb, err = newSandbox(); err != nil {
			log.Fatal(err)
		}
	}
	if err := execute(flag.Arg(0), run, out, files, workers, timeout, sb); err != nil {
		log.Fatal(err)
	}
}

func defaultWorkers() int { return max(1, runtime.NumCPU()/4) }

func execute(pkg, run, out, files string, workers int, timeout time.Duration, sb *sandbox) error {
	mutants, err := enumeratePackage(pkg, files)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(".tmp", 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(".tmp", "mutate-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	jobs, done := make(chan int), make(chan result)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				done <- runMutant(&mutants[i], pkg, run, tmp, timeout, sb)
			}
		}()
	}
	go func() {
		for i := range mutants {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(done)
	}()

	results := make([]result, 0, len(mutants))
	var sum summary
	for r := range done {
		results = append(results, r)
		sum.Total++
		switch r.Status {
		case "killed":
			sum.Killed++
		case "lived":
			sum.Lived++
		case "invalid":
			sum.Invalid++
		case "timeout":
			sum.Timeout++
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].ID < results[j].ID })
	data, err := json.MarshalIndent(report{Package: pkg, Mutants: results, Summary: sum}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(out, append(data, '\n'), 0o644); err != nil {
		return err
	}
	score := 0.0
	if sum.Killed+sum.Lived > 0 {
		score = 100 * float64(sum.Killed) / float64(sum.Killed+sum.Lived)
	}
	fmt.Printf("summary total=%d killed=%d lived=%d invalid=%d timeout=%d mutation_score=%.2f%%\n",
		sum.Total, sum.Killed, sum.Lived, sum.Invalid, sum.Timeout, score)
	return nil
}

func enumeratePackage(pkg, pattern string) ([]mutant, error) {
	paths, err := sourceFiles(pkg, pattern)
	if err != nil {
		return nil, err
	}
	var mutants []mutant
	for _, path := range paths {
		source, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if generated.Match(source) {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, source, parser.SkipObjectResolution)
		if err != nil {
			return nil, err
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		found := enumerate(fset, file, source)
		for i := range found {
			found[i].ID = len(mutants) + 1
			found[i].File = filepath.ToSlash(path)
			found[i].path = absolute
			found[i].source = source
			mutants = append(mutants, found[i])
		}
	}
	return mutants, nil
}

func sourceFiles(pkg, pattern string) ([]string, error) {
	var paths []string
	if pattern != "" {
		glob := pattern
		if !filepath.IsAbs(glob) && filepath.Dir(glob) == "." {
			glob = filepath.Join(pkg, glob)
		}
		matches, err := filepath.Glob(glob)
		if err != nil {
			return nil, err
		}
		paths = matches
	} else {
		entries, err := os.ReadDir(pkg)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() && filepath.Ext(entry.Name()) == ".go" {
				paths = append(paths, filepath.Join(pkg, entry.Name()))
			}
		}
	}
	var kept []string
	for _, path := range paths {
		if filepath.Ext(path) == ".go" && !strings.HasSuffix(path, "_test.go") {
			kept = append(kept, path)
		}
	}
	sort.Strings(kept)
	return kept, nil
}

func enumerate(fset *token.FileSet, file *ast.File, source []byte) []mutant {
	var mutants []mutant
	tf := fset.File(file.Pos())
	add := func(operator string, node ast.Node, start, end token.Pos, replacement string) {
		ns, ne, s, e := tf.Offset(node.Pos()), tf.Offset(node.End()), tf.Offset(start), tf.Offset(end)
		position := fset.Position(start)
		mutants = append(mutants, mutant{
			result: result{
				Line: position.Line, Column: position.Column, Operator: operator,
				Original: string(source[ns:ne]),
				Mutated:  string(source[ns:s]) + replacement + string(source[e:ne]),
			},
			start: s, end: e, replacement: replacement,
		})
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.BinaryExpr:
			boundary := map[token.Token]token.Token{token.LSS: token.LEQ, token.LEQ: token.LSS, token.GTR: token.GEQ, token.GEQ: token.GTR}
			negation := map[token.Token]token.Token{token.EQL: token.NEQ, token.NEQ: token.EQL, token.LSS: token.GEQ, token.LEQ: token.GTR, token.GTR: token.LEQ, token.GEQ: token.LSS}
			if replacement, ok := boundary[n.Op]; ok {
				add("comparison_boundary", n, n.OpPos, n.OpPos+token.Pos(len(n.Op.String())), replacement.String())
			}
			if replacement, ok := negation[n.Op]; ok {
				add("comparison_negation", n, n.OpPos, n.OpPos+token.Pos(len(n.Op.String())), replacement.String())
			}
			if n.Op == token.LAND || n.Op == token.LOR {
				replacement := token.LAND
				if n.Op == token.LAND {
					replacement = token.LOR
				}
				add("logical_swap", n, n.OpPos, n.OpPos+token.Pos(len(n.Op.String())), replacement.String())
			}
			if n.Op == token.ADD || n.Op == token.SUB {
				replacement := token.ADD
				if n.Op == token.ADD {
					replacement = token.SUB
				}
				add("arithmetic_swap", n, n.OpPos, n.OpPos+1, replacement.String())
			}
		case *ast.IncDecStmt:
			replacement, operator := "++", "incdec_swap"
			if n.Tok == token.INC {
				replacement = "--"
			}
			add(operator, n, n.TokPos, n.TokPos+2, replacement)
		case *ast.IfStmt:
			condition := string(source[tf.Offset(n.Cond.Pos()):tf.Offset(n.Cond.End())])
			add("if_negation", n.Cond, n.Cond.Pos(), n.Cond.End(), "!("+condition+")")
		case *ast.BasicLit:
			if n.Kind == token.INT {
				value, ok := new(big.Int).SetString(strings.ReplaceAll(n.Value, "_", ""), 0)
				if ok {
					value.Add(value, big.NewInt(1))
					add("integer_increment", n, n.Pos(), n.End(), value.String())
				}
			}
		case *ast.ExprStmt:
			if _, ok := n.X.(*ast.CallExpr); ok {
				add("call_removal", n, n.Pos(), n.End(), "")
			}
		case *ast.DeferStmt:
			add("call_removal", n, n.Pos(), n.End(), "")
		case *ast.ReturnStmt:
			if len(n.Results) == 1 && isErrorExpr(n.Results[0]) {
				add("return_nil", n, n.Results[0].Pos(), n.Results[0].End(), "nil")
			}
		case *ast.BranchStmt:
			if n.Tok == token.BREAK || n.Tok == token.CONTINUE {
				replacement := "break"
				if n.Tok == token.BREAK {
					replacement = "continue"
				}
				add("branch_swap", n, n.Pos(), n.Pos()+token.Pos(len(n.Tok.String())), replacement)
			}
		}
		return true
	})
	return mutants
}

func isErrorExpr(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name == "err" || strings.HasSuffix(e.Name, "Err")
	case *ast.CallExpr:
		selector, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := selector.X.(*ast.Ident)
		return ok && (pkg.Name == "fmt" && selector.Sel.Name == "Errorf" ||
			pkg.Name == "errors" && selector.Sel.Name == "New")
	}
	return false
}

func runMutant(m *mutant, pkg, run, tmp string, timeout time.Duration, sb *sandbox) result {
	started := time.Now()
	r := m.result
	mutated := append([]byte(nil), m.source...)
	mutated = append(mutated[:m.start], append([]byte(m.replacement), mutated[m.end:]...)...)
	formatted, err := format.Source(mutated)
	if err != nil {
		r.Status, r.Detail = "invalid", err.Error()
		r.DurationMS = time.Since(started).Milliseconds()
		return r
	}
	scratch := filepath.Join(tmp, fmt.Sprintf("scratch-%06d", r.ID))
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		r.Status, r.Detail = "invalid", err.Error()
		return r
	}
	defer os.RemoveAll(scratch)
	if scratch, err = filepath.Abs(scratch); err != nil {
		r.Status, r.Detail = "invalid", err.Error()
		return r
	}
	mutantPath := filepath.Join(scratch, "mutant.go")
	overlayPath := filepath.Join(scratch, "overlay.json")
	if err := os.WriteFile(mutantPath, formatted, 0o600); err != nil {
		r.Status, r.Detail = "invalid", err.Error()
		return r
	}
	// The sandboxed go command must see the overlay and the mutated file at
	// the paths recorded in the overlay: the scratch directory is mounted at
	// the repository .tmp path inside the sandbox.
	overlayArg, mutantRef := overlayPath, mutantPath
	if sb != nil {
		overlayArg = filepath.Join(sb.repoTmp, "overlay.json")
		mutantRef = filepath.Join(sb.repoTmp, "mutant.go")
	}
	overlay, _ := json.Marshal(struct{ Replace map[string]string }{Replace: map[string]string{m.path: mutantRef}})
	if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
		r.Status, r.Detail = "invalid", err.Error()
		return r
	}
	r.Status, r.Detail = runIsolated(sb, goTestArgs(pkg, run, overlayArg, timeout), scratch, timeout)
	r.DurationMS = time.Since(started).Milliseconds()
	return r
}

func goTestArgs(pkg, run, overlay string, timeout time.Duration) []string {
	args := []string{"go", "test", "-overlay", overlay, "-count=1", "-timeout", timeout.String()}
	if run != "" {
		args = append(args, "-run", run)
	}
	return append(args, pkg)
}

// runIsolated executes one test command under the sandbox (or plainly with
// -no-sandbox, for debugging) and classifies the outcome.
func runIsolated(sb *sandbox, argv []string, scratch string, timeout time.Duration) (string, string) {
	var cmd *exec.Cmd
	if sb != nil {
		cmd = sb.command(argv, scratch)
	} else {
		cmd = exec.Command(argv[0], argv[1:]...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		return "invalid", err.Error()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-timer.C:
		killTree(cmd, sb == nil)
		<-done
		return "timeout", "external test timeout"
	}
	text := output.String()
	if strings.Contains(text, "panic: test timed out") {
		killTree(cmd, sb == nil)
		return "timeout", clipped(text)
	}
	if err == nil {
		return "lived", ""
	}
	if strings.Contains(text, "[build failed]") {
		return "invalid", clipped(text)
	}
	return "killed", ""
}

// killTree stops a test run. In sandbox mode killing the bubblewrap
// supervisor by PID collapses its PID namespace, so every descendant -
// including orphans a broken reaper left behind - is killed by the kernel.
// Without a sandbox the command is its own process group leader.
func killTree(cmd *exec.Cmd, group bool) {
	if group {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		return
	}
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGKILL)
}

func clipped(text string) string {
	if len(text) > 4000 {
		return text[:4000] + "..."
	}
	return text
}
