package leakcheck

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// Check names one invariant for Allow.
type Check string

const (
	// Processes covers live descendants left after the test.
	Processes Check = "processes"
	// Zombies covers exited descendants the code under test never reaped.
	Zombies Check = "zombies"
	// FDs covers descriptors of the test process left open after the test.
	FDs Check = "fds"
	// Files covers output-root and state-directory contents and deleted-but-open files.
	Files Check = "files"
	// Disk covers the total size of output roots.
	Disk Check = "disk"
	// Calls covers dispatcher executions audited by AuditTools.
	Calls Check = "calls"
)

const (
	// DefaultDiskLimit bounds the output roots of one test.
	DefaultDiskLimit int64 = 64 << 20
	// DefaultMemoryLimit bounds VmHWM, multiplied by three under the race
	// detector. Measured package peaks on 2026-11-10 were at most 35 MiB, and
	// 106 MiB with -race (fstools, whose 64 MiB ranged Reads are the heaviest).
	DefaultMemoryLimit int64 = 128 << 20

	pollInterval = 10 * time.Millisecond
)

// grace bounds every wait for asynchronous cleanup. The self-test shortens it.
var grace = 2 * time.Second

// supported limits the checks to Linux, where /proc and subreapers exist.
const supported = runtime.GOOS == "linux"

// Allowance permits matching descriptor targets to stay open, for a reason.
type Allowance struct {
	Match  *regexp.Regexp
	Reason string
}

// Config holds package-level settings for Run.
type Config struct {
	// MemoryLimit bounds VmHWM; zero uses DefaultMemoryLimit.
	MemoryLimit int64
	// AllowFDs lists descriptor targets that may remain open, per test and at
	// package end, including in their deleted form.
	AllowFDs []Allowance
}

type testState struct {
	t         testing.TB
	allowed   map[Check]string
	reexec    map[string]string
	checker   *checker
	diskLimit int64
}

func (s *testState) allowance(check Check) string {
	global.mu.Lock()
	defer global.mu.Unlock()
	if reason := s.allowed[check]; reason != "" {
		return reason
	}
	return ""
}

var global struct {
	mu       sync.Mutex
	running  bool
	cfg      Config
	tests    map[testing.TB]*testState
	expected map[string]bool
	zombies  int
}

func stateFor(t testing.TB) *testState {
	t.Helper()
	global.mu.Lock()
	running := global.running
	global.mu.Unlock()
	if !running {
		t.Fatalf("leakcheck: this package's TestMain must return leakcheck.Run(m, ...)")
	}
	global.mu.Lock()
	defer global.mu.Unlock()
	if s := global.tests[t]; s != nil {
		return s
	}
	s := &testState{t: t, allowed: make(map[Check]string), reexec: make(map[string]string), diskLimit: DefaultDiskLimit}
	global.tests[t] = s
	// Registered on first use, so it runs after every check of this test.
	t.Cleanup(func() {
		global.mu.Lock()
		delete(global.tests, t)
		global.mu.Unlock()
	})
	return s
}

func requireReason(t testing.TB, reason string) {
	t.Helper()
	if strings.TrimSpace(reason) == "" {
		t.Fatalf("leakcheck: an allowance needs a reason")
	}
}

// Allow opts this test out of one check. Findings are still logged.
func Allow(t testing.TB, check Check, reason string) {
	t.Helper()
	if !supported {
		return
	}
	requireReason(t, reason)
	s := stateFor(t)
	global.mu.Lock()
	s.allowed[check] = reason
	global.mu.Unlock()
}

// AllowReexecution permits callID to execute more than once in this test.
func AllowReexecution(t testing.TB, callID, reason string) {
	t.Helper()
	if !supported {
		return
	}
	requireReason(t, reason)
	s := stateFor(t)
	global.mu.Lock()
	s.reexec[callID] = reason
	global.mu.Unlock()
}

// DiskLimit raises or lowers this test's output-root size limit.
func DiskLimit(t testing.TB, bytes int64, reason string) {
	t.Helper()
	if !supported {
		return
	}
	requireReason(t, reason)
	s := stateFor(t)
	global.mu.Lock()
	s.diskLimit = bytes
	global.mu.Unlock()
}

