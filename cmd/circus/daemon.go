package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sync"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/daemon"
	"github.com/EricWvi/circus/internal/daemon/bashtool"
	"github.com/EricWvi/circus/internal/daemon/jobs"
	"google.golang.org/protobuf/encoding/protojson"
)

func daemonCommand(args []string, in io.Reader, out, errOut io.Writer) error {
	if len(args) == 0 {
		return errors.New("not implemented yet")
	}
	if args[0] != "call" {
		return errors.New("usage: circus daemon call [-wait=true]")
	}
	flags := flag.NewFlagSet("daemon call", flag.ContinueOnError)
	flags.SetOutput(errOut)
	wait := flags.Bool("wait", true, "wait for completed calls")
	if err := flags.Parse(args[1:]); err != nil {
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
