package bashtool

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Options isolates shell startup and daemon files, primarily for embedded users
// and tests. Empty fields use the current user's environment and state directory.
type Options struct {
	Home     string
	StateDir string
	Shell    string
	TempDir  string
}

type environment struct {
	mu       sync.Mutex
	options  Options
	snapshot string
	env      []string
	loading  chan struct{}
}

var processEnvironment = &environment{}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700)
}

// Concurrent callers share collection, but retain their own cancellation budget.
func (e *environment) load(ctx context.Context) (string, []string, error) {
	for {
		e.mu.Lock()
		if e.snapshot != "" {
			if _, err := os.Stat(e.snapshot); err == nil {
				path, env := e.snapshot, append([]string(nil), e.env...)
				e.mu.Unlock()
				return path, env, nil
			}
		}
		if pending := e.loading; pending != nil {
			e.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", nil, ctx.Err()
			case <-pending:
				continue
			}
		}
		e.loading = make(chan struct{})
		e.mu.Unlock()
		path, env, err := e.collect(ctx)
		e.mu.Lock()
		if err == nil {
			e.snapshot, e.env = path, env
		}
		close(e.loading)
		e.loading = nil
		e.mu.Unlock()
		return path, env, err
	}
}

func (e *environment) collect(ctx context.Context) (string, []string, error) {
	home := e.options.Home
	if home == "" {
		var err error
		home, err = os.UserHomeDir()
		if err != nil {
			return "", nil, err
		}
	}
	state := e.options.StateDir
	if state == "" {
		state = os.Getenv("XDG_STATE_HOME")
		if state == "" {
			state = filepath.Join(home, ".local", "state")
		}
		state = filepath.Join(state, "circus", "daemon")
	}
	if err := privateDir(state); err != nil {
		return "", nil, err
	}
	shell := e.options.Shell
	if shell == "" {
		shell = os.Getenv("SHELL")
	}
	if shell == "" {
		// os/user has no shell field on Unix; the passwd entry is the fallback.
		if data, err := os.ReadFile("/etc/passwd"); err == nil {
			uid := fmt.Sprint(os.Getuid())
			for line := range strings.SplitSeq(string(data), "\n") {
				fields := strings.Split(line, ":")
				if len(fields) >= 7 && fields[2] == uid {
					shell = fields[6]
					break
				}
			}
		}
	}
	if filepath.Base(shell) != "bash" {
		slog.Warn("only bash is supported; using /bin/bash", "shell", shell)
		shell = "/bin/bash"
	}
	name := os.Getenv("USER")
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	clean := []string{"HOME=" + home, "USER=" + name, "LOGNAME=" + name, "SHELL=" + shell, "PATH=/usr/local/bin:/usr/bin:/bin", "TERM=dumb"}
	if lang, ok := os.LookupEnv("LANG"); ok {
		clean = append(clean, "LANG="+lang)
	}
	dir, err := os.MkdirTemp(state, "snapshot-")
	if err != nil {
		return "", nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = os.RemoveAll(dir)
		}
	}()
	snapshot, exports := filepath.Join(dir, "snapshot.sh"), filepath.Join(dir, "environment")
	// Source .bashrc independently: login profiles need not source it. Serialize
	// non-exported user variables too, without restoring Bash's process-local state.
	collector := `if [ -f "$HOME/.bashrc" ]; then source "$HOME/.bashrc"; fi
command env -0 > ` + quote(exports) + `
{
printf '%s\n' '# Snapshot file' 'unalias -a 2>/dev/null || true' 'shopt -s expand_aliases' 'shopt -s extglob'
declare -f
while IFS= read -r _circus_name; do
 case $_circus_name in BASH*|CLAUDECODE|GIT_EDITOR|COREPACK_ENABLE_AUTO_PIN|EUID|UID|PPID|SHELLOPTS|SHLVL|PWD|OLDPWD|_|PIPESTATUS|FUNCNAME|GROUPS|DIRSTACK|RANDOM|SRANDOM|SECONDS|LINENO|HIST*|PS*|PROMPT_COMMAND|_circus_*) continue;; esac
 _circus_decl=$(declare -p "$_circus_name" 2>/dev/null) || continue
 _circus_flags=${_circus_decl#declare -}
 _circus_flags=${_circus_flags%% *}
 case $_circus_flags in *r*|*x*) continue;; esac
 printf '%s\n' "$_circus_decl"
done < <(compgen -v)
shopt -p
set +o
alias -p
printf '%s\n' 'shopt -s expand_aliases'
} > ` + quote(snapshot) + `
`
	for attempt := range 2 {
		args := []string{"-l", "-i", "-c", collector}
		if attempt == 1 {
			args = []string{"-l", "-c", collector}
		}
		cmd := exec.Command(shell, args...)
		cmd.Env = clean
		cmd.Dir = home
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		// A failed interactive collector must not leave files that can make
		// an early-exiting fallback appear successful.
		_ = os.Remove(snapshot)
		_ = os.Remove(exports)
		cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
		if err = cmd.Start(); err == nil {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err = waitProcess(ctx, cmd)
			cancel()
		}
		if err == nil {
			_, err = os.Stat(snapshot)
			if err == nil {
				_, err = os.Stat(exports)
			}
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		if attempt == 0 {
			slog.Warn("interactive shell snapshot failed; retrying noninteractive login shell", "error", err)
		}
	}
	if err != nil {
		return "", nil, fmt.Errorf("collect bash environment: %w", err)
	}
	data, err := os.ReadFile(exports)
	if err != nil {
		return "", nil, err
	}
	env := make([]string, 0)
	for _, value := range bytes.Split(data, []byte{0}) {
		key, _, found := strings.Cut(string(value), "=")
		if !found {
			continue
		}
		switch key {
		case "PWD", "OLDPWD", "SHLVL", "_", "CLAUDECODE", "GIT_EDITOR", "COREPACK_ENABLE_AUTO_PIN":
			continue
		}
		env = append(env, string(value))
	}
	env = append(env, "CLAUDECODE=1", "GIT_EDITOR=true", "COREPACK_ENABLE_AUTO_PIN=0")
	if err := os.Chmod(snapshot, 0600); err != nil {
		return "", nil, err
	}
	if err := os.Chmod(exports, 0600); err != nil {
		return "", nil, err
	}
	ok = true
	return snapshot, append([]string(nil), env...), nil
}
