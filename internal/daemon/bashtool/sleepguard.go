package bashtool

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var leadingSleep = regexp.MustCompile(`^\s*sleep(?:\s|$)`)
var numericSleep = regexp.MustCompile(`^\s*sleep\s+([0-9]+(?:\.[0-9]+)?)(?:\s*([;&|]+)\s*(.*)|\s*)$`)

// Inferred: inspect only an initial, unwrapped sleep and a unitless number;
// quoted names, assignments, and recursive shell parsing are unmeasured.
func sleepGuard(command string, background bool) string {
	if background {
		return ""
	}
	m := numericSleep.FindStringSubmatch(command)
	if m == nil {
		return ""
	}
	duration, err := strconv.ParseFloat(m[1], 64)
	if err != nil || duration < 25 {
		return ""
	}
	shape := fmt.Sprintf("standalone sleep %s", strconv.FormatFloat(duration, 'f', -1, 64))
	if rest := strings.TrimSpace(m[3]); rest != "" {
		shape = fmt.Sprintf("sleep %s followed by: %s", strconv.FormatFloat(duration, 'f', -1, 64), rest)
	}
	return "<tool_use_error>Blocked: " + shape + ". To wait for a condition, use Monitor with an until-loop (e.g. `until <check>; do sleep 2; done`). To wait for a command you started, use run_in_background: true. Do not chain shorter sleeps to work around this block.</tool_use_error>"
}

// Inferred: native kills the leading sleep shape on timeout and promotes other
// commands, irrespective of stdout/stderr bytes already produced.
func killOnTimeout(command string) bool { return leadingSleep.MatchString(command) }
