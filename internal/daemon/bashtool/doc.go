// Package bashtool implements the device Bash and TaskStop tools. It returns one
// model-visible text body plus BashResult metadata. The worker supplies cwd and
// working_dirs, remembers the returned cwd, and renders its own reminders.
//
// A lazy, shared collector reconstructs the user's terminal environment using
// bash -l -i -c with stdin at /dev/null. Both collector and commands use setsid,
// like Node's detached mode: they have an independent session and no controlling
// terminal. Group signals still target -pid. Collection has a ten-second limit
// and retries without -i on failure. Non-bash login shells fall back to /bin/bash
// with a warning. Its clean environment contains HOME, USER, LOGNAME, SHELL,
// PATH=/usr/local/bin:/usr/bin:/bin, TERM=dumb and the daemon's LANG when set.
// Login startup files run normally; .bashrc is also sourced independently.
// Exports are captured as NUL-delimited records. The snapshot serializes user
// functions, non-exported variables, shopt/set options and aliases. Extglob is
// enabled while loading functions, then disabled in the command wrapper. The
// wrapper removes which/unsetenv aliases and functions before eval. Commands
// receive captured exports minus PWD, OLDPWD, SHLVL and _, plus CLAUDECODE=1,
// GIT_EDITOR=true and COREPACK_ENABLE_AUTO_PIN=0. Stdin is /dev/null.
//
// Every call checks mtime and existence of .bashrc, .bash_profile, .bash_login
// and .profile. Changes or a missing snapshot rebuild it; successful replacement
// removes the previous directory. Live shells hold leases so concurrent refresh
// cannot remove their source or cwd scratch files. Their obsolete directories
// are removed when the shells finish. First use removes stale snapshot-* dirs.
// Commands do not update the captured environment. Explicit Options isolate
// startup and storage for tests and embedded callers.
//
// Cwd is required and absolute. A deleted cwd falls back to working_dirs[0]; a
// missing start folder is an error. Only successful commands write physical cwd.
// Directory changes persist only within working_dirs, otherwise cwd resets to
// the start folder. Background directory changes do not update session cwd.
//
// Foreground stdout and stderr go directly to one private tool-results file.
// Only the shell is awaited: its background children can outlive the call within
// its session. At most 30001 bytes are read. Up to 30000 raw bytes are inlined and
// the file removed; larger output stays on disk with its stat size and a preview
// of at most 2000 UTF-16 code units, without splitting surrogate pairs. All
// model-facing bytes are decoded like Node UTF-8, replacing invalid subparts.
//
// The pre-execution sleep guard blocks an initial unwrapped sleep with a plain
// numeric argument >=25 in foreground mode. Separators and a following command
// affect the rejection wording; unit suffixes and wrappers bypass this guard.
// A foreground deadline kills leading sleep commands, preserving prior output.
// Other commands move to background, with their original session, process and
// output file. The promotion message includes buffering and cwd notes where
// applicable. Explicit background jobs use tasks files. Both kinds append the
// native exit marker on completion. Background lifetime defaults to 30 minutes,
// capped at two hours; promotion starts a new remaining lifetime. Cancellation
// and job stops send TERM to the group, allowing two seconds for shell cleanup
// before KILL; remaining descendants are killed after the shell exits.
//
// jobs.Manager owns IDs, session ownership, ordered events and shutdown. A
// lifetime limit produces KILLED. A natural exit during Stop keeps its actual
// exit code and EXITED state. TaskStop reports no task for ended or foreign jobs.
//
// Snapshot state lives in $XDG_STATE_HOME/circus/daemon (default
// ~/.local/state/circus/daemon). Output root is $XDG_RUNTIME_DIR/circus, or
// os.TempDir()/circus-<uid>. Each owned directory is checked with Lstat, must be
// a real directory owned by the current uid, and is repaired to mode 0700 when
// needed. Files use 0600. Unsafe ID components are encoded. Output sweeps run
// on first use, then at most hourly, deleting files older than 24 hours while
// exempting active jobs. Snapshots can contain secrets from startup files and
// are protected by their private directories.
//
// Deliberate differences from the captured native CLI:
//   - Interactive reconstruction preserves profile exports, interactive bashrc
//     branches and bashrc PATH changes that native's SDK snapshot may discard.
//   - Native binary shims for rg, find/bfs, grep/ugrep and the pkill guard are
//     omitted; commands use installed system tools and user definitions.
//   - SDK/session/socket variables are not synthesized; only the three command
//     environment additions above are forced.
//   - Storage uses daemon directories instead of Claude config/project paths.
//   - CLI token suffixes, cwd reminders and job notifications belong to the worker.
//   - Startup refresh and output retention are daemon lifecycle decisions.
//
// Unmeasured extensions are marked Inferred at implementation: initial sleep
// syntax matching, shape-based timeout handling, mandatory cwd, surviving &
// children, fresh promoted lifetime, stale snapshot cleanup, output retention,
// cancellation wording, and timeout labels beyond the measured durations.
package bashtool
