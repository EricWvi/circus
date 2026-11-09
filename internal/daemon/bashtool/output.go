package bashtool

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

func (e *environment) outputRoot() string {
	if e.options.TempDir != "" {
		return filepath.Join(e.options.TempDir, fmt.Sprintf("circus-%d", os.Getuid()))
	}
	if runtime := os.Getenv("XDG_RUNTIME_DIR"); runtime != "" {
		return filepath.Join(runtime, "circus")
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("circus-%d", os.Getuid()))
}

func (b *Bash) outputFile(sessionID, kind, pattern string) (*os.File, error) {
	e := b.environment
	e.mu.Lock()
	defer e.mu.Unlock()
	root := e.outputRoot()
	dir := filepath.Join(root, component(sessionID), kind)
	for _, path := range []string{root, filepath.Dir(dir), dir} {
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
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !entry.Type().IsRegular() || e.active[path] {
				return nil
			}
			kind := filepath.Base(filepath.Dir(path))
			if kind != "tasks" && kind != "tool-results" {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if now.Sub(info.ModTime()) > 24*time.Hour {
				return os.Remove(path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		e.lastSweep = now
	}
	out, err := os.CreateTemp(dir, pattern)
	if err == nil {
		e.active[out.Name()] = true
	}
	return out, err
}

func (e *environment) releaseOutput(path string) {
	e.mu.Lock()
	delete(e.active, path)
	e.mu.Unlock()
}
