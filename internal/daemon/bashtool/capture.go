package bashtool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/EricWvi/circus/internal/daemon/jobs"
)

// captureState separates the command's phase from the driver responsible for
// completing it. Terminal shell phases can still await foreground formatting;
// tracked groups retain the same capture, descriptor and budget record.
type capturePhase uint8

const (
	captureForeground capturePhase = iota
	captureBackground
	captureExited
	captureKilled
	captureStopped
	captureTracked
	captureEmpty
)

type captureOwner uint8

const (
	ownerForeground captureOwner = iota
	ownerBackground
	ownerTracker
	ownerReleased
)

type captureState struct {
	phase capturePhase
	owner captureOwner
}
type outputDisposition uint8

const (
	deleteOutput outputDisposition = iota
	retainOutput
)

// effects separates fallible operations from scheduling-only observation.
// Hooks are configured before starting commands. after can simulate a Close
// that reports an error after closing the descriptor, as real Close may do.
type effects struct {
	waitAlive func(int) bool
	alive     func(int) bool
	before    func(string) error
	after     func(string, error) error
	delay     func(string)
	started   func(*capture)
}

func (e *effects) attempt(step string, action func() error) error {
	if e != nil && e.before != nil {
		if err := e.before(step); err != nil {
			return err
		}
	}
	err := action()
	if e != nil && e.after != nil {
		return e.after(step, err)
	}
	return err
}
func (e *effects) pause(step string) {
	if e != nil && e.delay != nil {
		e.delay(step)
	}
}

// capture is the sole resource owner from Start to group emptiness. Drivers
// transfer through state under mu; they never reopen or re-register its file.
// Only the current driver consumes waited or performs finishing side effects.
type capture struct {
	mu           sync.Mutex
	state        captureState
	bash         *Bash
	cmd          *exec.Cmd
	waited       <-chan processOutcome
	outcome      *processOutcome
	out          *os.File
	cwdPath      string
	disposition  outputDisposition
	released     chan struct{}
	job          *backgroundJob
	lease        *snapshotLease
	shellRelease sync.Once
	tracking     *trackedGroup
}

func (b *Bash) startCapture(reqCmd *exec.Cmd, out *os.File, cwdPath string, lease *snapshotLease) (*capture, error) {
	c := &capture{bash: b, cmd: reqCmd, out: out, cwdPath: cwdPath, released: make(chan struct{}), state: captureState{captureForeground, ownerForeground}, lease: lease.retain()}
	reqCmd.Stdout, reqCmd.Stderr = out, out
	if err := b.effects.attempt("start", reqCmd.Start); err != nil {
		c.move(captureExited, ownerForeground)
		c.release(ownerForeground, deleteOutput)
		return nil, err
	}
	b.effects.pause("budget-start")
	b.outputs.outputBudget.start(out)
	waited := make(chan processOutcome, 1)
	c.waited = waited
	go func() {
		b.effects.pause("wait-shell")
		err := reqCmd.Wait()
		b.effects.pause("shell-exited")
		waited <- processOutcome{code: exitCode(reqCmd), err: err}
	}()
	if b.effects.started != nil {
		b.effects.started(c)
	}
	return c, nil
}

func (c *capture) snapshotState() captureState { c.mu.Lock(); defer c.mu.Unlock(); return c.state }
func (c *capture) move(phase capturePhase, owner captureOwner) {
	c.mu.Lock()
	c.state = captureState{phase, owner}
	c.mu.Unlock()
}
func (c *capture) pgid() int { return c.cmd.Process.Pid }
func (c *capture) groupAlive() bool {
	if c.bash.effects.alive != nil {
		return c.bash.effects.alive(c.pgid())
	}
	return groupAlive(c.pgid())
}

