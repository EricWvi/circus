package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestEnumerateOperators(t *testing.T) {
	source := []byte(`package sample
func f(a, b int, err error) error {
	if a == b { a++ }
	if a != b { a-- }
	if a < b || a <= b { println(a + b) }
	if a > b && a >= b { println(a - b) }
	for { if a == 0 { break }; continue }
	defer println(0)
	println(1)
	return err
}
`)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "sample.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, mutant := range enumerate(fset, file, source) {
		got[mutant.Operator]++
	}
	want := map[string]int{
		"comparison_negation": 7,
		"comparison_boundary": 4,
		"logical_swap":        2,
		"arithmetic_swap":     2,
		"incdec_swap":         2,
		"if_negation":         5,
		"integer_increment":   3,
		"call_removal":        4,
		"return_nil":          1,
		"branch_swap":         2,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("operator counts = %v, want %v", got, want)
	}
}

// TestRunIsolatedTimeoutLeavesNoProcesses is a self-check for the sandbox: a
// command that starts background processes and blocks must, once its timeout
// fires, leave no process behind. The sleepers are observed by PID (cmdline
// match plus a nested NSpid, so unrelated host processes never match) and
// then checked to be gone after the run.
func TestRunIsolatedTimeoutLeavesNoProcesses(t *testing.T) {
	sb, err := newSandbox()
	if err != nil {
		t.Skipf("sandbox unavailable: %v", err)
	}
	scratch, err := filepath.Abs(filepath.Join(".tmp", fmt.Sprintf("mutate-test-%d", os.Getpid())))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(scratch, 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(scratch)

	sleepers := make(chan []int, 1)
	go func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if pids := namespaceSleeperPIDs(); len(pids) >= 2 {
				sleepers <- pids
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		sleepers <- nil
	}()

	status, detail := runIsolated(sb, []string{"sh", "-c", "sleep 1000 & sleep 1000 & wait"}, scratch, time.Second)
	if status != "timeout" {
		t.Fatalf("status = %q, want timeout (detail: %s)", status, detail)
	}
	pids := <-sleepers
	if len(pids) < 2 {
		t.Fatal("did not observe the two sleeper processes during the timeout window")
	}
	time.Sleep(200 * time.Millisecond)
	for _, pid := range pids {
		if _, err := os.Stat("/proc/" + strconv.Itoa(pid)); err == nil {
			t.Fatalf("process %d (sleep 1000) survived the sandbox teardown", pid)
		}
	}
}

// namespaceSleeperPIDs returns the host PIDs of `sleep 1000` processes that
// live in a nested PID namespace, i.e. the ones started inside the sandbox.
func namespaceSleeperPIDs() []int {
	entries, _ := filepath.Glob("/proc/[0-9]*")
	var pids []int
	for _, entry := range entries {
		data, err := os.ReadFile(entry + "/cmdline")
		if err != nil || string(data) != "sleep\x001000\x00" {
			continue
		}
		status, err := os.ReadFile(entry + "/status")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(status), "\n") {
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[0] == "NSpid:" {
				if pid, err := strconv.Atoi(filepath.Base(entry)); err == nil {
					pids = append(pids, pid)
				}
			}
		}
	}
	return pids
}
