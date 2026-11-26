package userservice

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/EricWvi/circus/internal/circushome"
	"github.com/EricWvi/circus/internal/credfile"
)

// Definition is what an installed service file runs.
type Definition struct {
	// Argv is the program and its arguments.
	Argv []string
	// Env is the environment the service file sets.
	Env map[string]string
}

// Program returns the executable, or "" when the file names none.
func (d Definition) Program() string {
	if len(d.Argv) == 0 {
		return ""
	}
	return d.Argv[0]
}

// Credentials returns the credential file of role the program reads: the
// daemon's --credentials argument, else the default path under the
// recorded CIRCUS_HOME, XDG_CONFIG_HOME or HOME.
func (d Definition) Credentials(role credfile.Role) string {
	for i, arg := range d.Argv {
		if arg == "--credentials" && i+1 < len(d.Argv) {
			return d.Argv[i+1]
		}
		if path, ok := strings.CutPrefix(arg, "--credentials="); ok {
			return path
		}
	}
	name := string(role) + ".json"
	if root := d.Env[circushome.Env]; root != "" {
		return filepath.Join(root, "config", name)
	}
	if config := d.Env["XDG_CONFIG_HOME"]; filepath.IsAbs(config) {
		return filepath.Join(config, "circus", name)
	}
	home := d.Env["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".config", "circus", name)
}

func makeDefinition(exe string, args, env []string) Definition {
	d := Definition{Argv: append([]string{exe}, args...), Env: make(map[string]string)}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		d.Env[key] = value
	}
	return d
}

// recordedSettings are the recorded variables a replacement reports when
// they change, besides the program and the credentials.
var recordedSettings = []string{circushome.Env, "CIRCUS_NODE"}

func (s *Service) reportReplacement(out io.Writer, old, next Definition) {
	var changes []string
	fields := []struct{ name, old, next string }{
		{"program", old.Program(), next.Program()},
		{"credentials", old.Credentials(s.role), next.Credentials(s.role)},
	}
	for _, key := range recordedSettings {
		fields = append(fields, struct{ name, old, next string }{key, old.Env[key], next.Env[key]})
	}
	for _, field := range fields {
		if field.old != field.next {
			changes = append(changes, fmt.Sprintf("%s %q → %q", field.name, field.old, field.next))
		}
	}
	if len(changes) > 0 {
		fmt.Fprintf(out, "Replacing %s: %s\n", s.title(), strings.Join(changes, ", "))
	}
}