func (c *capture) terminate(kill bool) processOutcome {
	var outcome processOutcome
	step := "stop"
	if kill {
		step = "kill"
	}
	c.bash.effects.pause(step)
	if kill {
		outcome = killProcess(c.cmd, c.waited)
	} else {
		outcome = stopProcess(c.cmd, c.waited)
	}

	if kill {
		c.waitEmpty()
	}
	c.outcome = &outcome
	return outcome
}
func (c *capture) waitEmpty() {
	c.bash.effects.pause("wait")
	probe := groupAlive
	if c.bash.effects.waitAlive != nil {
		probe = c.bash.effects.waitAlive
	}
	waitGroupEmptyWith(c.pgid(), groupExitWait, probe)
}
func (c *capture) killLeftovers() {
	c.bash.effects.pause("kill-group")
	killGroup(c.pgid())
	c.waitEmpty()
}
func (c *capture) stopLeftovers() {
	c.bash.effects.pause("stop-group")
	stopGroup(c.pgid())
	c.waitEmpty()
}
func (c *capture) writeLimit(message string) error {
	return c.bash.effects.attempt("explain", func() error { return appendOutputLimit(c.out, message) })
}
func (c *capture) finishBudget() {
	c.bash.effects.pause("budget-finish")
	c.bash.outputs.outputBudget.finish(c.out.Name())
}

func (c *capture) beginBackground() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != (captureState{captureForeground, ownerForeground}) {
		return errors.New("capture is not foreground")
	}
	c.state = captureState{captureBackground, ownerBackground}
	return nil
}

func captureOutcomePhase(outcome processOutcome) capturePhase {
	if !outcome.terminationRequested {
		return captureExited
	}
	if errors.Is(outcome.err, jobs.ErrKilled) {
		return captureKilled
	}
	return captureStopped
}

// finishShell is shared by foreground and background drivers. A natural
// aggregate victim that is already empty retains its actual exit and no cap
// diagnostic. A live leftover group retains both its budget and file lease.
func (c *capture) finishShell(outcome processOutcome, message string, background bool) (processOutcome, bool, error) {
	c.bash.effects.pause("finish-shell")
	c.outcome = &outcome
	alive := c.groupAlive()
	message = outputMessageAfterExit(message, outcome.terminationRequested, alive)
	if message != "" || !alive {
		c.finishBudget()
	}
	if message == "" {
		message = outputMessageAfterExit(c.bash.outputs.outputStopMessage(c.out), outcome.terminationRequested, alive)
		if message != "" {
			c.killLeftovers()
			c.finishBudget()
		}
	}
	phase := captureOutcomePhase(outcome)
	if message != "" {
		phase = captureKilled
		if background {
			outcome.err = errors.Join(outcome.err, jobs.ErrKilled)
		} else if outcome.code == 0 {
			outcome.code = 137
		}
	}
	c.move(phase, c.snapshotState().owner)
	c.outcome = &outcome
	var err error
	if background {
		err = c.bash.effects.attempt("exit-marker", func() error { _, err := fmt.Fprintf(c.out, "\n[exited with code %d]\n", outcome.code); return err })
	}
	if message != "" {
		err = errors.Join(err, c.writeLimit(message))
	}
	leftovers := message == "" && c.groupAlive()
	if background {
		leftovers = leftovers && !outcome.terminationRequested
	} else {
		leftovers = leftovers && outcome.err != context.Canceled && outcome.err != context.DeadlineExceeded
	}
	return outcome, leftovers, err
}

