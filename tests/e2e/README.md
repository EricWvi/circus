# Process end-to-end tests

Run on Linux with Go, Bash, the usual coreutils and Node.js 24 or later installed.
Run `npm ci` from the repository root before running the suite (it installs
the Agent SDK and the Claude Code CLI for this platform).

```sh
go test -tags e2e ./tests/e2e/ -count=1
go test -tags e2e ./tests/e2e/ -count=3
go test -race -tags e2e ./tests/e2e/ -count=1
```

The harness builds `bin/circus` once in a per-run temporary directory under
`.tmp/`, with `e2efault` enabled. Under `-race` the harness builds only a
race-enabled binary instead. The build and all test temp directories share that per-run root, removed
by a defer on normal return. Each root holds an flock for its whole lifetime;
startup sweeps a root only when it can acquire that lock, so hard test timeouts
are collected without relying on PID liveness or deleting an active run. Runs
predating the lock file are deleted only when more than one hour old. The
default build contains no crash-injection branch. Tests use isolated HOME/XDG directories, real SQLite
databases, an initial loopback port-0 listener, enrollment links read from
stdin, and real daemon processes. The server uses a short test timer poll
interval instead of the default 1s. Restarts reuse the server's logged address
only after waiting for its exact PID to exit. No port probing or failed-dial
inference is used.

Each subprocess has its own process group and Linux Pdeathsig=SIGKILL. Start and
Wait run on one dedicated runtime.LockOSThread goroutine per child, keeping the
creator thread alive for the whole child lifetime. Cleanup signals only recorded
PIDs, waits for them, escalates to SIGKILL after a grace, and checks that none
remains. The daemon-crash and revocation tools write their exact PID and then execs sleep; the Linux
subreaper collects that one adopted child after killing its daemon
(TestRunDirectorySweepUsesFlock checks the run-directory lock separately). Child stderr
is included in failed-test logs. All ordering uses protocol replies, durable
metadata, marker files or bounded polling; no sleep establishes test ordering.

- Happy path: Bash, read, write and edit through both the CLI and Go client;
  CLI query/cancel; ordinary tool errors still exit zero. Bash passes --cwd;
  file calls leave it empty and use absolute paths.
- Validation and operation answers: invalid IDs fail locally, a raw empty-ID
  rejection closes instead of hanging, and query/cancel NOT_FOUND prints
  "unknown call id" with exit 1 without becoming a call outcome.
- CLI exits: a running query exits 4, running cancel exits 0 with Dispatched,
  accepted cancel exits 0 with Cancelled, and querying that Cancelled state
  exits 1. The daemon-crash test also proves --wait follows Unknown to terminal
  Lost. A server crash after Submit exits 3 and prints the call ID
  (TestCLITransportFailureIsUnknown).
- Worker ownership: closing a client mid-call does not cancel it; a new
  connection obtains its Result. On one connection a worker can Query and
  Cancel a call it is watching: Query answers Dispatched, Cancel answers
  Dispatched (intent recorded) and the stream carries the real terminal update.
  66 sequential calls exceed the 64-slot cap and prove terminal slot release.
  Raw same-link Submit→Cancel (request IDs 1 and 2) stays ordered: both the
  Cancel reply and the watched stream's terminal update are Cancelled.
- TestUnwatchFreesSlotImmediately: with the device offline inside its grace
  period, 64 Accepted watches fill one connection; the next Submit is refused
  locally, and after closing one stream (Unwatch) it is admitted at once by
  the server while the unwatched call stays Accepted and uncancelled.
- TestSessionTurn: a session with pending input (written to the server's
  database) is offered to a worker that attaches over the test client
  (`internal/testutil/workerclient`); the worker acquires it, takes the
  input, reports the turn, runs a device call admitted by its lease through
  the real daemon (a call at another epoch is `SESSION_FENCED`) and
  releases.
- TestTypeScriptWorkerLink: the worker workspace builds and connects using a
  credential from `circus join`. It takes input for a session the server
  assigns to the worker on the daemon's machine. It loses an in-flight reply
  during a control-plane restart, resumes the same lease, receives a replayed
  Nudge and retries successfully. It round-trips a fragmented transcript entry,
  publishes a result, ends the turn and releases the lease.
- TestWorkerRestartWhileHalfOpen: a worker connection goes half-open (a
  local proxy stops moving bytes without closing), a CLI call is running on
  the same credential, and a restarted worker still connects and gets a
  result well inside the heartbeat timeout.
- TestCancelReplies: Cancel of a finished call answers its result, of an
  accepted call (device offline) Cancelled (the watched stream ends
  Cancelled too), and of an unknown ID Rejected NOT_FOUND.
