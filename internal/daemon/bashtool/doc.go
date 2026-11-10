// Package bashtool implements the device Bash and TaskStop tools. It returns one
// model-visible text body plus BashResult metadata. The worker supplies cwd and
// working_dirs, remembers the returned cwd, and renders its own reminders.
//
// A lazy shared collector reconstructs the user's terminal environment using
// bash -l -i -c with stdin at /dev/null. Warm explicitly starts it at production
// daemon startup; constructing tools or DefaultTools does not collect anything. Both collector and commands use setsid,
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
// are removed when the shells finish. Each daemon instance holds flock on a
// private instance-* directory; startup only removes instances whose lock can
// be acquired, plus legacy top-level snapshot-* directories. A Bash built
// with explicit Options owns its environment, and its Shutdown releases the
// instance: once no shell or visible background job holds a snapshot lease,
// it removes the directory and closes the lock; a later call collects into a
// new instance. NewBash without Options shares the process-wide environment,
// whose lock lasts until process exit. Failed refresh keeps a usable old snapshot, warns once, and
// remembers the failed startup stamps until they change again. Without an old
// snapshot, callers waiting on the same failed collection receive its error;
// a later independent call can retry. Cancellation of the collecting caller is
// not shared: waiters retry with their own contexts. The dispatcher starts the command timeout
// after load through toolcontext.StartTimeout,
// which survives context wrappers. Bash declares its own PreparationBudget,
// covering both collector attempts and cleanup. Commands do not update the
// captured environment. Explicit Options isolate startup/storage and can shorten
// the collector timeout for tests and embedded callers.
//
// Cwd is required and absolute. A deleted cwd falls back to working_dirs[0]; a
// missing start folder is an error. Only successful commands write physical cwd.
// Directory changes persist only within working_dirs, otherwise cwd resets to
// the start folder. Background directory changes do not update session cwd.
//
// Foreground stdout and stderr go directly to one private tool-results file.
// Only the shell is awaited: its background children can outlive the call within
// its session. Leftover process groups from foreground, promoted and explicit
// background shells are polled internally with kill(-pgid, 0). They have no job
// ID, JobEvent or TaskStop entry, no model notification, and no lifetime limit.
// Ending them with their session is planned for a later milestone. Their files
// remain linked and exempt from sweeps until the group is empty; inline-result
// deletion is postponed until then. Polling first reaps adopted group members
// with wait4(-pgid, WNOHANG), supporting PID 1 and child subreapers. Double forks without setsid remain tracked.
// After a timeout, cancellation, output cap, lifetime limit or shutdown kills
// a group, members the daemon adopted are reaped the same way until ECHILD,
// bounded by the five-second group wait, so none stays a zombie.
// Known limitation: setsid or any other escape from the process group cannot be
// tracked by this mechanism and may outlive the daemon.
// At most 30001 bytes are read. Up to 30000 raw bytes are inlined and the file
// removed once no process holds it; larger output stays on disk with a preview
// of at most 2000 UTF-16 code units, without splitting surrogate pairs. All
// model-facing bytes are decoded like Node UTF-8, replacing invalid subparts.
//
// The pre-execution sleep guard blocks an initial unwrapped sleep with a plain
// numeric argument >=25 in foreground mode. Separators and a following command
// affect the rejection wording; unit suffixes and wrappers bypass this guard.
// A foreground deadline kills leading sleep commands, preserving prior output.
// Other commands move to background, with their original session, process and
// output file. An inferred 100 ms drain precedes the promotion preview so output
// written at the deadline can appear. The message includes buffering and cwd notes where
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
// Snapshot state lives in $XDG_STATE_HOME/circus/daemon, falling back to
// ~/.local/state/circus/daemon when XDG_STATE_HOME is unset or not absolute,
// under locked instance directories. Output root
// is $XDG_CACHE_HOME/circus/output, falling back to ~/.cache/circus/output when
// XDG_CACHE_HOME is unset or not absolute. The circus cache directory itself
// (by default ~/.cache/circus) must not be a symlink, though its ancestors may
// be symlinks. The old
// os.TempDir()/circus-<uid> location is no longer used and is not cleaned up.
// Options.TempDir keeps the isolated
// circus-<uid> test override. Every owned directory (circus, output, session and
// kind) is checked with Lstat, must be real and owned by the current uid, and is
// repaired to mode 0700 when needed. Files use 0600. Unsafe ID components are encoded. Output sweeps run
// on first use, then at most hourly, deleting files older than 24 hours while
// exempting active commands and internally tracked groups. Sweeps run outside
// the environment lock, ignore per-entry errors, and always advance lastSweep.
// Shared output-file locks protect running files from other daemon instances'
// sweeps too. Snapshots can contain secrets and use private directories.
//
// Inferred: every command capture file has a 1 GiB cap. All running capture
// files in this daemon also share a 4 GiB total cap. Checks run every 100 ms;
// one shared stat pass accounts for foreground, promoted, background and tracked
// groups. Files of finished commands are excluded, even when retained on disk.
// Total overflow selects the largest file and waits for that victim to finish
// before selecting another. Transfers preserve a file's accounting and selected
// diagnostic. Overshoot between polls is possible. Exceeding either cap sends
// KILL to the selected process group, without TERM grace, and appends a diagnostic
// identifying the per-file or aggregate limit.
// Foreground results retain the usual inline/preview formatting rules.
// Deviation: native leaves ampersand output unbounded. Internally tracked groups
// have the same output cap. Shutdown stops these groups with TERM and a bounded
// KILL escalation; native leaves children running after a normal CLI exit.
// Inferred: after KILL, wait at most five seconds for group emptiness; if members
// remain, warn and release tracking and output resources anyway. Dispatcher tool
// shutdown contexts are bounded (default ten seconds per tool).
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
// syntax matching, shape-based timeout handling, mandatory cwd, promotion drain,
// preparation budget, fresh promoted lifetime, output cap, output retention,
// cancellation wording, and timeout labels beyond the measured durations.
package bashtool
