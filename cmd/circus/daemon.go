package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/credfile"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	daemonlink "github.com/EricWvi/circus/internal/daemon/link"
	"github.com/EricWvi/circus/internal/daemonlock"
	"github.com/EricWvi/circus/internal/userservice"
	"google.golang.org/protobuf/encoding/protojson"
)

const daemonUsage = `usage: circus daemon [--credentials path] [--service]
       circus daemon call [-wait=true]
       circus daemon install [--credentials path]
       circus daemon <uninstall|start|stop|restart|status>`

func daemonCommand(args []string, in io.Reader, out, errOut io.Writer) error {
	if len(args) > 0 && args[0] == "call" {
		return daemonCallCommand(args[1:], in, out, errOut)
	}
	command, credentials, service, err := parseDaemonArgs(args, errOut)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	switch command {
	case "install":
		if err := userservice.InstallDaemon(credentials, out); err != nil {
			return err
		}
		st, err := userservice.Daemon.GetAfterInstall()
		if st.Running && err == nil {
			fmt.Fprintln(out, "✓ Installed and started the circus device daemon service.")
		} else {
			fmt.Fprintln(out, "✓ Installed the circus device daemon service.")
			if err != nil {
				fmt.Fprintf(errOut, "WARNING: Could not confirm that the circus daemon service is running: %v. See the logs: %s\n", err, st.Logs)
			} else {
				fmt.Fprintf(errOut, "WARNING: The circus daemon service is not running. See the logs: %s\n", st.Logs)
			}
		}
		for _, warning := range st.Advice {
			fmt.Fprintln(errOut, "WARNING:", warning)
		}
		printDaemonLocation(out, st)
		return nil
	case "uninstall":
		return daemonAction(out, userservice.Daemon.Uninstall, "Removed")
	case "start":
		return daemonAction(out, userservice.Daemon.Start, "Started")
	case "stop":
		return daemonAction(out, userservice.Daemon.Stop, "Stopped")
	case "restart":
		return daemonAction(out, userservice.Daemon.Restart, "Restarted")
	case "status":
		return daemonStatus(out)
	default:
		return daemonRun(credentials, service, errOut)
	}
}

func parseDaemonArgs(args []string, errOut io.Writer) (command, credentials string, service bool, err error) {
	name := "daemon"
	usage := daemonUsage
	if len(args) > 0 {
		switch args[0] {
		case "install", "uninstall", "start", "stop", "restart", "status":
			command, args = args[0], args[1:]
			name += " " + command
			usage = "usage: circus " + name
			if command == "install" {
				usage += " [--credentials path]"
			}
		}
	}
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(errOut)
	flags.Usage = func() {
		fmt.Fprintln(errOut, usage)
		if command == "" {
			fmt.Fprintln(errOut, "\nRun the outbound daemon client until SIGINT or SIGTERM.")
		}
		flags.PrintDefaults()
	}
	if command == "" || command == "install" {
		flags.StringVar(&credentials, "credentials", "", "daemon credential file (default: circus/daemon.json in the config directory)")
	}
	if command == "" {
		flags.BoolVar(&service, "service", false, "used by the installed service")
	}
	if err := flags.Parse(args); err != nil {
		return "", "", false, err
	}
	if flags.NArg() != 0 {
		return "", "", false, errors.New(usage)
	}
	if command == "install" && flags.NFlag() > 0 && credentials == "" {
		return "", "", false, errors.New("--credentials requires a path")
	}
	return command, credentials, service, nil
}

func daemonAction(out io.Writer, action func() error, verb string) error {
	if err := action(); err != nil {
		return err
	}
	_, err := fmt.Fprintln(out, "✓", verb, "the circus device daemon service.")
	return err
}

func daemonStatus(out io.Writer) error {
	st, err := userservice.Daemon.Get()
	if err != nil {
		return err
	}
	if !st.Installed {
		fmt.Fprintln(out, "The circus device daemon service is not installed. Run `circus daemon install`.")
		return nil
	}
	fmt.Fprintf(out, "The circus device daemon service is installed and %s.\n", runState(st))
	printServiceState(out, st)
	printDaemonLocation(out, st)
	printServiceAdvice(out, st)
	return nil
}

func runState(st userservice.Status) string {
	state := "not running"
	if st.Running {
		state = "running"
	}
	if st.PID > 0 {
		state += fmt.Sprintf(" (PID %d)", st.PID)
	}
	return state
}

func printServiceState(out io.Writer, st userservice.Status) {
	if st.State != "" {
		fmt.Fprintln(out, "  State     ", st.State)
	}
	if st.LastExit != "" {
		fmt.Fprintln(out, "  Last exit ", st.LastExit)
	}
	start := "only when started"
	if st.Enabled {
		start = "at login"
		if st.AtBoot {
			start = "at boot or login"
		}
	}
	fmt.Fprintln(out, "  Starts    ", start)
}

func printServiceAdvice(out io.Writer, st userservice.Status) {
	if st.StoppedAfterExit {
		fmt.Fprintf(out, "See the logs for why it stopped: %s\n", st.Logs)
	}
	for _, advice := range st.Advice {
		fmt.Fprintln(out, advice)
	}
}

func printDaemonLocation(out io.Writer, st userservice.Status) {
	fmt.Fprintln(out, "  File      ", st.File)
	fmt.Fprintln(out, "  Logs      ", st.Logs)
}