- TestCancelErrorDoesNotBlockLaterCancel: at the cap (63 Accepted watches
  with the device offline inside its grace period), a Cancel of an unknown ID
  answers NOT_FOUND in the last slot and gives it back, so a later real
  Cancel still fits and answers Cancelled; the link stays open.
- TestDaemonCrashLeavesCallUnknown: kill the daemon during a marked Bash call; observe Unknown without ending
  the subscription (CLI query exits 3), restart with a new boot, then observe
  Lost. Equal resubmit stays Lost; the marker contains exactly one line.
- TestServerCrashLosesAcceptedCallThenResubmits: take the device offline within grace, persist Accepted, kill the
  server, and restart. Query reports Lost with no dispatch boot. Equal resubmit
  executes exactly once when the daemon returns.
- TestServerCrashWithRunningToolRecoversResult: block a marked real tool on a FIFO, kill the server, release the
  tool, then restart. The same daemon boot reconciles to Completed; Query
  returns the real retained output, an equal resubmit acts as a per-call ordered
  dedup barrier, and only then is the marker counted (exactly one line).
- TestServerCrashAfterDispatchCommitLosesCall: the tagged Sender wrapper
  SIGKILLs the server after dispatch commit and before gateway delivery. Restart queries the daemon and settles Lost. Equal retry
  stays Lost and the marker is absent (zero executions).
- TestRevocationEndsDaemon: a separate CLI process revokes the connected daemon in the same SQLite
  database. Recheck closes the link, its next upgrade receives 403, and the daemon
  exits nonzero. Revoking the worker closes its link while a call subscription
  is active; a new dial gets 403. Assertions allow the 1s recheck interval plus
  5s timeout and scheduling.
- Graceful shutdown: SIGTERM with daemon and worker connected exits zero within
  the configured deadline. The device row records LastDisconnectedAt >=
  LastConnectedAt and has a device_grace timer before Router.Close could lose
  that evidence. The daemon stays alive and executes after the server restarts.

## Real worker against a fake model