// release runs exactly once, by the driver named in state. The descriptor is
// closed before unlinking and before releasing its active/budget record.
func (c *capture) release(owner captureOwner, disposition outputDisposition) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state.owner != owner {
		return fmt.Errorf("capture release by driver %d; owner is %d", owner, c.state.owner)
	}
	c.state.owner = ownerReleased
	if c.state.phase == captureTracked {
		c.state.phase = captureEmpty
	}
	closeErr := c.bash.effects.attempt("close", c.out.Close)
	// Foreground has always ignored deferred Close errors; visible background
	// jobs include them in Wait. Unlink failures still fail foreground results.
	var err error
	if owner == ownerBackground {
		err = closeErr
	}
	if disposition == deleteOutput {
		removeErr := c.bash.effects.attempt("remove", func() error {
			err := os.Remove(c.out.Name())
			if os.IsNotExist(err) {
				return nil
			}
			return err
		})
		if owner != ownerTracker {
			err = errors.Join(err, removeErr)
		}
	}
	c.bash.outputs.releaseOutput(c.out.Name())
	c.releaseShell()
	close(c.released)
	return err
}

// abandonForeground is also the rollback for failed registration, preview or
// formatting. Background and tracker drivers are exempt by state, not flags.
func (c *capture) abandonForeground() {
	if c.snapshotState().owner != ownerForeground {
		return
	}
	if c.waited != nil {
		if c.outcome == nil {
			outcome := c.terminate(false)
			c.move(captureOutcomePhase(outcome), ownerForeground)
		} else {
			c.stopLeftovers()
		}
	}
	_ = c.release(ownerForeground, c.disposition)
}
func (c *capture) releaseShell() {
	c.shellRelease.Do(func() {
		_ = os.Remove(c.cwdPath)
		c.lease.release()
	})
}
func (c *capture) track(disposition outputDisposition) error {
	c.disposition = disposition
	c.move(captureTracked, ownerTracker)
	return c.bash.trackCapture(c)
}

// reject waits for a launched job before reclaiming its resources. A shell
// may already have handed children to the tracker while Start was rejected.
func (c *capture) reject() {
	if c.job != nil {
		_ = c.job.Stop()
		_, _ = c.job.Wait()
	}
	if c.snapshotState().owner == ownerTracker {
		c.bash.stopTracked(c)
		<-c.released
	}
	c.abandonForeground()
	_ = c.bash.effects.attempt("remove", func() error {
		err := os.Remove(c.out.Name())
		if os.IsNotExist(err) {
			return nil
		}
		return err
	})
}

type backgroundJob struct {
	done   chan struct{}
	cancel context.CancelFunc
	code   int
	err    error
	once   sync.Once
}

func (j *backgroundJob) Wait() (int, error) { <-j.done; return j.code, j.err }
func (j *backgroundJob) Stop() error        { j.once.Do(j.cancel); <-j.done; return nil }

func (c *capture) background(lifetime time.Duration) (*backgroundJob, error) {
	b := c.bash
	if err := b.effects.attempt("promote", c.beginBackground); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), lifetime)
	job := &backgroundJob{done: make(chan struct{}), cancel: cancel}
	c.job = job
	go func() {
		defer close(job.done)
		defer cancel()
		defer c.releaseShell()
		var outcome processOutcome
		tick := time.NewTicker(outputPollInterval)
		defer tick.Stop()
		capMessage := ""
	waitBackground:
		for {
			select {
			case outcome = <-c.waited:
				break waitBackground
			case <-tick.C:
				capMessage = b.outputs.outputStopMessage(c.out)
				if capMessage == "" {
					continue
				}
			case <-ctx.Done():
			}
			outcome = c.terminate(capMessage != "")
			if errors.Is(ctx.Err(), context.DeadlineExceeded) && outcome.terminationRequested {
				outcome.err = errors.Join(outcome.err, jobs.ErrKilled)
			}
			break
		}
		outcome, leftovers, err := c.finishShell(outcome, capMessage, true)
		job.code, job.err = outcome.code, errors.Join(outcome.err, err)
		var exitErr *exec.ExitError
		if errors.As(job.err, &exitErr) && !errors.Is(job.err, jobs.ErrKilled) {
			job.err = err
		}
		if leftovers {
			job.err = errors.Join(job.err, c.track(retainOutput))
		} else {
			job.err = errors.Join(job.err, c.release(ownerBackground, retainOutput))
		}
	}()
	return job, nil
}
