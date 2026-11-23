package dirlist

import (
	"context"
	"path/filepath"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/clock"
)

const maxProbes = 16

// Mark applies best-effort markers and caps the entries. Each listing has one
// probe in flight at a time and never waits to acquire a shared probe slot.
func (l *Lister) Mark(ctx context.Context, clk clock.Clock, deadline time.Time, result *circusv1.ListDirectoryResult) {
	entries := result.Entries
	if len(entries) == 0 {
		return
	}
	// Armed for the deadline itself: After(deadline.Sub(Now())) would miss a
	// fake clock advanced between the two calls.
	alarm := clock.Until(clk, deadline)
	check := func(probe func() bool) (value, checked bool) {
		if ctx.Err() != nil || !clk.Now().Before(deadline) {
			return false, false
		}
		select {
		case l.probeSlots <- struct{}{}:
		default:
			// Probes stuck in syscalls (such as on a hung mount) hold every
			// slot until those calls return.
			if l.logger != nil {
				l.logger.Warn("directory listing: all marker probes are busy; returning entries without markers", "path", result.Path)
			}
			return false, false
		}
		done := make(chan bool, 1)
		go func() {
			var value bool
			if ctx.Err() == nil && clk.Now().Before(deadline) {
				value = probe()
			}
			// A blocked syscall retains this slot even after the query replies.
			<-l.probeSlots
			done <- value
		}()
		select {
		case value := <-done:
			return value, ctx.Err() == nil && clk.Now().Before(deadline)
		case <-alarm:
			return false, false
		case <-ctx.Done():
			return false, false
		}
	}

	kept := entries[:0]
	i := 0
	for ; i < len(entries); i++ {
		if len(kept) == maxEntries {
			break
		}
		entry := entries[i]
		path := filepath.Join(result.Path, entry.Name)
		if entry.Symlink {
			dir, checked := check(func() bool {
				info, err := l.fs.Stat(path)
				return err == nil && info.IsDir()
			})
			if !checked {
				break
			}
			if !dir {
				continue
			}
		}
		git, checked := check(func() bool {
			_, err := l.fs.Lstat(filepath.Join(path, ".git"))
			return err == nil
		})
		if !checked {
			break
		}
		entry.GitRepository = git
		kept = append(kept, entry)
	}

	// Keep unchecked candidates in sorted order, including unresolved symlinks.
	end := min(len(entries), i+maxEntries-len(kept))
	kept = append(kept, entries[i:end]...)
	result.Truncated = result.Truncated || end < len(entries)
	clear(entries[len(kept):])
	result.Entries = kept
}
