package daemonservice

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/EricWvi/circus/internal/circushome"
)

type definition struct {
	argv []string
	env  map[string]string
}

func (d definition) program() string {
	if len(d.argv) == 0 {
		return ""
	}
	return d.argv[0]
}

func (d definition) credentials() string {
	for i, arg := range d.argv {
		if arg == "--credentials" && i+1 < len(d.argv) {
			return d.argv[i+1]
		}
		if path, ok := strings.CutPrefix(arg, "--credentials="); ok {
			return path
		}
	}
	if root := d.env[circushome.Env]; root != "" {
		return filepath.Join(root, "config", "daemon.json")
	}
	if config := d.env["XDG_CONFIG_HOME"]; filepath.IsAbs(config) {
		return filepath.Join(config, "circus", "daemon.json")
	}
	home := d.env["HOME"]
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	return filepath.Join(home, ".config", "circus", "daemon.json")
}

func makeDefinition(exe string, args, env []string) definition {
	d := definition{argv: append([]string{exe}, args...), env: make(map[string]string)}
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		d.env[key] = value
	}
	return d
}

func reportReplacement(out io.Writer, old, next definition) {
	var changes []string
	for _, field := range []struct{ name, old, next string }{
		{"program", old.program(), next.program()},
		{"credentials", old.credentials(), next.credentials()},
		{circushome.Env, old.env[circushome.Env], next.env[circushome.Env]},
	} {
		if field.old != field.next {
			changes = append(changes, fmt.Sprintf("%s %q → %q", field.name, field.old, field.next))
		}
	}
	if len(changes) > 0 {
		fmt.Fprintln(out, "Replacing circus daemon service:", strings.Join(changes, ", "))
	}
}
