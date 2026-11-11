package bashtool

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type outputOptions struct {
	Home, TempDir                           string
	OutputLimitBytes, TotalOutputLimitBytes int64
}

func outputConfig(o Options) outputOptions {
	return outputOptions{o.Home, o.TempDir, o.OutputLimitBytes, o.TotalOutputLimitBytes}
}

// outputStore has an independent lock: no invariant spans snapshots and output.
// The budget lock protects stat records; the store lock protects sweep exemptions.
type outputStore struct {
	mu           sync.Mutex
	options      outputOptions
	lastSweep    time.Time
	active       map[string]bool
	outputBudget outputBudget
	effects      *effects
}

func (e *outputStore) outputRoot() string {
	if e.options.TempDir != "" {
		return filepath.Join(e.options.TempDir, fmt.Sprintf("circus-%d", os.Getuid()))
	}
	cache := os.Getenv("XDG_CACHE_HOME")
	if !filepath.IsAbs(cache) {
		home, err := userHome(e.options.Home)
		if err != nil || !filepath.IsAbs(home) {
			return ""
		}
		cache = filepath.Join(home, ".cache")
	}
	return filepath.Join(cache, "circus", "output")
}

func (e *outputStore) outputFile(sessionID, kind, pattern string) (*os.File, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	root := e.outputRoot()
	// Inferred: fail rather than write to a relative cache path when HOME is unusable.
	if root == "" {
		return nil, errors.New("cannot determine output cache directory")
	}
	dir := filepath.Join(root, component(sessionID), kind)
	paths := []string{root, filepath.Dir(dir), dir}
	if e.options.TempDir == "" {
		paths = append([]string{filepath.Dir(root)}, paths...)
	}
	for _, path := range paths {
		if err := privateDir(path); err != nil {
			return nil, err
		}
	}
	if e.active == nil {
		e.active = make(map[string]bool)
	}
	// Inferred: retain output for 24 hours, sweeping on first use and at most hourly.
	// Active files are exempt, including long-running jobs without recent output.
	now := time.Now()
	if e.lastSweep.IsZero() || now.Sub(e.lastSweep) >= time.Hour {
		// Schedule under the lock, then walk outside it. Even failed sweeps advance
		// lastSweep; individual inaccessible or vanished entries are best effort.
		go e.sweepOutput(root, now)
		e.lastSweep = now
	}
	var out *os.File
	err := e.effects.attempt("open", func() error { var err error; out, err = os.CreateTemp(dir, pattern); return err })
	if err == nil {
		// Keep a shared file lease across daemon instances. Another instance's
		// sweep must not unlink a quiet, indefinitely running ampersand child.
		err = e.effects.attempt("file-lock", func() error { return syscall.Flock(int(out.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) })
		if err != nil {
			_ = e.effects.attempt("close", out.Close)
			_ = e.effects.attempt("remove", func() error { return os.Remove(out.Name()) })
			return nil, err
		}
		e.active[out.Name()] = true
		e.effects.pause("budget-add")
		e.outputBudget.register(out)
	}
	return out, err
}

func (e *outputStore) releaseOutput(path string) {
	e.mu.Lock()
	e.effects.pause("active-remove")
	delete(e.active, path)
	e.mu.Unlock()
	e.effects.pause("budget-remove")
	e.outputBudget.release(path)
}

func (e *outputStore) sweepOutput(root string, now time.Time) {
	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		kind := filepath.Base(filepath.Dir(path))
		if kind != "tasks" && kind != "tool-results" {
			return nil
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) <= 24*time.Hour {
			return nil
		}
		e.mu.Lock()
		active := e.active[path]
		e.mu.Unlock()
		if active {
			return nil
		}
		file, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return nil
		}
		defer file.Close()
		if syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			_ = os.Remove(path)
		}
		return nil
	})
}

// Inferred: bound each command's capture file to 1 GiB, checking every 100 ms.
// A polling interval permits bounded-in-time overshoot while a writer is active.
const outputPollInterval = 100 * time.Millisecond
const defaultOutputLimit = int64(1 << 30)

// Inferred: this diagnostic explains daemon-enforced output termination.
const outputLimitMessage = "\n[circus: output exceeded 1 GiB, the command was stopped]\n"

// Inferred: running captures share a per-Bash 4 GiB budget. Finished files
// remain available under the retention policy but no longer consume this budget.
const defaultTotalOutputLimit = int64(4 << 30)

// Inferred: this diagnostic explains termination for the aggregate output cap.
const totalOutputLimitMessage = "\n[circus: total output of running commands exceeded 4 GiB, this command was stopped]\n"

type outputRecord struct {
	file    *os.File
	running bool
	message string
}

// outputBudget shares one stat pass per polling interval across all captures.
// A selected victim keeps its reason across foreground/background transfers.
// Inferred: until it finishes, do not select additional total-cap victims.
type outputBudget struct {
	mu        sync.Mutex
	files     map[string]*outputRecord
	lastCheck time.Time
}

func (a *outputBudget) register(out *os.File) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.files == nil {
		a.files = make(map[string]*outputRecord)
	}
	a.files[out.Name()] = &outputRecord{file: out}
}

func (a *outputBudget) start(out *os.File) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if record := a.files[out.Name()]; record != nil {
		record.file, record.running = out, true
	}
}

func (a *outputBudget) finish(path string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if record := a.files[path]; record != nil {
		record.running = false
	}
}

func (a *outputBudget) release(path string) {
	a.mu.Lock()
	delete(a.files, path)
	a.mu.Unlock()
}

func (e *outputStore) outputStopMessage(out *os.File) string {
	e.effects.pause("output-check")
	limit, totalLimit := e.options.OutputLimitBytes, e.options.TotalOutputLimitBytes
	if limit <= 0 {
		limit = defaultOutputLimit
	}
	if totalLimit <= 0 {
		totalLimit = defaultTotalOutputLimit
	}
	a := &e.outputBudget
	a.mu.Lock()
	defer a.mu.Unlock()
	record := a.files[out.Name()]
	if record == nil {
		return ""
	}
	// Preserve the per-file check at natural shell exit, even between shared
	// polls or after an aggregate selection. Finished files are excluded from
	// the aggregate calculation below.
	if info, err := out.Stat(); err == nil && info.Size() > limit {
		record.message = outputLimitMessage
	}
	if record.message != "" {
		return record.message
	}
	if time.Since(a.lastCheck) >= outputPollInterval {
		a.lastCheck = time.Now()
		var total, largestSize int64
		var largest *outputRecord
		pending := false
		for _, candidate := range a.files {
			if !candidate.running {
				continue
			}
			if candidate.message != "" {
				pending = true
			}
			info, err := candidate.file.Stat()
			if err != nil {
				continue
			}
			total += info.Size()
			if info.Size() > largestSize {
				largestSize, largest = info.Size(), candidate
			}
		}
		if total > totalLimit && !pending && largest != nil {
			largest.message = totalOutputLimitMessage
		}
	}
	return record.message
}

func appendOutputLimit(out *os.File, message string) error {
	_, err := out.Seek(0, 2)
	if err == nil {
		_, err = out.WriteString(message)
	}
	return err
}

// An aggregate selection applies only while the command or its group is still
// running. Preserve per-file enforcement and a termination already requested.
func outputMessageAfterExit(message string, terminationRequested, groupIsAlive bool) string {
	if message == totalOutputLimitMessage && !terminationRequested && !groupIsAlive {
		return ""
	}
	return message
}
