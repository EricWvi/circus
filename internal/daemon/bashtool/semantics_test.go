package bashtool

import (
	"strings"
	"syscall"
	"testing"
)

func TestLastCommandName(t *testing.T) {
	for command, want := range map[string]string{
		"cd missing && grep x":             "grep",
		"false | grep x":                   "grep",
		"cd missing &&grep x":              "grep",
		"cd missing\ngrep -q x /dev/null":  "grep",
		"   grep -q x /dev/null":           "grep",
		"cd missing && grep x;":            "grep",
		"cd missing && grep x # a && b":    "grep",
		"cd missing && grep 'x;y'":         "grep",
		"cd missing && grep x 2>&1":        "grep",
		"cd missing && grep x &>/dev/null": "grep",
		"cd missing && grep x |& cat":      "cat",
		"cd missing && grep x | head":      "head",
		"(cd missing && grep x)":           ")",
		"cd missing && ( grep x )":         ")",
		"{ cd missing && grep x; }":        "}",
		"cd missing && $(echo grep) x":     "$(echo",
		"cd missing && echo '&& grep'":     "echo",
		`cd missing && echo "a; grep"`:     "echo",
		`cd missing && \grep x`:            `\grep`,
		"cd missing && /usr/bin/grep x":    "/usr/bin/grep",
		"LC_ALL=C grep -q x /dev/null":     "LC_ALL=C",
		"grep x & echo y":                  "echo",
		"":                                 "",
	} {
		if got := lastCommandName(command); got != want {
			t.Errorf("lastCommandName(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestExitFailed(t *testing.T) {
	for _, name := range strings.Fields("grep egrep fgrep rg find diff test [") {
		if exitFailed("cd missing && "+name+" x", 1) {
			t.Errorf("%s status 1 should not fail", name)
		}
		if !exitFailed("cd missing && "+name+" x", 2) {
			t.Errorf("%s status 2 should fail", name)
		}
	}
	for _, name := range strings.Fields("zgrep ugrep cmp pgrep which expr ls GREP") {
		if !exitFailed("cd missing && "+name+" x", 1) {
			t.Errorf("%s status 1 should fail", name)
		}
	}
	if exitFailed("grep x", 0) {
		t.Error("status 0 failed")
	}
}

func TestSignalStatus(t *testing.T) {
	if got := signalStatus(syscall.SIGTERM); got != 144 {
		t.Errorf("SIGTERM status %d", got)
	}
	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGHUP, syscall.SIGPIPE, syscall.SIGKILL} {
		if got := signalStatus(signal); got != 1 {
			t.Errorf("%v status %d", signal, got)
		}
	}
}

func TestOutputPreview(t *testing.T) {
	repeat := strings.Repeat
	for _, test := range []struct {
		name, text, want string
		more             bool
	}{
		{"short", "abc", "abc", false},
		{"exact limit", repeat("a", 2000), repeat("a", 2000), false},
		{"no newline", repeat("a", 2001), repeat("a", 2000), true},
		{"newline at unit 1000 kept", repeat("a", 1000) + "\n" + repeat("b", 2000), repeat("a", 1000) + "\n" + repeat("b", 999), true},
		{"newline at unit 1001 cuts", repeat("a", 1001) + "\n" + repeat("b", 2000), repeat("a", 1001), true},
		{"newline at unit 2000 outside", repeat("a", 2000) + "\n" + repeat("b", 10), repeat("a", 2000), true},
		{"previous newline kept", repeat("abc\n\n", 401), repeat("abc\n\n", 399) + "abc\n", true},
		{"surrogate pair not split", "a" + repeat("😀", 1000), "a" + repeat("😀", 999), true},
	} {
		got, more := outputPreview(test.text)
		if got != test.want || more != test.more {
			t.Errorf("%s: got %d bytes (more %v), want %d bytes (more %v)", test.name, len(got), more, len(test.want), test.more)
		}
	}
}

func TestLastCommandNameExpansionsAndHeredocs(t *testing.T) {
	for command, want := range map[string]string{
		"test $(echo 1) = 2":                         "test",
		"[ $(echo 1) -gt 5 ]":                        "[",
		"test $((1+1)) -eq 3":                        "test",
		"diff <(echo a) <(echo b)":                   "diff",
		"grep -q x /dev/null 2> >(cat)":              "grep",
		"test $(echo \"a)\" | (cat)) = b":            "test",
		"grep -q zzz <<EOF\nabc\nEOF":                "grep",
		"grep -q zzz <<-'EOF'\n\tabc;def\n\tEOF":     "grep",
		"cat <<EOF\nit's\nEOF\ngrep -q x /dev/null":  "grep",
		"grep -q zzz <<EOF 2>/dev/null\nabc\nEOF":    "grep",
		"cat <<EOF | grep -q zzz\nabc\nEOF":          "cat",
		"cat <<EOF; grep -q zzz /dev/null\nabc\nEOF": "cat",
		"grep -q zzz <<EOF\nEOF x\nEOF":              "grep",
		"grep -q zzz <<EOF\nabc":                     "grep",
		"cat <<A <<B\na\nA\nb\nB\ngrep x":            "",
		"grep -q zzz <<< abc":                        "grep",
		"echo $'a\\';'; grep -q x /dev/null":         "grep",
		"cd missing && \\\ngrep -q x /dev/null":      "grep",
		"echo a#b; grep -q x /dev/null":              "grep",
	} {
		if got := lastCommandName(command); got != want {
			t.Errorf("lastCommandName(%q) = %q, want %q", command, got, want)
		}
	}
}

func TestLastCommandNameHeredocLineAndSubstitutions(t *testing.T) {
	for command, want := range map[string]string{
		"grep -q zzz <<EOF && echo hit\nabc\nEOF":           "grep",
		"grep -q zzz <<EOF || false\nabc\nEOF":              "grep",
		"true <<EOF | grep -q zzz\nabc\nEOF":                "true",
		"true <<EOF & grep -q zzz /dev/null\nabc\nEOF":      "true",
		"true <<EOF && echo\nabc\nEOF\ngrep -q x /dev/null": "grep",
		"true <<EOF && false\ngrep -q x /dev/null\nEOF":     "true",
		"grep -q zzz <<EOF":                                 "grep",
		"cat <<A\na\nA\ncat <<B\nb\nB\ngrep -q x /dev/null": "grep",
		"cat <<-A <<B\n\ta\n\tA\nb\nB\ngrep -q x /dev/null": "",
		"grep -q zzz <<A 3<<B\na\nA\nb\nB":                  "",
		"grep -q zzz <<-EOF\n\tabc\n\tEOF\nls /nonexistent": "ls",
		"grep -q zzz <<'EOF'\nabc\nEOF\nls /nonexistent":    "ls",
		"test $(case a in a) echo b;; esac) = c":            "test",
		"test $(case a in (a) echo b;; esac) = c":           "test",
		"test $(echo 1 # )\n) = 2":                          "test",
		"x=$(cat <<EOF\nit's\nEOF\n); grep -q x /dev/null":  "grep",
		"test $(echo ')') = 2":                              "test",
		"test $(echo \"a)b\") = 2":                          "test",
		"test $(( 1 << 2 )) -eq 5":                          "test",
		"(( x = 1 << 2 ))\ngrep -q x /dev/null":             "grep",
		"diff <(echo $(echo a)) <(echo b)":                  "diff",
	} {
		if got := lastCommandName(command); got != want {
			t.Errorf("lastCommandName(%q) = %q, want %q", command, got, want)
		}
	}
}
