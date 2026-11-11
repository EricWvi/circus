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
	"sync"
	"sync/atomic"
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
	total              int64
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
func (s *captureSequence) settlePromote(m *modeledCapture) {
	s.h.promote(m.run)
	m.mode = "background"
	m.phase = captureBackground
	m.keep = true
}
func (s *captureSequence) promote(m *modeledCapture) {
	s.record("promote " + m.run.name)
	s.settlePromote(m)
	s.check()
}
func (s *captureSequence) fill(m *modeledCapture, n int) {
	s.record(fmt.Sprintf("fill %s +%d", m.run.name, n))
	s.h.fill(m.run, n, false)
	m.bytes += n
	s.check()
}
func (s *captureSequence) settleCancel(m *modeledCapture) {
	if m.mode == "background" {
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
}
func (s *captureSequence) cancel(m *modeledCapture) {
	s.record("Cancel " + m.run.name)
	m.run.cancel()
	s.settleCancel(m)
	s.check()
}
func (s *captureSequence) settleNatural(m *modeledCapture) {
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
}
func (s *captureSequence) natural(m *modeledCapture) {
	s.record("shell exit " + m.run.name)
	gate(s.h.t, filepath.Join(m.run.dir, "exit"))
	s.settleNatural(m)
	s.check()
}
func (s *captureSequence) settleStop(m *modeledCapture) {
	m.live = false
	m.phase = captureKilled
	m.code = 143
	m.keep = true
	m.jobState = circusv1.JobEvent_STATE_KILLED
	s.h.job(m.run)
	s.h.released(m.run, m.phase, m.keep)
}
func (s *captureSequence) stop(m *modeledCapture) {
	s.record("TaskStop " + m.run.name)
	s.h.stop(m.run)
	s.settleStop(m)
	s.check()
}
func (s *captureSequence) settlePressure(m *modeledCapture, n int, aggregate bool) {
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
}
func (s *captureSequence) pressure(m *modeledCapture, n int, aggregate bool) {
	s.record(fmt.Sprintf("pressure %s +%d total=%v", m.run.name, n, aggregate))
	if aggregate {
		// The victim is selected from actual descriptor sizes, including exit
		// markers on tracked background files, rather than construction order or
		// requested byte counts alone.
		info, err := m.run.c.out.Stat()
		if err != nil {
			s.h.t.Fatal(err)
		}
		for _, other := range s.models {
			if other == m || (!other.live && !other.tracked) {
				continue
			}
			otherInfo, err := other.run.c.out.Stat()
			if err != nil {
				s.h.t.Fatal(err)
			}
			if otherInfo.Size() >= info.Size() {
				s.h.t.Fatal("total-budget victim is not the unique largest actual writer", m.run.name, other.run.name)
			}
		}
	}
	s.h.fill(m.run, n, true)
	s.settlePressure(m, n, aggregate)
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
		s.h.b.groups.mu.Unlock()
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

type captureWaveOp struct {
	kind    string
	capture *modeledCapture
	n, id   int
	jobID   string
}

// wave triggers one operation per capture behind a common barrier. Fill
// requests also wait at per-request shell gates, proving that their writes are
// simultaneously in flight. Only the parent goroutine settles replies and
// updates the model, so concurrent public operations never race test state.
func (s *captureSequence) wave(ops []captureWaveOp) {
	t := s.h.t
	if len(ops) < 2 {
		t.Fatal("random wave needs at least two captures")
	}
	seen := make(map[*modeledCapture]bool)
	descriptions := make([]string, len(ops))
	for i := range ops {
		op := &ops[i]
		if !op.capture.live || seen[op.capture] {
			t.Fatal("invalid concurrent operation", op.kind, op.capture.run.name)
		}
		seen[op.capture] = true
		descriptions[i] = op.kind + " " + op.capture.run.name
		switch op.kind {
		case "fill":
			op.id = op.capture.run.write
			op.capture.run.write++
			gate(t, filepath.Join(op.capture.run.dir, fmt.Sprintf("hold-%d", op.id)))
			descriptions[i] = fmt.Sprintf("fill %s +%d", op.capture.run.name, op.n)
		case "TaskStop":
			reply := s.h.returned(op.capture.run)
			op.jobID = reply.result.GetBash().JobId
		case "promote", "Cancel", "natural":
		default:
			t.Fatal("unknown wave operation", op.kind)
		}
	}
	s.record("concurrent [" + strings.Join(descriptions, " | ") + "]")

	var ready sync.WaitGroup
	ready.Add(len(ops))
	start := make(chan struct{})
	errs := make(chan error, len(ops))
	for i := range ops {
		op := &ops[i]
		go func() {
			ready.Done()
			<-start
			switch op.kind {
			case "fill":
				errs <- s.h.publishFill(op.capture.run, op.id, op.n)
			case "promote":
				op.capture.run.expiry.expire()
				errs <- nil
			case "Cancel":
				op.capture.run.cancel()
				errs <- nil
			case "natural":
				errs <- os.WriteFile(filepath.Join(op.capture.run.dir, "exit"), nil, 0600)
			case "TaskStop":
				result, err := NewTaskStop(s.h.b.jobs).Run(context.Background(), &circusv1.DeviceCall{SessionId: "review", Tool: &circusv1.DeviceCall_TaskStop{TaskStop: &circusv1.TaskStopRequest{JobId: op.jobID}}})
				if err == nil && result.Output.IsError {
					err = fmt.Errorf("TaskStop rejected %s", op.capture.run.name)
				}
				errs <- err
			}
		}()
	}
	ready.Wait()
	close(start)
	for range ops {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	for _, op := range ops {
		if op.kind == "fill" {
			await(t, func() bool {
				_, err := os.Stat(filepath.Join(op.capture.run.dir, fmt.Sprintf("writing-%d", op.id)))
				return err == nil
			})
		}
	}
	for _, op := range ops {
		if op.kind == "fill" {
			gate(t, filepath.Join(op.capture.run.dir, fmt.Sprintf("go-%d", op.id)))
		}
	}
	for _, op := range ops {
		switch op.kind {
		case "fill":
			s.h.awaitFill(op.capture.run, op.id)
			op.capture.bytes += op.n
		case "promote":
			s.settlePromote(op.capture)
		case "Cancel":
			s.settleCancel(op.capture)
		case "natural":
			s.settleNatural(op.capture)
		case "TaskStop":
			s.settleStop(op.capture)
		}
	}
	s.check()
}

func (s *captureSequence) actualSizes() (map[*modeledCapture]int64, int64) {
	sizes := make(map[*modeledCapture]int64)
	var total int64
	for _, m := range s.models {
		if !m.live && !m.tracked {
			continue
		}
		info, err := m.run.c.out.Stat()
		if err != nil {
			s.h.t.Fatal(err)
		}
		sizes[m] = info.Size()
		total += info.Size()
	}
	return sizes, total
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
						s := &captureSequence{h: newCaptureHarness(t, limit, total), total: total}
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
		scenarioSeed := random.Uint64()
		t.Run(fmt.Sprintf("random/%#x/%d", seed, i), func(t *testing.T) {
			t.Parallel()
			t.Logf("CIRCUS_CAPTURE_SEED=%#x scenario=%#x", seed, scenarioSeed)
			rng := rand.New(rand.NewPCG(scenarioSeed, scenarioSeed^0x9e3779b97f4a7c15))
			s := &captureSequence{h: newCaptureHarness(t, 1<<20, 10000), total: 10000}
			var blockKill atomic.Bool
			killEntered := make(chan struct{}, 1)
			resumeKill := make(chan struct{})
			var resumeOnce sync.Once
			defer resumeOnce.Do(func() { close(resumeKill) })
			s.h.b.effects.delay = func(step string) {
				if step == "kill" && blockKill.CompareAndSwap(true, false) {
					killEntered <- struct{}{}
					<-resumeKill
				}
			}
			models := make([]*modeledCapture, 3)
			initial := make([]captureWaveOp, 3)
			for j := range models {
				models[j] = s.start(string(rune('A'+j)), "promoted", rng.IntN(2) == 0)
				initial[j] = captureWaveOp{kind: "fill", capture: models[j], n: 32 + rng.IntN(160)}
			}
			// Every scenario starts with writes genuinely in flight on all three
			// captures. The following wave can effectively Cancel any of them:
			// none has been promoted yet, so none is a background no-op.
			s.wave(initial)
			cancelTarget := rng.IntN(len(models))
			wave := []captureWaveOp{{kind: "Cancel", capture: models[cancelTarget]}}
			for j, m := range models {
				if j == cancelTarget {
					continue
				}
				kind := "fill"
				if rng.IntN(2) == 0 {
					kind = "promote"
				}
				wave = append(wave, captureWaveOp{kind: kind, capture: m, n: 32 + rng.IntN(160)})
			}
			s.wave(wave)

			nextName := 0
			live := func() []*modeledCapture {
				var result []*modeledCapture
				for _, m := range s.models {
					if m.live {
						result = append(result, m)
					}
				}
				return result
			}
			ensureLive := func() {
				for len(live()) < 3 {
					name := fmt.Sprintf("N%d", nextName)
					nextName++
					s.start(name, "promoted", rng.IntN(2) == 0)
				}
			}
			for range 3 + rng.IntN(3) {
				ensureLive()
				candidates := live()
				permutation := rng.Perm(len(candidates))
				count := 2 + rng.IntN(len(candidates)-1)
				wave = make([]captureWaveOp, 0, count)
				for _, index := range permutation[:count] {
					m := candidates[index]
					choices := []string{"fill", "fill", "fill", "natural"}
					if m.mode == "background" {
						choices = append(choices, "TaskStop")
					} else {
						choices = append(choices, "Cancel")
						if !s.jobsClosed {
							choices = append(choices, "promote")
						}
					}
					kind := choices[rng.IntN(len(choices))]
					wave = append(wave, captureWaveOp{kind: kind, capture: m, n: 16 + rng.IntN(128)})
				}
				s.wave(wave)
				if rng.IntN(10) == 0 && !s.jobsClosed {
					s.shutdownJobs()
				}
			}

			// A final concurrent write wave makes the largest writer seed-dependent.
			// Choose the victim from descriptor sizes, not from A/B/C construction.
			ensureLive()
			candidates := live()
			wave = make([]captureWaveOp, len(candidates))
			for j, m := range candidates {
				wave[j] = captureWaveOp{kind: "fill", capture: m, n: 32 + rng.IntN(160)}
			}
			s.wave(wave)
			childless := false
			for _, m := range live() {
				childless = childless || !m.children
			}
			if !childless {
				s.start(fmt.Sprintf("N%d", nextName), "promoted", false)
				nextName++
			}
			// No captures are started after Bash.Shutdown: a later first use would
			// legitimately create a fresh snapshot instance outside this model's
			// single shutdown lifecycle.
			if !s.closed && rng.IntN(2) == 0 {
				s.shutdownBash()
			}
			candidates = live()
			sizes, total := s.actualSizes()
			var victim *modeledCapture
			var victimSize int64 = -1
			for _, m := range candidates {
				if sizes[m] > victimSize {
					victim, victimSize = m, sizes[m]
				}
			}
			var largestOther int64
			for m, size := range sizes {
				if m != victim && size > largestOther {
					largestOther = size
				}
			}
			if largestOther >= victimSize {
				s.fill(victim, int(largestOther-victimSize)+1+rng.IntN(32))
				sizes, total = s.actualSizes()
				victimSize = sizes[victim]
			}
			for m, size := range sizes {
				if m != victim && size >= victimSize {
					t.Fatal("selected victim is not the unique largest actual writer", victim.run.name, m.run.name)
				}
			}
			if total >= s.total {
				t.Fatal("random setup crossed the aggregate budget before pressure", total)
			}
			// Hold the selected victim inside KILL. A second writer can now exceed
			// the aggregate limit without being selected: pending suppresses another
			// victim. Let that second shell exit before releasing the first KILL so
			// its finished file leaves the running budget deterministically.
			var second *modeledCapture
			for _, m := range live() {
				if m != victim && !m.children {
					second = m
					break
				}
			}
			if second == nil {
				second = s.start(fmt.Sprintf("N%d", nextName), "promoted", false)
				nextName++
			}
			victimBytes := int(s.total-total) + 1
			victimID := victim.run.write
			victim.run.write++
			blockKill.Store(true)
			if err := s.h.publishFill(victim.run, victimID, victimBytes); err != nil {
				t.Fatal(err)
			}
			select {
			case <-killEntered:
			case <-time.After(4 * time.Second):
				t.Fatal("largest actual writer was not selected for aggregate KILL", victim.run.name)
			}
			s.record("pending total victim " + victim.run.name)
			secondBytes := int(s.total) + 1
			s.h.fill(second.run, secondBytes, false)
			second.bytes += secondBytes
			gate(t, filepath.Join(second.run.dir, "exit"))
			s.settleNatural(second)
			resumeOnce.Do(func() { close(resumeKill) })
			s.settlePressure(victim, victimBytes, true)
			s.check()
			s.finish()
		})
	}
}
