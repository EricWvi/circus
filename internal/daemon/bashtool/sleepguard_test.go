package bashtool

import (
	"fmt"
	"testing"
)

// The guard/shape matrix in NATIVE-BEHAVIOR.md, including all bisection probes,
// unit discriminators, background bypasses, and timeout shape discriminators.
func TestSleepGuardProbeMatrix(t *testing.T) {
	for _, tc := range []struct {
		command    string
		background bool
		blocked    string
		kill       bool
	}{
		{"sleep 1", false, "", true},
		{"sleep 30", false, "standalone sleep 30", true},
		{"sleep 15", false, "", true},
		{"sleep 22", false, "", true},
		{"sleep 26", false, "standalone sleep 26", true},
		{"sleep 24", false, "", true},
		{"sleep 25", false, "standalone sleep 25", true},
		{"sleep 2.5", false, "", true},
		{"sleep 1m", false, "", true},
		{"sleep 0", false, "", true},
		{"sleep 24 ; echo x", false, "", true},
		{"sleep 25 ; echo x", false, "sleep 25 followed by: echo x", true},
		{"sleep 24 && echo x", false, "", true},
		{"sleep 25 && echo x", false, "sleep 25 followed by: echo x", true},
		{"sleep 24 & echo x", false, "", true},
		{"sleep 25 & echo x", false, "sleep 25 followed by: echo x", true},
		{"echo x; sleep 30", false, "", false},
		{"cd . && sleep 30", false, "", false},
		{"(sleep 30)", false, "", false},
		{"sleep 30 | cat", false, "sleep 30 followed by: cat", true},
		{"timeout 30 sleep 30", false, "", false},
		{"bash -c 'sleep 30'", false, "", false},
		{"sleep 30", true, "", true},
		{"sleep 30; echo x", true, "", true},
		{"sleep 30", false, "standalone sleep 30", true}, // timeout 60000
		{"sleep 5", false, "", true},
		{"echo before; sleep 5", false, "", false},
		{"printf x; sleep 5", false, "", false},
		{"echo x >/dev/null; sleep 5", false, "", false},
		{"true; sleep 5", false, "", false},
		{"sleep 0.5; sleep 5", false, "", true},
		{"echo before >&2; sleep 5", false, "", false},
		{"for i in 1 2 3 4 5; do echo $i; sleep 1; done", false, "", false},
		{"echo before; sleep 5", false, "", false}, // timeout 2000
		{"sleep 0.1; echo x; sleep 5", false, "", true},
		{"sleep 5; echo x", false, "", true},
		{"sleep 0.1 && true && sleep 5", false, "", true},
		{"(sleep 5)", false, "", false},
		{"sleep 25s", false, "", true},
		{"sleep 25m", false, "", true},
		{"sleep 24.999", false, "", true},
		{"sleep 25.0", false, "standalone sleep 25", true},
		{"sleep 70", false, "standalone sleep 70", true},
		{"sleep 30 & echo started", false, "sleep 30 followed by: echo started", true},
	} {
		t.Run(fmt.Sprintf("%s/background=%t", tc.command, tc.background), func(t *testing.T) {
			t.Parallel()
			want := ""
			if tc.blocked != "" {
				want = "<tool_use_error>Blocked: " + tc.blocked + ". To wait for a condition, use Monitor with an until-loop (e.g. `until <check>; do sleep 2; done`). To wait for a command you started, use run_in_background: true. Do not chain shorter sleeps to work around this block.</tool_use_error>"
			}
			if got := sleepGuard(tc.command, tc.background); got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
			if got := killOnTimeout(tc.command); got != tc.kill {
				t.Fatalf("kill on timeout: %v, want %v", got, tc.kill)
			}
		})
	}
}