// Expect records files that may remain in output roots, typically the
// OutputPath announced by a result. Empty paths are ignored.
func Expect(paths ...string) {
	if !supported {
		return
	}
	global.mu.Lock()
	defer global.mu.Unlock()
	for _, path := range paths {
		if path != "" {
			global.expected[filepath.Clean(path)] = true
		}
	}
}

func expected(path string) bool {
	global.mu.Lock()
	defer global.mu.Unlock()
	return global.expected[path]
}

func allowedFD(target string) string {
	plain := strings.TrimSuffix(target, deletedSuffix)
	for _, a := range global.cfg.AllowFDs {
		if a.Match.MatchString(target) || a.Match.MatchString(plain) {
			return a.Reason
		}
	}
	return ""
}

type checker struct {
	state       *testState
	roots       []string
	outputRoots []string
	stateDirs   []string
	fdsBefore   map[int]string
	procsBefore map[string]bool
	active      bool
}

// Track starts the per-test resource check, adding roots used to attribute
// processes and descriptors to this test. The first call registers the
// check with t.Cleanup; later calls only add roots.
func Track(t testing.TB, roots ...string) {
	t.Helper()
	if !supported {
		return
	}
	s := stateFor(t)
	expanded := expand(roots)
	global.mu.Lock()
	c := s.checker
	global.mu.Unlock()
	if c == nil {
		// A test's helpers run on its goroutine, so only this call creates it.
		c = &checker{state: s, fdsBefore: selfFiles(), procsBefore: make(map[string]bool), active: true}
		for _, p := range descendants() {
			c.procsBefore[p.key()] = true
		}
		global.mu.Lock()
		s.checker = c
		global.mu.Unlock()
		t.Cleanup(c.verify)
	}
	global.mu.Lock()
	c.roots = append(c.roots, expanded...)
	global.mu.Unlock()
}

// OutputRoot tracks dir as an injected bash output root: it may contain only
// directories and files passed to Expect, and its size counts toward DiskLimit.
func OutputRoot(t testing.TB, dir string) {
	t.Helper()
	if !supported {
		return
	}
	Track(t, dir)
	global.mu.Lock()
	s := global.tests[t]
	s.checker.outputRoots = append(s.checker.outputRoots, expand([]string{dir})[0])
	global.mu.Unlock()
}

// StateDir tracks dir as an injected bash state directory: it may contain only
// the instance locks and each instance's current snapshot.
func StateDir(t testing.TB, dir string) {
	t.Helper()
	if !supported {
		return
	}
	Track(t, dir)
	global.mu.Lock()
	s := global.tests[t]
	s.checker.stateDirs = append(s.checker.stateDirs, expand([]string{dir})[0])
	global.mu.Unlock()
}

// expand returns absolute roots plus their symlink-resolved forms.
func expand(roots []string) []string {
	var out []string
	for _, root := range roots {
		abs, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		out = append(out, abs)
		if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != abs {
			out = append(out, resolved)
		}
	}
	return out
}

func under(target string, roots []string) bool {
	target = strings.TrimSuffix(target, deletedSuffix)
	for _, root := range roots {
		if target == root || strings.HasPrefix(target, root+"/") {
			return true
		}
	}
	return false
}

// exclusive reports whether t and all of its ancestors are sequential, so no
// other test can run at the same time. The testing package has no public
// accessor; without the expected fields the answer is conservatively false.
// Reading parents only after a sequential child is race-free: a sequential
// subtest's parent is blocked in t.Run.
func exclusive(t testing.TB) bool {
	tt, ok := t.(*testing.T)
	if !ok {
		return false
	}
	c := reflect.ValueOf(tt).Elem().FieldByName("common")
	for c.IsValid() && c.Kind() == reflect.Struct {
		parallel := c.FieldByName("isParallel")
		if !parallel.IsValid() || parallel.Kind() != reflect.Bool {
			return false
		}
		if parallel.Bool() {
			return false
		}
		parent := c.FieldByName("parent")
		if !parent.IsValid() || parent.Kind() != reflect.Pointer {
			return false
		}
		if parent.IsNil() {
			return true
		}
		c = parent.Elem()
	}
	return false
}

