package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/EricWvi/circus/internal/selfupdate"
)

func TestRunsUpdatedBinary(t *testing.T) {
	dir := t.TempDir()
	updated := filepath.Join(dir, "circus")
	other := filepath.Join(dir, "other-circus")
	symlink := filepath.Join(dir, "service-circus")
	for _, path := range []string{updated, other} {
		if err := os.WriteFile(path, []byte("circus"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, updated, program string
		want                   bool
	}{
		{"direct path", updated, updated, true},
		{"another install", updated, other, false},
		{"no service", updated, "", false},
		{"missing service executable", updated, symlink, false},
		{"no update", "", updated, false},
		{"missing updated file", symlink, updated, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := runsUpdatedBinary(tc.updated, tc.program); got != tc.want {
				t.Errorf("runsUpdatedBinary(%q, %q) = %t, want %t", tc.updated, tc.program, got, tc.want)
			}
		})
	}
	if err := os.Symlink(updated, symlink); err != nil {
		t.Fatal(err)
	}
	newBinary := filepath.Join(dir, "new-circus")
	if err := os.WriteFile(newBinary, []byte("new circus"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newBinary, updated); err != nil {
		t.Fatal(err)
	}
	if !runsUpdatedBinary(updated, symlink) {
		t.Error("service symlink should still match the replaced executable")
	}
}

func TestUpdateCommandRestartsMatchingService(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("uses a systemctl fake")
	}
	for _, tc := range []struct {
		name, program, notice string
		active, managerError  bool
		wantTryRestart        bool
		selfStopped, noBus    bool
	}{
		{name: "running", program: "linked", active: true, wantTryRestart: true},
		{name: "stopped", program: "linked"},
		{name: "unreachable manager", program: "linked", noBus: true, notice: "cannot reach the user bus"},
		{name: "stopped itself", program: "linked", selfStopped: true},
		{name: "not installed"},
		{name: "another install", program: "other", active: true, notice: "runs another binary"},
		{name: "removed binary", program: "removed", active: true, notice: "points to a removed binary"},
		{name: "restart fails", program: "linked", active: true, managerError: true, wantTryRestart: true, notice: "warning: could not restart"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			config := filepath.Join(dir, "config")
			t.Setenv("XDG_CONFIG_HOME", config)
			t.Setenv("PATH", dir)
			t.Setenv("UPDATE_TEST_DIR", dir)
			write := func(path, text string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(text), 0700); err != nil {
					t.Fatal(err)
				}
			}
			updated := filepath.Join(dir, "circus")
			link := filepath.Join(dir, "linked")
			write(updated, "old executable")
			write(filepath.Join(dir, "other"), "other executable")
			if err := os.Symlink(updated, link); err != nil {
				t.Fatal(err)
			}
			if tc.program != "" {
				unit := filepath.Join(config, "systemd", "user", "circus-daemon.service")
				if err := os.MkdirAll(filepath.Dir(unit), 0700); err != nil {
					t.Fatal(err)
				}
				write(unit, fmt.Sprintf("[Service]\nExecStart=%q daemon --service\n", filepath.Join(dir, tc.program)))
			}
			if tc.active {
				write(filepath.Join(dir, "active"), "")
			}
			if tc.selfStopped {
				write(filepath.Join(dir, "self-stopped"), "")
			}
			if tc.noBus {
				write(filepath.Join(dir, "no-bus"), "")
			}
			if tc.managerError {
				write(filepath.Join(dir, "manager-error"), "")
			}
			write(filepath.Join(dir, "systemctl"), `#!/bin/sh
dir=$UPDATE_TEST_DIR
printf '%s\n' "$*" >> "$dir/calls"
case "$2" in
    show)
        if [ -f "$dir/no-bus" ]; then
            printf 'Failed to connect to bus: No medium found\n' >&2
            exit 1
        fi
        if [ -f "$dir/active" ]; then
            printf 'ActiveState=active\n'
        elif [ -f "$dir/self-stopped" ]; then
            printf 'ActiveState=inactive\nExecMainCode=exited\nExecMainStatus=78\n'
        else
            printf 'ActiveState=inactive\nExecMainCode=1\nExecMainStatus=0\n'
        fi
        ;;
    try-restart)
        if [ -f "$dir/manager-error" ]; then
            printf 'Test manager error\n' >&2
            exit 1
        fi
        if [ -f "$dir/active" ]; then : > "$dir/restarted"; fi
        ;;
    restart|start)
        : > "$dir/active"
        : > "$dir/restarted"
        ;;
    *) exit 1 ;;
esac
`)
			fakeSelfUpdate(t, selfupdate.Plan{Version: "v9.9.9", Path: updated, Apply: func(context.Context) (selfupdate.Result, error) {
				next := filepath.Join(dir, "next")
				write(next, "new executable")
				return selfupdate.Result{Message: "Updated circus", ReplacedPath: updated, Version: "v9.9.9"}, os.Rename(next, updated)
			}})
			var out, errOut bytes.Buffer
			if err := updateCommand(nil, &out, &errOut); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "Updated circus") {
				t.Errorf("update output = %q", out.String())
			}
			if tc.notice == "" && errOut.Len() != 0 || tc.notice != "" && !strings.Contains(errOut.String(), tc.notice) {
				t.Errorf("update diagnostics = %q, want %q", errOut.String(), tc.notice)
			}
			calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
			if strings.Contains(string(calls), "--user restart") || strings.Contains(string(calls), "--user try-restart") != tc.wantTryRestart {
				t.Errorf("manager calls = %q", calls)
			}
			if strings.Contains(string(calls), "--user start") != tc.selfStopped {
				t.Errorf("manager calls = %q, want start: %t", calls, tc.selfStopped)
			}
			if got := strings.Contains(out.String(), "the circus daemon service is installed but not running; run `circus daemon start`"); got != (tc.name == "stopped") {
				t.Errorf("update output = %q", out.String())
			}
			if tc.program != "linked" && len(calls) != 0 {
				t.Errorf("found manager calls for an unrelated service: %q", calls)
			}
			_, err := os.Stat(filepath.Join(dir, "restarted"))
			wantRestarted := tc.selfStopped || tc.wantTryRestart && !tc.managerError
			if (err == nil) != wantRestarted || strings.Contains(out.String(), "Asked the service manager to restart the circus device daemon service.") != wantRestarted {
				t.Errorf("restart state = %v, output = %q", err, out.String())
			}
			_, err = os.Stat(filepath.Join(dir, "active"))
			if (err == nil) != (tc.active || tc.selfStopped) {
				t.Errorf("update changed whether the service is active: %v", err)
			}
		})
	}
}

// fakeSelfUpdate makes `circus update` find plan.
func fakeSelfUpdate(t *testing.T, plan selfupdate.Plan) {
	t.Helper()
	old := checkSelfUpdate
	t.Cleanup(func() { checkSelfUpdate = old })
	checkSelfUpdate = func(context.Context, string, bool, io.Writer) (selfupdate.Plan, error) { return plan, nil }
}
