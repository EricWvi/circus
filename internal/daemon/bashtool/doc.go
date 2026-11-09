// Package bashtool implements the device Bash and TaskStop tools. It returns one
// model-visible text body and structured BashResult metadata, without session
// state. The worker supplies cwd and working_dirs and remembers the returned cwd.
//
// A background daemon lacks the terminal's environment. On first use, a mutex-
// protected process-wide collector launches the user's bash with -l -i -c and
// stdin at /dev/null. Its clean environment contains HOME, USER, LOGNAME, SHELL,
// PATH=/usr/local/bin:/usr/bin:/bin, TERM=dumb, and the daemon's LANG if set.
// Login startup files run normally; .bashrc is also sourced independently. The
// collector saves exported environment as NUL-delimited records and serializes
// functions, non-exported user variables, shopt state, set -o state and aliases.
// Extglob is enabled while loading function definitions so system completion
// functions parse correctly; captured shopt state is restored afterwards. Alias
// expansion is enabled. Collection times out after 10 seconds, and failures
// retry once without -i with a warning. Non-bash login shells use /bin/bash with
// a warning. A missing snapshot is rebuilt on the next use. Explicit Options
// provide isolated snapshots for tests and embedded callers.
//
// Commands receive the captured exports minus PWD, OLDPWD, SHLVL and _, plus
// CLAUDECODE=1, GIT_EDITOR=true and COREPACK_ENABLE_AUTO_PIN=0. Each runs in a new
// process group under /bin/bash -c, sources the snapshot, disables extglob, and
// evals a safely single-quoted command with stdin at /dev/null. Only successful
// commands write a physical cwd. Cwd persists within the supplied working dirs;
// otherwise it returns to the first working dir (or request cwd). Commands never
// update the snapshot. Foreground stdout and stderr share one pipe. Cancellation
// signals the process group with TERM, escalating to KILL after two seconds.
// Background jobs use the same wrapper but write directly to an append-only
// file; their independent lifetime defaults to 30 minutes, capped at two hours.
// JobManager owns job IDs, command metadata, session attribution and stopping.
//
// Snapshots, exported-environment files and cwd scratch files live in private
// directories under $XDG_STATE_HOME/circus/daemon, falling back to
// ~/.local/state/circus/daemon. Output directories are private (0700), with files
// at $TMPDIR/circus-<uid>/<session_id or _>/tool-results/<call_id>.txt or
// tasks/<job_id>.output; TMPDIR defaults to /tmp. Output files use mode 0600.
// Unsafe path-component IDs are encoded. Exported snapshots may contain secrets
// from the user's own startup files and are protected by their private directory.
//
// Deliberate differences from the captured native CLI:
//   - Interactive collection reconstructs a terminal environment, including
//     profile exports, interactive bashrc branches and bashrc PATH changes. The
//     SDK fixture's noninteractive collector discarded some of these exports.
//   - Native binary shims for rg, find/bfs, grep/ugrep and the pkill guard are
//     omitted; commands use installed system tools and the user's definitions.
//   - Native SDK/session/socket variables are not synthesized. Only the three
//     command-environment additions above are forced.
//   - Files use daemon state and temporary directories instead of Claude's
//     config/project directories.
//   - CLI token suffixes, environment-update reminders and background completion
//     notifications belong to the worker and are never emitted here.
//
// Unmeasured rules are marked Inferred at their implementation: cancellation
// preserves output and appends "Command was interrupted"; longer timeout labels
// use whole seconds below a minute and minutes plus seconds thereafter.
package bashtool