// rootList copies the roots; the sampler reads them while Track may append.
func (c *checker) rootList() []string {
	global.mu.Lock()
	defer global.mu.Unlock()
	return append([]string(nil), c.roots...)
}

func (c *checker) owns(p process) bool {
	roots := c.rootList()
	if under(cwd(p.pid), roots) || containsRoot(cmdline(p.pid), roots) {
		return true
	}
	for _, target := range openFiles(p.pid) {
		if under(target, roots) {
			return true
		}
	}
	return false
}

func containsRoot(s string, roots []string) bool {
	for _, root := range roots {
		if strings.Contains(s, root+"/") || strings.HasSuffix(s, root) {
			return true
		}
	}
	return false
}

// waitFor polls until find returns nothing or the grace expires.
func waitFor[T any](find func() []T) []T {
	deadline := time.Now().Add(grace)
	for {
		found := find()
		if len(found) == 0 || time.Now().After(deadline) {
			return found
		}
		time.Sleep(pollInterval)
	}
}

// zombieSettle ends the wait early when only the same zombies remain: three
// tracked-group polls (100 ms each) would have reaped a tracked member.
const zombieSettle = 300 * time.Millisecond

// waitForProcesses is waitFor, except that unchanged zombies stop the wait
// after zombieSettle: nothing reaps them later once tools are shut down.
func waitForProcesses(find func() []process) []process {
	deadline := time.Now().Add(grace)
	var stable string
	var since time.Time
	for {
		found := find()
		if len(found) == 0 || time.Now().After(deadline) {
			return found
		}
		var keys []string
		live := false
		for _, p := range found {
			keys = append(keys, p.key())
			live = live || p.state != "Z"
		}
		if key := strings.Join(keys, ","); live || key != stable {
			stable, since = key, time.Now()
		} else if time.Since(since) >= min(zombieSettle, grace) {
			return found
		}
		time.Sleep(pollInterval)
	}
}

func (c *checker) verify() {
	s := c.state
	t := s.t
	t.Helper()
	all := exclusive(t)
	report := func(check Check, format string, args ...any) {
		message := fmt.Sprintf(format, args...)
		if reason := s.allowance(check); reason != "" {
			t.Logf("leakcheck: allowed %s (%s): %s", check, reason, message)
			return
		}
		t.Errorf("leakcheck [%s]: %s", check, message)
	}
	scope := "attributed to this test by cwd, open files, command line, or a process group or session seen with them"
	if all {
		scope = "started while this sequential test ran"
	}

	// 1. Processes.
	procs := waitForProcesses(func() []process {
		var found []process
		for _, p := range scanDescendants() {
			if c.procsBefore[p.key()] {
				continue
			}
			if all || ownerOf(p) == c || c.owns(p) {
				found = append(found, p)
			}
		}
		return found
	})
	if len(procs) > 0 {
		var live, zombies []string
		for _, p := range procs {
			if p.state == "Z" {
				zombies = append(zombies, "    "+describe(p))
			} else {
				live = append(live, "    "+describe(p))
			}
		}
		if len(live) > 0 {
			report(Processes, "%d live descendant process(es) %s remain after the test, its Shutdown and a grace of up to %s; killing and reaping them:\n%s", len(live), scope, grace, strings.Join(live, "\n"))
		}
		if len(zombies) > 0 {
			global.mu.Lock()
			global.zombies += len(zombies)
			global.mu.Unlock()
			report(Zombies, "%d unreaped zombie descendant(s) %s remain after the test, its Shutdown and a grace of up to %s; reaping them:\n%s", len(zombies), scope, grace, strings.Join(zombies, "\n"))
		}
		for _, p := range killAndReap(procs) {
			t.Errorf("leakcheck: process %d could not be killed and reaped: %s", p.pid, describe(p))
		}
	}

	forget(c)

	// 2. Descriptors of the test process.
	global.mu.Lock()
	roots := append([]string(nil), c.roots...)
	outputRoots := append([]string(nil), c.outputRoots...)
	stateDirs := append([]string(nil), c.stateDirs...)
	global.mu.Unlock()
	runFinalizers()
	extra := waitFor(func() []string {
		var found []string
		for fd, target := range selfFiles() {
			if before := c.fdsBefore[fd]; before == target || before == strings.TrimSuffix(target, deletedSuffix) || allowedFD(target) != "" {
				continue
			}
			if all || under(target, roots) {
				found = append(found, fmt.Sprintf("fd %d -> %s", fd, target))
			}
		}
		sort.Strings(found)
		return found
	})
	if len(extra) > 0 {
		report(FDs, "%d file descriptor(s) %s remain open after %s:\n    %s", len(extra), scope, grace, strings.Join(extra, "\n    "))
	}

	// 3. Files: deleted-but-open, output roots and state directories.
	var unexpected []string
	for fd, target := range selfFiles() {
		// A descriptor that existed before the test and only lost its file
		// meanwhile belongs to an earlier test, which reported it.
		before := c.fdsBefore[fd]
		if before == target || before == strings.TrimSuffix(target, deletedSuffix) {
			continue
		}
		if deleted(target) && (all || under(target, roots)) && allowedFD(target) == "" {
			unexpected = append(unexpected, fmt.Sprintf("fd %d of the test process holds deleted file %s", fd, target))
		}
	}
	size := int64(0)
	for _, root := range outputRoots {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			if info, err := entry.Info(); err == nil {
				size += info.Size()
			}
			if !expected(path) {
				unexpected = append(unexpected, "unannounced file in output root: "+path)
			}
			return nil
		})
	}
	for _, dir := range stateDirs {
		unexpected = append(unexpected, checkStateDir(dir)...)
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		report(Files, "%d leftover file(s):\n    %s", len(unexpected), strings.Join(unexpected, "\n    "))
	}

	// 4. Disk.
	global.mu.Lock()
	diskLimit := s.diskLimit
	global.mu.Unlock()
	if len(outputRoots) > 0 && size > diskLimit {
		report(Disk, "output roots hold %s, above the %s limit", mib(size), mib(diskLimit))
	}

	global.mu.Lock()
	c.active = false
	global.mu.Unlock()
}