func daemonCallCommand(args []string, in io.Reader, out, errOut io.Writer) error {
	flags := flag.NewFlagSet("daemon call", flag.ContinueOnError)
	flags.SetOutput(errOut)
	wait := flags.Bool("wait", true, "wait for completed calls")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("usage: circus daemon call [-wait=true]")
	}
	jobs := jobs.NewManager(jobs.Options{})
	defer func() { _ = jobs.Shutdown(context.Background()) }()
	tools := daemon.DefaultTools(jobs, bashtool.Options{})
	for _, tool := range tools {
		if warmer, ok := tool.(interface{ Warm() }); ok {
			warmer.Warm()
		}
	}
	return daemonCall(in, out, tools, *wait)
}

// completionQueue serializes result notifications without blocking a tool on stdout.
// Requests may arrive before or after their completion callback (including retries).
type completionQueue struct {
	mu        sync.Mutex
	ready     *sync.Cond
	items     []*circusv1.CallStatus
	results   map[string]*circusv1.DeviceResult
	pending   map[string]int
	inputDone bool
}

func newCompletionQueue() *completionQueue {
	q := &completionQueue{results: make(map[string]*circusv1.DeviceResult), pending: make(map[string]int)}
	q.ready = sync.NewCond(&q.mu)
	return q
}

func (q *completionQueue) enqueue(result *circusv1.DeviceResult) {
	q.items = append(q.items, &circusv1.CallStatus{CallId: result.CallId, State: circusv1.CallStatus_STATE_COMPLETED, Result: result})
}

func (q *completionQueue) completed(result *circusv1.DeviceResult) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.results[result.CallId] = result
	for range q.pending[result.CallId] {
		q.enqueue(result)
	}
	delete(q.pending, result.CallId)
	q.ready.Signal()
}

func (q *completionQueue) requested(id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if result := q.results[id]; result != nil {
		q.enqueue(result)
	} else {
		q.pending[id]++
	}
	q.ready.Signal()
}

func (q *completionQueue) next() (*circusv1.CallStatus, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 {
		if q.inputDone && len(q.pending) == 0 {
			return nil, false
		}
		q.ready.Wait()
	}
	item := q.items[0]
	q.items[0] = nil
	q.items = q.items[1:]
	return item, true
}

// daemonCall is the testable stream harness core. Completion callbacks enqueue
// results; a single writer emits them while stdin continues to accept calls.
func daemonCall(in io.Reader, out io.Writer, tools []daemon.Tool, wait bool) error {
	queue := newCompletionQueue()
	opts := daemon.Options{Tools: tools}
	if wait {
		opts.OnResult = queue.completed
	}
	dispatcher, err := daemon.NewDispatcher(opts)
	if err != nil {
		return err
	}
	defer func() { _ = dispatcher.Shutdown(context.Background()) }()
	write := func(status *circusv1.CallStatus) error {
		data, err := protojson.Marshal(status)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(data))
		return err
	}
	written := make(chan error, 1)
	if wait {
		go func() {
			var outputErr error
			for {
				status, ok := queue.next()
				if !ok {
					written <- outputErr
					return
				}
				if outputErr == nil {
					outputErr = write(status)
				}
			}
		}()
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 16*1024*1024)
	var streamErr error
	for scanner.Scan() {
		call := new(circusv1.DeviceCall)
		if err := protojson.Unmarshal(scanner.Bytes(), call); err != nil {
			streamErr = err
			break
		}
		status, err := dispatcher.Submit(call)
		if err != nil {
			streamErr = err
			break
		}
		if wait {
			queue.requested(call.CallId)
		} else if err := write(status); err != nil {
			streamErr = err
			break
		}
	}
	if wait {
		queue.mu.Lock()
		queue.inputDone = true
		queue.ready.Signal()
		queue.mu.Unlock()
		if err := <-written; streamErr == nil {
			streamErr = err
		}
	}
	if streamErr != nil {
		return streamErr
	}
	return scanner.Err()
}

// daemonRun reads role credentials and runs until SIGINT/SIGTERM.
func daemonRun(path string, service bool, errOut io.Writer) error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	mode := daemonlock.Foreground
	if service {
		mode = daemonlock.Service
	}
	lock, err := acquireDaemonLock(ctx, mode)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		if service {
			return logServiceExit(errOut, err)
		}
		return err
	}
	defer lock.Close()

	if path == "" {
		path, err = credfile.Path(credfile.RoleDaemon)
	}
	if err != nil {
		if service {
			return logServiceExit(errOut, err)
		}
		return err
	}
	file, err := credfile.Load(path)
	if err != nil {
		if service {
			return logServiceExit(errOut, fmt.Errorf("load daemon credentials at %q: %w", path, err))
		}
		return err
	}
	client, err := daemonlink.New(daemonlink.Options{Credentials: file, ClientVersion: version})
	if err != nil {
		if service {
			return logServiceExit(errOut, err)
		}
		return err
	}
	err = client.Run(ctx)
	if service && daemonlink.IsTerminal(err) {
		return logServiceExit(errOut, err)
	}
	return err
}

func acquireDaemonLock(ctx context.Context, mode daemonlock.Mode) (*daemonlock.Lock, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		lock, err := daemonlock.Acquire(mode)
		var held *daemonlock.HeldError
		if mode != daemonlock.Service || !errors.As(err, &held) || held.Mode != daemonlock.Service {
			return lock, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(100 * time.Millisecond):
		}
	}
}

var errDaemonServiceStopped = errors.New("daemon service stopped itself")

func logServiceExit(errOut io.Writer, err error) error {
	message := strings.NewReplacer("\r", "\\r", "\n", "\\n").Replace(err.Error())
	fmt.Fprintln(errOut, "circus daemon service stopped:", message)
	if runtime.GOOS == "linux" {
		return errDaemonServiceStopped
	}
	return nil
}
