package bashtool

import (
	"context"
	"fmt"
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This reference model follows external operations, not finishing helpers.
// Each sequence holds at least two captures and checks every operation before
// advancing. Bytes, job state, linked output, group ownership and pending
// largest-victim selection are part of the expected state.
type modeledCapture struct {
	run                           *drivenCapture
	mode                          string
	children, live, tracked, keep bool
	bytes                         int
	phase                         capturePhase
	code                          int
	jobState                      circusv1.JobEvent_State
	message                       string
}
type captureSequence struct {
	h                  *captureHarness
	models             []*modeledCapture
	closed, jobsClosed bool
	trace              []string
}

func (s *captureSequence) record(op string) {
	s.trace = append(s.trace, op)
	s.h.t.Logf("operations: %s", strings.Join(s.trace, " → "))
}
func (s *captureSequence) start(name, mode string, children bool) *modeledCapture {
	s.record("start " + name + " " + mode)
	m := &modeledCapture{run: s.h.start(name, mode, children, 0, "", 0), mode: mode, children: children, live: true, phase: captureForeground, jobState: circusv1.JobEvent_STATE_RUNNING}
	if mode == "background" {
		m.phase = captureBackground
		m.keep = true
	}
	s.models = append(s.models, m)
	s.check()
	return m
}
func (s *captureSequence) promote(m *modeledCapture) {
	s.record("promote " + m.run.name)
	s.h.promote(m.run)
	m.mode = "background"
	m.phase = captureBackground
	m.keep = true
	s.check()
}
func (s *captureSequence) fill(m *modeledCapture, n int) {
	s.record(fmt.Sprintf("fill %s +%d", m.run.name, n))
	s.h.fill(m.run, n, false)
	m.bytes += n
	s.check()
}
func (s *captureSequence) cancel(m *modeledCapture) {
	s.record("Cancel " + m.run.name)
	m.run.cancel()
	if m.mode == "background" {
		s.check()
		return
	}
	m.live = false
	m.phase = captureStopped
	m.code = 143
	m.keep = false
	reply := s.h.returned(m.run)
	if reply.err != nil || !reply.result.GetBash().Interrupted {
		s.h.t.Fatal("cancel outcome", reply.err, reply.result)
	}
	s.h.released(m.run, m.phase, m.keep)
	s.check()
}
func (s *captureSequence) natural(m *modeledCapture) {
	s.record("shell exit " + m.run.name)
	gate(s.h.t, filepath.Join(m.run.dir, "exit"))
	m.live = false
	m.phase = captureExited
	m.keep = m.mode == "background"
	m.jobState = circusv1.JobEvent_STATE_EXITED
	if m.children {
		if s.closed {
			m.phase = captureEmpty
		} else {
			m.phase = captureTracked
			m.tracked = true
		}
	}
	if m.mode == "background" {
		s.h.job(m.run)
	} else {
		s.h.returned(m.run)
	}
	if !m.tracked {
		s.h.released(m.run, m.phase, m.keep)
	}
	s.check()
}
func (s *captureSequence) stop(m *modeledCapture) {
	s.record("TaskStop " + m.run.name)
	s.h.stop(m.run)
	m.live = false
	m.phase = captureKilled
	m.code = 143
	m.keep = true
	m.jobState = circusv1.JobEvent_STATE_KILLED
	s.h.job(m.run)
	s.h.released(m.run, m.phase, m.keep)
	s.check()
}
func (s *captureSequence) pressure(m *modeledCapture, n int, aggregate bool) {
	s.record(fmt.Sprintf("pressure %s +%d total=%v", m.run.name, n, aggregate))
	if aggregate {
		// All other live captures are smaller; they must survive the shared stat
		// pass while the selected largest victim is pending and being reaped.
		for _, other := range s.models {
			if other != m && (other.live || other.tracked) && other.bytes >= m.bytes+n {
				s.h.t.Fatal("invalid largest-victim model")
			}
		}
	}
	s.h.fill(m.run, n, true)
	m.bytes += n
	m.live = false
	m.phase = captureKilled
	m.code = 137
	m.keep = m.mode == "background"
	m.jobState = circusv1.JobEvent_STATE_KILLED
	m.message = outputLimitMessage
	if aggregate {
		m.message = totalOutputLimitMessage
	}
	if m.mode == "background" {
		s.h.job(m.run)
	} else {
		s.h.returned(m.run)
	}
	s.h.released(m.run, m.phase, m.keep)
	s.check()
}
func (s *captureSequence) drain(m *modeledCapture) {
	s.record("children exit " + m.run.name)
	gate(s.h.t, filepath.Join(m.run.dir, "child-exit"))
	m.tracked = false
	m.phase = captureEmpty
	s.h.released(m.run, m.phase, m.keep)
	s.check()
}
func (s *captureSequence) shutdownJobs() {
	s.record("jobs.Shutdown")
	if err := s.h.b.jobs.Shutdown(context.Background()); err != nil {
		s.h.t.Fatal(err)
	}
	s.jobsClosed = true
	for _, m := range s.models {
		if m.live && m.mode == "background" {
			m.live = false
			m.phase = captureKilled
			m.code = 143
			m.keep = true
			m.jobState = circusv1.JobEvent_STATE_KILLED
			s.h.job(m.run)
			s.h.released(m.run, m.phase, m.keep)
		}
	}
	s.check()
}
func (s *captureSequence) shutdownBash() {
	s.record("Bash.Shutdown")
	if err := s.h.b.Shutdown(context.Background()); err != nil {
		s.h.t.Fatal(err)
	}
	s.closed = true
	for _, m := range s.models {
		if m.tracked {
			m.tracked = false
			m.phase = captureEmpty
			s.h.released(m.run, m.phase, m.keep)
		}
	}
	s.check()
}
func (s *captureSequence) check() {
	t := s.h.t
	t.Helper()
	running, tracked := 0, 0
	for _, m := range s.models {
		r := m.run
		c := r.c
		if m.live || m.tracked {
			running++
			owner := ownerForeground
			if m.mode == "background" {
				owner = ownerBackground
			}
			if m.tracked {
				owner = ownerTracker
				tracked++
			}
			if got := c.snapshotState(); got != (captureState{m.phase, owner}) {
				t.Fatal("model phase/owner", r.name, got, m.phase, owner)
			}
			if _, err := os.Stat(c.out.Name()); err != nil {
				t.Fatal("live pathname", r.name, err)
			}
			info, err := c.out.Stat()
			if err != nil {
				t.Fatal("live descriptor", r.name, err)
			}
			expectedBytes := m.bytes
			if m.tracked && m.mode == "background" {
				expectedBytes += len("\n[exited with code 0]\n")
			}
			if info.Size() != int64(expectedBytes) {
				t.Fatalf("model bytes %s: got %d want %d", r.name, info.Size(), expectedBytes)
			}
			a := &s.h.b.outputs.outputBudget
			a.mu.Lock()
			record := a.files[c.out.Name()]
			valid := record != nil && record.running && record.file == c.out
			a.mu.Unlock()
			if !valid {
				t.Fatal("live budget record", r.name)
			}
		} else {
			s.h.released(r, m.phase, m.keep)
		}
		if m.mode == "background" {
			reply := s.h.returned(r)
			id := reply.result.GetBash().JobId
			info, ok := s.h.b.jobs.Get(id)
			if !ok || info.State != m.jobState || int(info.ExitCode) != m.code {
				t.Fatal("model job", r.name, info, "expected", m.jobState, m.code)
			}
		} else if !m.live {
			reply := s.h.returned(r)
			if reply.err != nil || int(reply.result.GetBash().ExitCode) != m.code {
				t.Fatal("model foreground result", r.name, reply.err, reply.result)
			}
		}
		if !m.live && !m.tracked {
			if m.keep {
				file, err := os.Open(c.out.Name())
				if err != nil {
					t.Fatal(err)
				}
				tail := outputTail(t, file)
				_ = file.Close()
				want := 0
				if m.message != "" {
					want = 1
				}
				if strings.Count(tail, "[circus:") != want || want == 1 && !strings.HasSuffix(tail, m.message) {
					t.Fatal("model diagnostic", tail)
				}
			} else if m.message != "" && !strings.Contains(s.h.returned(r).result.Output.Content[0].GetText(), strings.TrimSpace(m.message)) {
				t.Fatal("model inline diagnostic", r.name)
			}
		}
	}
	// Record release and tracker removal occur in the final defer, after release
	// becomes visible. Wait for those publication points instead of sampling a
	// transient between the descriptor close and map deletion.
	await(t, func() bool {
		s.h.b.outputs.mu.Lock()
		a := len(s.h.b.outputs.active)
		s.h.b.outputs.mu.Unlock()
		s.h.b.outputs.outputBudget.mu.Lock()
		b := len(s.h.b.outputs.outputBudget.files)
		s.h.b.outputs.outputBudget.mu.Unlock()
		s.h.b.groups.mu.Lock()
		g := len(s.h.b.groups.stops)
		err := s.h.b.groups.err
		s.h.b.groups.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		return a == running && b == running && g == tracked
	})
}
func (s *captureSequence) finish() {
	for _, m := range s.models {
		if m.live {
			if m.mode == "background" && !s.jobsClosed {
				s.stop(m)
			} else {
				s.natural(m)
			}
		}
		if m.tracked {
			s.drain(m)
		}
	}
	s.shutdownJobs()
	s.shutdownBash()
	s.h.resources()
}

func TestCaptureModelSequences(t *testing.T) {
	seed := uint64(time.Now().UnixNano())
	if value := os.Getenv("CIRCUS_CAPTURE_SEED"); value != "" {
		var err error
		seed, err = strconv.ParseUint(value, 0, 64)
		if err != nil {
			t.Fatal("CIRCUS_CAPTURE_SEED", err)
		}
	}
	t.Logf("CIRCUS_CAPTURE_SEED=%#x", seed)
	random := rand.New(rand.NewPCG(seed, seed^0x12345))
	// Enumerate all reachable mode/operation/children cells before any random
	// expansion. TaskStop has no foreground cell because foreground has no ID;
	// Cancel and either Shutdown are real operations in every mode, including
	// cases where the contract says the still-visible job keeps running.
	t.Run("cells", func(t *testing.T) {
		for _, mode := range []string{"foreground", "background", "promoted"} {
			for _, op := range []string{"natural", "cancel", "TaskStop", "cap", "total", "jobs.Shutdown", "Bash.Shutdown"} {
				if mode == "foreground" && op == "TaskStop" {
					continue
				}
				for _, children := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/children=%v", mode, op, children), func(t *testing.T) {
						t.Parallel()
						t.Logf("CIRCUS_CAPTURE_SEED=%#x", seed)
						limit, total := int64(10000), int64(1<<20)
						if op == "total" {
							limit, total = 1<<20, 10000
						}
						s := &captureSequence{h: newCaptureHarness(t, limit, total)}
						a := s.start("A", mode, children)
						b := s.start("B", "background", !children)
						s.fill(b, 1024)
						s.fill(a, 2048)
						if mode == "promoted" {
							s.promote(a)
						}
						s.fill(b, 512)
						s.cancel(b)
						switch op {
						case "natural":
							s.natural(a)
						case "cancel":
							s.cancel(a)
							if a.live {
								s.natural(a)
							}
						case "TaskStop":
							s.stop(a)
						case "cap":
							s.pressure(a, 9000, false)
						case "total":
							s.pressure(a, 8192, true)
						case "jobs.Shutdown":
							s.shutdownJobs()
							if a.live {
								s.natural(a)
							}
						case "Bash.Shutdown":
							s.shutdownBash()
							s.natural(a)
						}
						s.finish()
					})
				}
			}
		}
	})
	extra := 12
	if testing.Short() {
		extra = 2
	}
	for i := range extra {
		childA, childB := random.IntN(2) == 0, random.IntN(2) == 0
		order := random.Perm(6)
		fill := 256 + random.IntN(512)
		shutdownFirst := random.IntN(2) == 0
		endC := random.IntN(3)
		t.Run(fmt.Sprintf("random/%#x/%d", seed, i), func(t *testing.T) {
			t.Parallel()
			t.Logf("CIRCUS_CAPTURE_SEED=%#x", seed)
			s := &captureSequence{h: newCaptureHarness(t, 1<<20, 10000)}
			a := s.start("A", "promoted", childA)
			b := s.start("B", "background", childB)
			for _, event := range order {
				switch event {
				case 0:
					s.fill(a, fill)
				case 1:
					s.fill(b, 512)
				case 2:
					s.promote(a)
				case 3:
					s.cancel(b)
				case 4:
					s.fill(a, 512)
				case 5:
					s.fill(b, fill)
				}
			}
			s.natural(a)
			// A's completed shell may still account for live children. B must be the
			// unique largest victim, and releasing it must not consume A's record.
			s.pressure(b, 11000, true)
			modeC := "background"
			if endC == 2 {
				modeC = "foreground"
			}
			c := s.start("C", modeC, true)
			s.fill(c, 1024)
			if shutdownFirst {
				s.shutdownBash()
			} else {
				if a.tracked {
					s.drain(a)
				}
			}
			switch endC {
			case 0:
				s.natural(c)
			case 1:
				s.stop(c)
			case 2:
				s.cancel(c)
			}
			s.shutdownJobs()
			s.shutdownBash()
			s.finish()
		})
	}
}
