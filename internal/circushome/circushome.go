// Package circushome resolves CIRCUS_HOME, which moves all of a CLI's local
// files under one directory: credentials in <CIRCUS_HOME>/config, daemon
// state in <CIRCUS_HOME>/state and command output in <CIRCUS_HOME>/cache.
// It lets a development build run next to an installed circus without
// sharing files, and unlike the XDG variables it is not inherited by
// anything else as a changed default (tools run by the daemon see their
// usual XDG paths).
package circushome

import (
	"fmt"
	"os"
	"path/filepath"
)

// Env is the variable's name.
const Env = "CIRCUS_HOME"

// Root returns CIRCUS_HOME as an absolute path, or "" when it is unset or
// empty. A relative value is taken relative to the working directory.
func Root() (string, error) {
	v := os.Getenv(Env)
	if v == "" {
		return "", nil
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", fmt.Errorf("%s: %w", Env, err)
	}
	return abs, nil
}

// Dir returns <CIRCUS_HOME>/<name>, or "" when CIRCUS_HOME is unset.
func Dir(name string) (string, error) {
	root, err := Root()
	if err != nil || root == "" {
		return "", err
	}
	return filepath.Join(root, name), nil
}