// checkStateDir allows the documented layout: .instances.lock, instance-*
// with .lock and at most one snapshot-* holding snapshot.sh and environment.
var stateEntries = []*regexp.Regexp{
	regexp.MustCompile(`^\.instances\.lock$`),
	regexp.MustCompile(`^instance-[^/]+$`),
	regexp.MustCompile(`^instance-[^/]+/\.lock$`),
	regexp.MustCompile(`^instance-[^/]+/snapshot-[^/]+$`),
	regexp.MustCompile(`^instance-[^/]+/snapshot-[^/]+/(snapshot\.sh|environment)$`),
}

func checkStateDir(dir string) []string {
	var found []string
	snapshots := make(map[string][]string)
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		ok := false
		for _, pattern := range stateEntries {
			ok = ok || pattern.MatchString(rel)
		}
		if !ok {
			found = append(found, "unexpected state directory entry: "+path)
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if parts := strings.Split(rel, "/"); len(parts) == 2 && strings.HasPrefix(parts[1], "snapshot-") {
			snapshots[parts[0]] = append(snapshots[parts[0]], parts[1])
		}
		return nil
	})
	for instance, dirs := range snapshots {
		if len(dirs) > 1 {
			found = append(found, fmt.Sprintf("obsolete snapshots retained in %s: %v", filepath.Join(dir, instance), dirs))
		}
	}
	return found
}

// Run executes the tests with a child subreaper and an isolated HOME, then
// checks the package-wide invariants. TestMain passes its result to os.Exit.
func Run(m *testing.M, cfg Config) int {
	if !supported {
		return m.Run()
	}
	if cfg.MemoryLimit <= 0 {
		cfg.MemoryLimit = DefaultMemoryLimit * raceMemoryFactor
	}
	if err := setChildSubreaper(); err != nil {
		fmt.Fprintf(os.Stderr, "leakcheck: PR_SET_CHILD_SUBREAPER: %v\n", err)
		return 1
	}
	home, err := isolateHome()
	if err != nil {
		fmt.Fprintf(os.Stderr, "leakcheck: isolate HOME: %v\n", err)
		return 1
	}
	warmUp()
	baseFDs := selfFiles()
	baseProcs := make(map[string]bool)
	for _, p := range scanDescendants() {
		baseProcs[p.key()] = true
	}
	global.mu.Lock()
	global.running, global.cfg = true, cfg
	global.tests = make(map[testing.TB]*testState)
	global.expected = make(map[string]bool)
	global.mu.Unlock()
	stopSampler := startSampler()

	code := m.Run()
	stopSampler()

	failures := finalCheck(baseFDs, baseProcs, home)
	if global.zombies > 0 {
		fmt.Fprintf(os.Stderr, "leakcheck: reaped %d zombie(s) that the code under test left unreaped\n", global.zombies)
	}
	if testing.Verbose() {
		fmt.Fprintf(os.Stderr, "leakcheck: final VmHWM %s (limit %s)\n", mib(peakRSS()), mib(cfg.MemoryLimit))
	}
	_ = os.RemoveAll(home)
	if len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "leakcheck: package-end check failed:\n  %s\n", strings.Join(failures, "\n  "))
		if code == 0 {
			code = 1
		}
	}
	return code
}