The `TestClaude*` tests run the real worker (`node worker/dist/main.js`)
with the Claude Code CLI
the Agent SDK bundles, a real control plane and a real daemon on one
machine, in local mode. The model is `internal/testutil/fakemodel`, an
in-process fake Messages API that each test scripts with a Go closure;
the worker gets `ANTHROPIC_BASE_URL` pointing at it, a fake
`ANTHROPIC_API_KEY`, `CIRCUS_BIN` pointing at the built `circus` (for
`internal exec-guard`), and `HOME`, `CLAUDE_CONFIG_DIR` and
`XDG_RUNTIME_DIR` under the test's directories. The worker's environment,
which Claude Code and every Bash command the fake model scripts inherit, is
built from an allowlist (`PATH`, `LANG`, `LC_*`, `TZ`, `USER`, `LOGNAME`,
`SHELL`, `TERM`, `TMPDIR`, the harness's `HOME` and `XDG_*_HOME`) plus what
the stack sets, with `NO_PROXY=127.0.0.1,localhost` and no proxy: no
credential, proxy, agent socket or git variable of the caller gets
through. Telemetry, error reporting, auto-updates and other non-essential
traffic are disabled, so nothing reaches a real account or the network. The sessions' project
directories live in `<TMPDIR or /tmp>/circus-e2e-<uid>/e2e-run-*` (flock'd
and swept like the main run root), never under the user's home or this
repository: Claude Code reads the `CLAUDE.md` files of every ancestor of its
working directory, which would include the real `~/.claude/CLAUDE.md`.

The harness builds the worker once per run, and only when its inputs
(`worker/src`, the proto package's sources, the package files) changed since
the last e2e build: a stamp under `.tmp/` records their hash, and the check
and the build hold one lock, so concurrent runs never rewrite `worker/dist`
while another one starts a worker from it. The fake is strict: a request it
does not route to the test's script (a Claude Code session's request
offering Bash, not a subagent's) fails the test. Scripted tool loops are
bounded (`for i in $(seq 1200)`), and waits for files and session events
are bounded to 20 s. After the harness waits for its direct children, it
kills any remaining recorded session leaders and kills and reaps every
process the test binary adopted as a child subreaper.

Each test asserts the control plane's events and session state, the files
on disk where tools ran, the requests the fake model received and, where
it matters, the stored transcript (no duplicated input, answer or tool
result). Sessions of scenarios the Go fake worker also has are compared
with its recording (`web/src/lib/session-events/fake-worker.fixture.json`,
what the Web reducer is tested against; `compareEvents` in
`claude_compare_test.go`) when the test ends:

- Scope: from the session's first durable event to the next durable event
  after the scenario's last one, so the scenario's trailing ephemeral
  events are in. Without such an event the session is stopped at the end
  of the test and its SESSION_STATE ends the scope (every event published
  before it comes before it on the stream); for a session already
  stopped, what the watch received.
- The same durable event kinds and streamed SDK events in the same order
  (a run of deltas of one type counts as one; `KIND_QUEUE` events, which
  the control plane publishes on its own timing, are left out).
- Each event with the same key paths and JSON types as the fake's.
  Enumeration-like values (types, subtypes, stop and terminal reasons,
  notes, tool names, permission decisions, ...) must be equal; ids,
  texts the scripts write, counts, timings, tool inputs and the metric
  objects (`usage`, `modelUsage`, `safeguard_results`, ...) only need the
  same type. The fixture keeps the values that vary from run to run
  (`timestamp`, `usage`, durations, ...) as type markers
  (`fakeworker.MaskUnstable`); the real events are masked the same way.
- The fixed texts the Web shows are equal: the notes the worker writes for
  the model (except the interrupt note, which the Web hides) and the
  `tool_result` of a call that did not run to completion, with the
  scenario's approval device, command and reason replaced by placeholders.
- On each side, the ids the reducer joins on point where they should: a
  permission denial, an approval request and a `tool_result` name an
  earlier `tool_use`; `api_message_id`, the durable `message.id` and
  `abandoned_blocks.api_message_id` name the message being streamed.

The streamed copies are also checked against the durable events the way
the reducer joins them: a stream's `message_start` id is the durable
`message.id`, its `content_block_start` indexes count from 0, and the
n-th durable assistant event of a message carries block n of the same
type.

To re-capture what the real worker publishes (after an Agent SDK or
Claude Code upgrade), set `CIRCUS_E2E_RECORD_REAL` to a file (relative to
the repository root, e.g. `.tmp/real-events.json`): every compared
scenario then also writes the events it compared, ephemeral ones included
and payloads as received, under its name in the fixture's format. The
test process truncates the file at its first write, so it holds only the
scenarios of that run.

```sh
CIRCUS_E2E_RECORD_REAL=.tmp/real-events.json go test -tags e2e ./tests/e2e/ -run 'TestClaude' -count=1
```

- TestClaudeTextAndTools: a text turn, a thinking turn, one Bash, two
  parallel Bash calls, and Write, Read, Edit and Bash changing a file in
  the project directory.
- TestClaudeMergeAndNow: an input sent while Bash runs joins the turn
  (Claude Code's transcript shows it queued before the command is let go;
  the model sees it with the tool result; one turn); an input with NOW
  priority stops the streaming turn and starts its own; the turn after it
  completes.
- TestClaudeInterruptAndStop: interrupt while the model streams and while
  Bash runs (the command's process is gone afterwards), each followed by a
  turn that completes; stop mid-turn releases the lease and ends the
  session's Claude Code.
- TestClaudeApproval: auto mode's server-side check (`safeguards` answered
  `flagged` by the fake) denies Bash, the approval card is resolved, and
  the retried command runs exactly once although the check flags it again
  (the worker's PreToolUse hook allows the approved call). A denied
  approval is not retried and the session takes its next input (compared
  with the fake worker's "deny").
- TestClaudeWorkerKilled: SIGKILL of the worker mid-stream (input already
  in the stored transcript) and with another session idle: both guards
  exit and both CLI PIDs are verified reaped, the held stream is cut, no
  model request follows; a new worker process resumes both sessions in a
  new epoch; the interrupted one tells the model about the interruption.
- TestClaudeWorkerKilledDuringBash: SIGKILL of the worker while Bash runs
  in a separate session. The guard reaps the shell and the CLI before
  exiting, and a new worker resumes the session in a new epoch.
- TestClaudeControlPlaneRestart: the control plane restarts mid-stream; the
  lease is resumed (same epoch and boot, new connection) and the turn
  completes once; the worker then shuts down and releases its lease.
- TestClaudeIdleRelease: with `CIRCUS_WORKER_IDLE_SECONDS=1` the idle lease
  is released and Claude Code exits (its process is taken while the answer
  is held); the next input acquires a new epoch and the conversation
  resumes from the stored transcript.
- TestClaudeBackgroundTask: a background Bash task ends while the session
  is idle; its end is published, then Claude Code's own turn (origin `cli`)
  tells the model.
- TestClaudeMissingDirectory: the session's directory is gone when the
  worker takes it: an ERROR event is published before the lease is
  released, and no Claude Code starts.

The normal suite completes in about a minute including the builds; the
real-worker tests take a few seconds each. Tests have individual deadlines and never start Docker or BuildKit.
