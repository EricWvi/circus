# Circus worker

Build with Node.js 24 or later and run from the repository root.

```sh
npm ci
task worker:build
circus join --role worker
node worker/dist/main.js
```

The worker runs Claude Code sessions in local mode: it attaches with
`local_only`, acquires the sessions the control plane offers it (up to
`CIRCUS_WORKER_MAX_SESSIONS`) and drives each with the Claude Agent SDK and
Claude Code's own tools in the session's working directory. It logs to
stderr as JSON lines and shuts down gracefully on SIGTERM or SIGINT: it stops
taking leases, closes every query, releases every lease and closes the link,
within 15 seconds. It reads the credential written by
`circus join` from `$CIRCUS_HOME/config/worker.json`, or from
`$XDG_CONFIG_HOME/circus/worker.json` with a default of
`~/.config/circus/worker.json`. The file supplies the server URL, principal ID,
bearer token and optional CA bundle.

| Environment variable | Default | Values |
| --- | --- | --- |
| `CIRCUS_WORKER_MAX_SESSIONS` | `4` | Integer from 1 to 64 |
| `CIRCUS_WORKER_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `CIRCUS_WORKER_IDLE_SECONDS` | `300` | Integer from 1 to 86400: how long an idle session keeps its lease |
| `CIRCUS_BIN` | `circus` on `PATH` | The `circus` binary providing `internal exec-guard` |

Claude Code runs with the user's own login on this machine
(`CLAUDE_CONFIG_DIR` is left alone) and the environment of the worker.

## Claude Agent SDK

The worker pins `@anthropic-ai/claude-agent-sdk` **0.3.330**, which bundles
Claude Code **2.1.330**, and its peers `@anthropic-ai/sdk` 1.12.0,
`@modelcontextprotocol/sdk` 1.32.2 and `zod` 4.6.7. Attach reports both
versions.

Each session runs one `query()` on the main thread with: `cwd` the
`SessionConfig.cwds` entry of `default_device`; `settingSources: ['user',
'project', 'local']`; the `claude_code` preset system prompt; the session's
permission mode (`auto` when empty); partial messages; `sessionId`, or
`resume` once the session has a transcript; the control-plane SessionStore
with eager flushing; a PreToolUse hook; and
the CLI launcher below. No tool list, MCP server, alias or agent is set.

SDK behaviour the worker relies on, measured with this version against a
fake Messages API (`test/fixtures/sdk/` holds the recordings its tests
replay):

- SessionStore appends are serialized across keys; a rejected append is
  retried after 200 ms and 800 ms, then dropped with one `system/mirror_error`
  before the turn's `result`; `result` waits for pending appends.
- An input's `uuid` is the transcript entry's uuid. Pushed inputs are not
  echoed as SDK messages; `command_lifecycle` frames (not in the SDK's types)
  announce them: `started` before the turn that takes them (or during the
  turn they merge into), `completed` after its `result`, and the `result`
  lists them in `user_message_uuids`. The worker attributes an input to a
  turn only from these (never to a turn that did not announce it), so an
  input racing a turn the CLI started itself is marked with its own turn.
- A user message pushed again with a uuid the CLI already took is
  dropped by Claude Code (it dedups user messages by uuid). The worker
  itself never pushes an input twice in a query, although TakeInputs
  returns every input taken in the epoch and not yet persisted, after a
  resumed lease too; with this CLI that guard cannot be observed end to end,
  so the worker's unit tests cover it (`test/runner/turns.test.ts`).
- Every turn starts with `system/init`, including turns the CLI starts by
  itself after a background task (preceded by `system/task_notification`).
- No `priority` merges into the running turn; `'now'` interrupts it, the
  turn ends with an empty `result` (`success`, `terminal_reason`
  `aborted_tools` or `aborted_streaming`) and a new turn takes the input.
- `interrupt()` ends the turn with `error_during_execution`; the CLI emits
  the `[Request interrupted by user]` user message (`... for tool use]` for a
  tool) as an SDK message, without `isSynthetic`. The worker publishes it as
  the `user_interrupt` note.
- `isSynthetic: true` on a pushed message makes the CLI prefix it with
  `[MESSAGE FROM NON-USER SOURCE - NOT USER INPUT]`, so the worker's own
  notes are pushed as `<system-reminder>` blocks in front of the user's
  input instead.
- A query that resumes from the SessionStore runs Claude Code with a
  temporary `CLAUDE_CONFIG_DIR` (`$TMPDIR/claude-resume-*`, with copies of
  the credentials and settings) that the SDK deletes when the query ends.
  The copied OAuth credentials lose their refresh token: such a query
  works until the copied access token expires, then every model call fails
  with "OAuth token has expired". The SDK's `getOAuthToken` option is only
  honoured by the CLI for a few host entrypoints; the worker does not use it.
  Instead, after a turn whose model call failed authentication
  (`authentication_failed`), the worker replaces the query once its
  background tasks have finished, or when the next input arrives (the
  tasks still running are then reported lost): the next input starts a new
  one, which copies the login as it is then in the real config
  directory (fresh if the user's own Claude Code or a non-resumed query has
  refreshed it).

View events go to the control plane in PublishEvents batches about every
100 ms, one batch in flight. A turn's events are acknowledged before its
ReportTurn ENDED and before the next STARTED; right after STARTED, the
turn's TURN_STARTED and the USER events of its inputs are flushed at once,
so a worker that dies early in a turn still leaves them in the view (unless
it dies before that flush is acknowledged).

## Transcript mirroring and repair

The SessionStore adapter keeps every entry the control plane has not
acknowledged, per subpath and in order, including the batches the SDK gave
up on (the ones behind a `mirror_error`). The next append sends them first.

Retained entries are bounded in memory cost, counted as twice their UTF-8
bytes (V8 keeps a string with any non-Latin-1 character at two bytes per
character): 32 MiB per session and 512 MiB for the whole worker, shared by
all sessions. In the worst case, every session at its bound up to the
worker budget, the retained entries take about 512 MiB of heap (16 sessions
at 32 MiB); a session that would exceed either bound becomes unrepairable. When a
session ends its retained entries are returned to the worker budget, and
appends the SDK makes after that are refused at once.
Since the runner interrupts the turn at the first `mirror_error` and takes no
input until the repair, a backlog is normally one turn's tail.

Entries go to the control plane in wire batches of at most 1 MiB (a single
larger entry goes alone). Each batch has its own 20 s, in an append call as
in the repair, so a backlog drains over any uplink
that carries 1 MiB in 20 s: about 52 KiB/s (0.42 Mbit/s). Slower links, or a
single entry of n MiB below n × 52 KiB/s, cannot get it through. After a
`mirror_error` the worker reports the turn ENDED with
`transcript_incomplete` and no marks (the session becomes BROKEN), closes
the query, and resends exactly those entries with their original content
and `batch_seq`; then it marks the turn's items persisted and sends
TranscriptRepaired. It never re-imports a local transcript. When the
missing entries are gone (a batch refused as INVALID or LIMIT, the bound
exceeded, or a worker restart), the repair fails: the lease is released
with ERROR and the session stays BROKEN for the user to recover. Background
tasks of the closed CLI are reported lost to the model with the next input.

## Approvals

When the permission mode denies a tool use (`system/permission_denied`, or
`result.permission_denials`), the worker asks for a one-time approval. The
device is the session's `default_device` (where local tools run), in
RequestApproval, ConsumeGrant and when reading grants. The tool is the SDK's
tool name. The match key is the SHA-256 of:

| Tool | Matched text |
| --- | --- |
| Bash, PowerShell | `command`, verbatim |
| Write, Edit, MultiEdit / NotebookEdit | `file_path` / `notebook_path`, made absolute against the session cwd |
| WebFetch / WebSearch | `url` / `query`, verbatim |
| Glob | canonical JSON of `pattern` and `path` (those present; `path` made absolute against the cwd) |
| Grep | canonical JSON of `pattern`, `path`, `glob` and `type` (those present; `path` made absolute) |
| Agent (Task) | `subagent_type`, verbatim |
| MCP tools (`mcp__*`) and every other tool | canonical JSON of the whole input (object keys sorted at every level), so only an identical input matches |

Denials by an explicit deny rule or a hook are not asked about. A grant is
known to the worker only once the control plane has it (`Lease.state.grants`,
or a GetSessionState refresh when an approval input arrives), never at
RequestApproval. The PreToolUse hook allows a call only when `ConsumeGrant`
returns `allowed` for that tool, device, key and tool use.

## Claude Code processes

Claude Code is launched as
`circus internal exec-guard --parent <worker pid> -- <command> <args...>`
when `CIRCUS_BIN` (or `circus` on `PATH`) runs that subcommand. The guard
leads its own session and process group and supervises the CLI. On Linux it
kills and reaps the CLI's entire descendant tree, including Bash commands in
their own sessions, when the worker dies, the CLI exits, or a release sends
SIGUSR1 to the guard after the exit grace period. The CLI also receives
SIGKILL if its Linux guard dies. The guard's teardown deadline is five
seconds. Changed credentials can prevent it from signalling a process,
and a process blocked in kernel I/O or held by an outside tracer can
delay reaping through that deadline.

The worker waits seven seconds after SIGUSR1 before falling back to
SIGKILL on the guard's process group. That fallback can leave tools
running if they left the guard's session. On macOS the guard starts the
CLI in a separate process group and cleans up that group. A descendant
that leaves the CLI's group (for example via `setsid`) can survive. The
worker's group-kill fallback on macOS reaches the guard's group only, so
the CLI's group can survive a forcibly killed guard.
If the guard is unavailable, the worker logs a warning and launches the
CLI directly as the leader of its own session and process group.

The worker records the leader's pid and start time, the kernel boot id,
the owning worker's pid and start time, and the SDK's `claude-resume-*`
directory if the SDK made one for that CLI (a directory in the worker's own
tmpdir created after the worker started, excluding the worker's own
`CLAUDE_CONFIG_DIR`). If the file system reports no birth time, the worker
skips that check. The record includes the directory's device and inode and
lives in `${XDG_RUNTIME_DIR:-$TMPDIR}/circus-worker-<uid>/` (0700, outside
HOME).

On Linux, after release or at startup, the worker scans the session and
process group of a gone guard for leftovers. Startup cleanup applies to
records owned by dead workers. The boot id and process start time protect
against PID reuse. Once the recorded leader is gone, the worker removes
its `claude-resume-*` directory and its copy of the credentials if the
directory still matches the record. On macOS cleanup uses the recorded
PIDs to check liveness and skips the session scan.

On shutdown a running turn gets up to ten seconds to end on its own (its
ENDED keeps its outcome and marks); a turn still running then is closed by
the release as INTERRUPTED, as the contract prescribes for a shutdown.

`WorkerLink` exposes one hint iterator. Wait for `onReady` before issuing
operations and update the held set with `setHeld` as lease ownership changes.
Attach reserves one slot, leaving 63 for session operations. Excess operations
fail locally. Pass a signal with a deadline to each request. A request waits
for its turn in the send byte budget and the same signal also bounds the
reply wait. Keep the submitted operation unchanged until it settles.
The budget is the smaller of the negotiated message limit and 32 MiB,
including a small per-operation Unwatch reservation. Split batches that
exceed the budget. An oversized individual record fails locally.
The runner owns request input and delivered replies
and must budget its own concurrent reads and writes. A lost reply produces `LinkLostError`. Retry after readiness
using the same operation idempotency keys and the same content. An aborted
operation sends Unwatch. If it was an acquisition, reacquire with `fresh` and
release before giving up the session.

## Wire fixtures

Go generates the downlink frames in `test/vectors/go.json` and validates the
malformed frames there. Vitest decodes them and generates the uplink fragments
in `test/vectors/ts.json`. The Go wire decoder and reassembler read those
fragments and compare the complete message against the expected protobuf
message. Each suite compares its generated file against the checked-in bytes.

```sh
UPDATE_WORKER_WIRE_VECTORS=1 go test ./internal/wire -run '^TestWorkerWireVectors$' -count=1
UPDATE_WORKER_WIRE_VECTORS=1 npm -w @circus/worker run test -- test/vectors.test.ts
```