func finalCheck(baseFDs map[int]string, baseProcs map[string]bool, home string) []string {
	var failures []string
	procs := waitForProcesses(func() []process {
		var found []process
		for _, p := range scanDescendants() {
			if !baseProcs[p.key()] {
				found = append(found, p)
			}
		}
		return found
	})
	for _, p := range procs {
		failures = append(failures, "leftover process: "+describe(p))
	}
	for _, p := range killAndReap(procs) {
		failures = append(failures, fmt.Sprintf("process %d could not be killed and reaped", p.pid))
	}
	runFinalizers()
	extra := waitFor(func() []string {
		var found []string
		for fd, target := range selfFiles() {
			if baseFDs[fd] != target && allowedFD(target) == "" {
				found = append(found, fmt.Sprintf("extra fd %d -> %s", fd, target))
			}
		}
		sort.Strings(found)
		return found
	})
	// Extra descriptors include deleted-but-open files, marked "(deleted)".
	failures = append(failures, extra...)
	_ = filepath.WalkDir(home, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			failures = append(failures, "a test used default daemon storage under the isolated HOME: "+path)
		}
		return nil
	})
	if failure := memoryFailure(global.cfg.MemoryLimit); failure != "" {
		failures = append(failures, failure)
	}
	return failures
}

// memoryFailure reports a process VmHWM above limit. It is checked once per
// package: the peak is process-wide, so per-test limits would blame
// whichever parallel test happened to run.
func memoryFailure(limit int64) string {
	if peak := peakRSS(); peak > limit {
		return fmt.Sprintf("process peak RSS (VmHWM) %s exceeds the package limit %s", mib(peak), mib(limit))
	}
	return ""
}

// runFinalizers forces a collection and waits until the finalizers it queued
// have run, so an unreachable *os.File is closed before descriptors are
// sampled instead of whenever the next GC happens. Finalizers run in one
// goroutine, batch by batch: a sentinel from a second collection can only run
// after the whole batch holding the first one.
func runFinalizers() {
	type sentinel struct{ _ *int }
	for range 2 {
		done := make(chan struct{})
		s := &sentinel{}
		runtime.SetFinalizer(s, func(*sentinel) { close(done) })
		s = nil
		runtime.GC()
		select {
		case <-done:
		case <-time.After(time.Second):
			return
		}
	}
}

// isolateHome points the default daemon paths at a private directory below
// the repository's .tmp, so no test can reach the real cache or state.
func isolateHome() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above the test directory")
		}
		dir = parent
	}
	base := filepath.Join(dir, ".tmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	home, err := os.MkdirTemp(base, "leakcheck-home-")
	if err != nil {
		return "", err
	}
	for key, value := range map[string]string{"HOME": home, "XDG_CACHE_HOME": filepath.Join(home, ".cache"), "XDG_STATE_HOME": filepath.Join(home, ".local", "state")} {
		if err := os.Setenv(key, value); err != nil {
			return "", err
		}
	}
	return home, nil
}

// warmUp creates the runtime's lazily opened descriptors (network poller,
// pidfd probing) before the baseline, so the first test is not blamed.
func warmUp() {
	if r, w, err := os.Pipe(); err == nil {
		_ = r.Close()
		_ = w.Close()
	}
	if path, err := exec.LookPath("true"); err == nil {
		_ = exec.Command(path).Run()
	}
}
