package leakcheck

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// samplerInterval paces the background owner sampler. Short commands may be
// missed; their processes are then attributed only by cwd, open files and
// command line, or reported at package end.
const samplerInterval = 25 * time.Millisecond

// owners remembers which test owned a process, its process group and its
// session while the process was still attributable. A zombie has no cwd,
// files or command line, but keeps its pgid and sid, so a member killed
// after its shell exited can still be blamed on the right test.
var owners struct {
	mu   sync.Mutex
	keys map[string]*checker
	// seen keeps the last live cwd and command line per process, because a
	// zombie has neither.
	seen map[string]string
}

// lastSeen describes the process, or else a member of its group or session,
// as the sampler last saw it alive.
func lastSeen(p process) string {
	owners.mu.Lock()
	defer owners.mu.Unlock()
	keys := ownerKeys(p)
	for i, key := range keys {
		if seen := owners.seen[key]; seen != "" {
			if i > 0 {
				return "a " + strings.SplitN(key, ":", 2)[0] + " member: " + seen
			}
			return seen
		}
	}
	return ""
}

func ownerKeys(p process) []string {
	keys := []string{"pid:" + p.key()}
	// The test process's own group and session are shared by every direct
	// child it starts without setsid; they identify no single test.
	if p.pgid != selfPgid {
		keys = append(keys, fmt.Sprintf("pgid:%d", p.pgid))
	}
	if p.sid != selfSid {
		keys = append(keys, fmt.Sprintf("sid:%d", p.sid))
	}
	return keys
}

var selfPgid, selfSid = func() (int, int) {
	p, _ := readStat(os.Getpid())
	return p.pgid, p.sid
}()

func ownerOf(p process) *checker {
	owners.mu.Lock()
	defer owners.mu.Unlock()
	for _, key := range ownerKeys(p) {
		if c := owners.keys[key]; c != nil {
			return c
		}
	}
	return nil
}

func forget(c *checker) {
	owners.mu.Lock()
	defer owners.mu.Unlock()
	for key, owner := range owners.keys {
		if owner == c {
			delete(owners.keys, key)
			delete(owners.seen, key)
		}
	}
}

func activeCheckers() []*checker {
	global.mu.Lock()
	defer global.mu.Unlock()
	var out []*checker
	for _, s := range global.tests {
		if s.checker != nil && s.checker.active && len(s.checker.roots) > 0 {
			out = append(out, s.checker)
		}
	}
	return out
}

// startSampler only reads /proc; it never signals or waits for processes.
func startSampler() (stop func()) {
	owners.keys = make(map[string]*checker)
	owners.seen = make(map[string]string)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		tick := time.NewTicker(samplerInterval)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				sample()
			}
		}
	}()
	return func() { close(done); <-finished }
}

func sample() {
	checkers := activeCheckers()
	if len(checkers) == 0 {
		return
	}
	for _, p := range descendants() {
		if ownerOf(p) != nil || p.state == "Z" {
			continue
		}
		// A subtest may write into its parent's shared root; the most
		// specific (longest-named) matching test owns the process.
		var owner *checker
		for _, c := range checkers {
			if (owner == nil || len(c.state.t.Name()) > len(owner.state.t.Name())) && c.owns(p) {
				owner = c
			}
		}
		if owner != nil {
			owners.mu.Lock()
			seen := fmt.Sprintf("cwd=%q cmdline=%q", cwd(p.pid), cmdline(p.pid))
			for _, key := range ownerKeys(p) {
				owners.keys[key] = owner
				if _, ok := owners.seen[key]; !ok || key[0] == 'p' && key[1] == 'i' {
					owners.seen[key] = seen
				}
			}
			owners.mu.Unlock()
		}
	}
}
