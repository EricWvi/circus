# Native Claude Code capture

This harness scripts the bundled Claude Code model through a local fake Anthropic
Messages API. The real built-in tools run; their next model-facing requests are
the evidence. It does not simulate tool implementations or call a real model.

## Run

Use Node 24 on Linux with `/bin/bash` and the standard commands used in
`cases.mjs` available. From this directory:

```bash
npm ci --no-audit --no-fund --cache .tmp/npm-cache
node capture.mjs
node capture.mjs --case 'read-*'
node capture.mjs --skip-slow
node capture.mjs --case bash-background --case bash-background-stop
node measure-thresholds.mjs
node measure-thresholds.mjs read-cjk-lines bash-cjk
node measure-sleep-encoding.mjs
node measure-sleep-encoding.mjs --supplement
node measure-sleep-encoding.mjs --units
node measure-amp-huge.mjs
node measure-amp-huge.mjs --bisect
node capture.mjs --case 'bash-amp-child-*' --case 'read-huge-line-*'
```

The SDK is pinned to `0.3.319`; the captured bundled CLI is `2.1.319`.
The committed lockfile includes the SDK's platform dependencies. Each selected
case gets a new session, project, HOME, config directory and effective TMPDIR
inside an `os.tmpdir()` temporary tree. `finally` closes the SDK and fake server,
then removes that entire tree, including snapshots, task output and transcripts.
Each capture explicitly calls `process.umask(0o022)` before setup and SDK spawn;
the CLI inherits that mask through the SDK's unchanged subprocess launcher.
The previous process umask is restored after teardown, including failure paths.
Captures are sequential because Node's umask is process-wide. No shell wrapper
or extra environment variable is added, preserving existing model-facing output.
Temporary diagnostics and npm's cache stay in this directory's ignored `.tmp/`.
An ordinary SIGINT or forced process termination can interrupt JavaScript cleanup;
the completed runs used for these fixtures left no case trees behind.

Only the five requested tools are enabled, with bypass permissions explicitly
allowed, `settingSources: []`, an empty MCP configuration and
`strictMcpConfig: true`. No host environment is inherited: `options.env` replaces
it with the allowlist in `capture.mjs`. This strips all inherited `CLAUDE*`,
credentials, provider settings and proxies. The allowlist sets the local
`ANTHROPIC_BASE_URL`, a fake API key, fresh `CLAUDE_CONFIG_DIR`,
`DISABLE_TELEMETRY=1` and `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`.
HOME is deliberately isolated too, so personal shell files and settings cannot
add credentials to `env` or the snapshot. This captures a clean Linux profile,
not the invoking user's customized shell.
The profile-sourcing case supplies controlled startup files through
`setup.home_files` under that isolated HOME. A case may select `shell` (used by
the conditional zsh variant); other environment values remain on the allowlist.

On failure the process exits nonzero, continues trying other selected cases, and
writes redacted diagnostics to `.tmp/<case>-failure.txt`. A previous fixture is
not evidence that a failed current capture succeeded; inspect the exit status
and `.tmp/last-run.json`. Successful cases print `new`, `changed` or `identical`
against the previously saved fixture, plus SHA-256 hashes in that run report.

The full run includes cases marked `slow: true`; `--skip-slow` omits them.
`bash-timeout-long` is marked slow and has `deadline_ms: 90000`, allowing its
65000 ms tool timeout even though native currently rejects its leading sleep
before execution. Ordinary sessions retain the 60000 ms harness deadline.

## Routing and evidence

`fake-api.mjs` adapts the phase0/a prototype. It accepts
`POST /v1/messages?beta=true` (the query is not required for routing) and
`POST /v1/messages/count_tokens`. A primary Messages request must use the selected
model, initially contain the case marker in conversation history and offer native
`Read`. A known continuation can lose its marker after native compaction; the
compaction control request itself remains auxiliary as described below.
All other Messages requests get a minimal text response without advancing the
script; this includes title generation even if it uses the same model.
Unexpected endpoints fail the run. Request headers are never recorded.
The HTTP request stream uses incremental UTF-8 decoding (`setEncoding('utf8')`)
so a multibyte character crossing network chunks cannot become replacement
characters. This matters for the large CJK fixtures.

The fake supports JSON responses and streaming SSE. Streaming sends
`message_start`, indexed block start/delta/stop events, `message_delta` and
`message_stop`; tool JSON is split across two input deltas. Synthetic usage is
25 input / 10 output tokens. Each primary turn emits exactly one `tool_use` with
a deterministic ID. The next primary request is matched by `tool_use_id`, and
the result and all other blocks in that same user message are extracted. Then
the harness performs the next step's optional `before.write`, emits that step,
or sends the final text response. Success requires that the final response and
every emitted tool result arrive. No SDK-facing pretty-print is used as evidence.

`capture()` is also importable. `capture(definition, { persist: false })` runs
the identical native loop and returns the normalized fixture in memory without
writing a golden fixture. `measure-thresholds.mjs` uses it for independent-session
integer bisection, widening the initial bracket if necessary and stopping only
when the passing and failing sizes differ by one unit. Its probe outcomes and
boundary sizes are written to ignored `.tmp/threshold-results.json`; when families
are named, results for other families are preserved. The complete measured probe
lists are published in the behavior document. Only the two boundary definitions
per family in `threshold-cases.mjs` are included in a normal capture, alongside
the requested range, edge-input, combined-stream and many-line cases.

`measure-sleep-encoding.mjs` likewise uses fresh native sessions without saving
probe fixtures. The base command runs 42 bisection, shell-shape, timeout and
encoding probes. `--supplement` appends four timeout discriminators and a full
background lifecycle; `--units` appends four duration-syntax probes. Run these
commands once in the order above to reproduce the 51-row report at ignored
`.tmp/sleep-encoding-probes.json`. All outcomes and distinct messages are copied
into the behavior document. `sleep-encoding-cases.mjs` retains only 17
representative fixtures, reusing the earlier golden cases for redundant shapes.

`when: 'task'` is an optional step condition: TaskStop cleanup runs only when an
actual task ID was discovered in a preceding native result. It records a real
model-facing TaskStop step if executed; otherwise it emits no tool call. This
stops native-managed background tasks without inventing task IDs or adding
failed cleanup calls to blocked cases. A shell's own `&` child does not provide
a native task ID and is not mislabeled as a managed task.

`measure-amp-huge.mjs` runs ten ampersand/process probes and nine generated-file
probes sequentially. `--bisect` reuses their 1 MiB/10 MiB bracket and appends
23 fresh-session single-line range-budget probes. Raw process observations,
timings and all 42 outcomes are retained in ignored `.tmp/amp-huge-probes.json`
and documented in the behavior spec. `amp-huge-cases.mjs` exports only eleven
representative fixtures into the ordinary capture list.

The native wire format in this version uses **string** `tool_result.content` for
all these cases. Fixtures retain that type verbatim instead of inventing an
array of text blocks. If a future case produces a block array, that array is
retained. An absent wire `is_error` is represented as `false`. `attachments`
contains only sibling blocks, not reminders already embedded in the result
string. In this capture all reminders, including the background completion
notification, are embedded, and every `attachments` array is empty.

`fixtures/native/tools.json` is the complete first primary request's `tools`
array, including descriptions and JSON schemas. Subsequent cases must advertise
the identical array after normalization. `NATIVE-BEHAVIOR.md` reproduces it and
documents the observed behavior, including contradictions with descriptions.

## Normalization

Normalization replaces run-specific identifiers, paths and the explicitly
documented unrelated-host process rows, plus redacts
credential assignments before any diagnostic or fixture is persisted. It does
not remove reminders, trim output, normalize CRLF file bytes, or alter line
numbers. PID-table numeric columns and their variable-width padding are replaced
together. All substitutions are applied recursively to string values; object
keys such as relative filenames are retained.
Assignment redaction and absolute-path discovery are anchored to assignments
and path starts, respectively, to avoid quadratic regex work on large single
lines. These harness fixes require no changes to the original 51 fixtures.

| Placeholder | Meaning |
| --- | --- |
| `{{CWD}}` | Fresh case project directory, the initial SDK working directory. |
| `{{TMPDIR}}` | Isolated effective system temporary directory passed to the CLI. |
| `{{SYSTEM_TMPDIR}}` | Host `os.tmpdir()` if it appears outside the isolated tree; not present in the committed cases. |
| `{{HOME}}` | Fresh isolated home directory, not the real user's home. |
| `{{CONFIG_DIR}}` | Fresh `CLAUDE_CONFIG_DIR`. |
| `{{CASE_ROOT}}` | Temporary parent of project, home, config and tmp; normally hidden by more specific replacements. |
| `{{CAPTURE_DIR}}` | This harness's absolute installation path, visible in native executable paths. |
| `{{PROJECT_KEY}}` | Native slash-to-hyphen encoding of the absolute project path. |
| `{{SESSION_ID}}` | CLI session UUID. |
| `{{TASK_ID}}` | Actual background task ID; the deliberately bogus ID is unchanged. |
| `{{OUTPUT_PATH}}` | Exact discovered background or persisted-output file path. |
| `{{SNAPSHOT_PATH}}` | Snapshot path discovered in the shell wrapper. |
| `{{PID}}` | PID/PPID fields, CLI PID, or the PID component of its messaging socket. |
| `{{TIME}}` | Timestamp, including the fixture's `meta.captured_at`. |
| `{{DURATION}}` | Variable elapsed wall time, if reported; not present in these cases. |
| `{{PORT}}` | Ephemeral local fake API port. |
| `{{ID}}` | Random cwd scratch basename, tool-result basename or snapshot suffix if still exposed. |
| `<redacted>` | Credential-looking assignment value, including the fake key and CLI-created messaging token. |

Longest concrete paths are replaced before their constituent identifiers.
The output and snapshot directory structures hidden by whole-path placeholders
are documented in `NATIVE-BEHAVIOR.md`. Definition inputs may use `{{CWD}}`,
`{{TASK_ID}}`, `{{OUTPUT_PATH}}` or `{{SNAPSHOT_PATH}}`; these are expanded to real
values before the fake sends the input. The task, output and snapshot references
come from captured native text, never guessed IDs or paths.

## Stability and adding cases

Run twice and inspect `.tmp/last-run.json`: every selected case must say
`identical` on the second run. `tools.json` is checked for exact equality too.
The behavior document records the completed two-run check. Stability is measured
after normalization, not by hiding semantic changes or regenerating expectations
from a daemon implementation. Synthetic token usage and deterministic tool IDs
also keep model-facing token reminders and notification references stable.

To add a case, add one plain-data definition to `cases.mjs`: `id`, `description`,
`setup.files`, `setup.dirs` and `steps`. File contents are strings or
`{ "base64": "..." }`. Each step has `tool` and exact `input`; an external change
uses `before: { write: { path, content } }`. Multi-step cases use one session.
Deleting the persisted working directory uses
`before: { remove: { path: "{{CWD}}/gone" } }`; the harness removes that path
recursively immediately before sending the associated tool input.
All Bash inputs include `description`. The shell-info snapshot step alone is
conditional (`when: 'snapshot'`) because it must only read a path actually
returned by native Bash; skipping it is recorded in fixture `notes`.

`files_after` recursively records all regular files under the case project.
Valid UTF-8 text without NUL is stored verbatim, including CRLF and final newlines;
binary content is base64. Output files outside the project are represented by
their native result text and, in the background case, the requested Read step.
Do not add host credentials or personal startup files to case setup.
The profile case's `resolve_snapshot: true` cats the sole actual `.sh` file in
the fresh config's `shell-snapshots` directory; it fails unless exactly one exists.
This avoids adding a process-inspection command to the requested seven-step
sequence. HOME setup is recorded as data but is not part of project `files_after`.

## Links, modes and metadata

Optional setup fields extend the existing file/directory setup:

```json
{
  "files": { "a.txt": "alpha\nbeta\ngamma\n", "script.sh": "#!/bin/sh\necho before\n" },
  "dirs": [],
  "symlinks": { "link.txt": "a.txt", "dangling.txt": "missing.txt" },
  "hardlinks": { "b.txt": "a.txt" },
  "modes": { "script.sh": "0755" }
}
```

All setup keys are relative to `{{CWD}}`. Setup creates directories and regular
files first, then symlinks, hard links, and finally explicit modes. Relative
symlink targets are passed verbatim to `symlink`; they resolve relative to the
link's parent, not the harness's directory. Placeholders in a target are expanded
before creation. Hard-link values name existing paths relative to `{{CWD}}`.
Modes must be four octal digits and are applied with `chmod` (which follows a
symlink if one is named); these cases set modes on regular files only.

A definition may specify `observe_fs: ["a.txt", "b.txt", "link.txt", "missing.txt"]`.
After the last tool step the fixture adds optional `fs_after`, keyed by exactly
those relative paths. Replay checks use its keys as the observation list; the
definition-only `observe_fs` property is not separately serialized. For example,
an untouched hard-link pair and a dangling symlink would be represented as:

```json
{
  "a.txt": { "type": "file", "mode": "0644", "same_inode_as": "b.txt" },
  "b.txt": { "type": "file", "mode": "0644", "same_inode_as": "a.txt" },
  "dangling.txt": { "type": "symlink", "link_target": "missing.txt", "mode": "0777", "same_inode_as": null },
  "missing.txt": { "type": "missing", "same_inode_as": null }
}
```

This example explains the format, not a Write outcome: native Write/Edit break
the hard-link relationship in the captured cases. Types and modes use `lstat`,
so symlink metadata describes the link itself, including a dangling link.
`link_target` is the exact stored `readlink` string with ordinary path
normalization; it is present only for symlinks. `mode` is `st_mode & 07777`,
formatted as a four-digit octal string, and is omitted for a missing path.
`same_inode_as` is the lexicographically first *other* path listed in setup
(`files`, `dirs`, `symlinks`, `hardlinks`, or `modes`) whose final `lstat` device
and inode equal this path's. It is `null` when no such path exists, including
missing paths. It compares final paths, not a path's historical inode; raw inode
and device numbers are never persisted. Unsupported special file types fail
capture rather than being mislabeled.

`files_after` continues to include regular-file contents without traversing
symlinks. A kept link is described by `fs_after`; its target's contents appear
under the target's own regular filename. Fixtures without `observe_fs` retain
their original format, and all accepted existing fixtures remain unchanged.

## FIFO guards and elapsed-time observations

`setup.fifos: ["p"]` creates named pipes relative to `{{CWD}}`, after regular
files/links and before explicit modes. It invokes `/usr/bin/mkfifo` directly,
without a shell, under the same explicit 022 umask. The project file-content
walker skips FIFOs and never reads them. They are not an additional supported
`fs_after.type`; the FIFO case does not request metadata observations.

A step may specify `guard_ms`. The timer starts just before its `tool_use` is
sent and is cleared when the next primary request supplies the corresponding
result. For this case only, the SDK's spawn callback tracks the exact CLI child
using the same command, arguments, cwd, isolated environment and pipe stdio.
If the guard expires, the harness aborts the query and sends SIGKILL to that
specific child. It closes the SDK/fake API and removes the isolated case tree.
Unrelated processes are never signaled. An ordinary failed or missing result
without a declared expired guard still fails capture.

`read-fifo` intentionally has no writer. If it blocks, its one step records:

```json
{
  "tool": "Read",
  "input": { "file_path": "{{CWD}}/p" },
  "result": null,
  "attachments": [],
  "observation": { "status": "blocked", "guard_ms": 10000, "session_terminated": true }
}
```

This is a harness observation of **no native tool_result**, not fabricated
native content or an API error. The fixture also records a fixed explanatory
note. If a future native version returns before the guard, its real result is
captured normally instead. The intentional blocked observation counts as a
captured case; it does not require a final model answer after the killed session.

Every step's elapsed interval is written only to ignored
`.tmp/<case>-timings.json`, measured from sending the scripted tool input to
receipt of the next primary request carrying its result. For a blocked step,
the interval ends after termination. Variable timing values are excluded from
fixtures; measured foreground-child timings are quoted in the behavior doc.

Task-ID normalization also recognizes automatic timeout-to-background messages.
Scratch `cwd-*` normalization matches path components only, so the literal case
name `bash-cwd-deleted` is preserved. These extensions do not change any of the
80 previously accepted fixtures.

## Process observations, generated files and large result digests

`observe_native: { processes: true }` enables a local SDK spawn callback with
the unchanged CLI executable, arguments, cwd, isolated environment and pipe stdio.
The observer records the actual CLI exit code/signal. `/proc/<pid>/status` VmHWM
is sampled every 20 ms for that CLI PID, including Read auxiliaries/compaction;
it excludes the Node harness and external Bash children. The maximum observed
kernel high-water mark is diagnostic evidence, not a promised hard memory bound.
`processes: false` keeps only RSS/exit observations for generated-file probes.

For process probes, the harness samples `ps -C sleep` and identifies only processes
whose `/proc/<pid>/cwd` equals the isolated project. It retains their start ticks
to prevent PID-reuse mistakes. Immediately after each native result arrives,
before sending another scripted Bash call, it records PID, PPID, PGID, SID,
state, parent metadata, `readlink` targets of fd 1/2 and `ls -l` of those fds.
It repeats this after normal CLI exit. `before: { delay_ms: 22000 }` keeps the
model request open on the fake server, then sends the next tool without using
a native sleep command. Raw observations and elapsed times are written only to
`.tmp/<case>-observations.json` and `.tmp/<case>-timings.json`.

Native child rows retain identity/equality through role placeholders:
`{{CLI_PID}}`, `{{HARNESS_PID}}`, `{{CHILD_PID}}`, `{{FOREGROUND_SLEEP_PID}}`,
`{{INIT_PID}}`, `{{SHELL_PID}}`, `{{CLI_PGID}}`, `{{CLI_SID}}`,
`{{CHILD_PGID}}`, `{{CHILD_SID}}` and fallback `{{PROCESS_N_PID}}`.
One numeric ID always has one label, so a child's PGID and SID may intentionally
share a label. PID-table padding is canonicalized. Role choice is independent
of whether sampling catches the transient shell before it exits.

Unrelated host sleeps were present (one versus two `sleep 180` rows), despite
sequential probe execution. The requested native `ps` command is unchanged.
Only in these process fixtures, all non-probe rows are represented by a single
`{{UNRELATED_SLEEP_ROWS}}` placeholder after the header, meaning zero or more
ambient rows. Owned rows are retained, not merged into this placeholder. The
complete original stdout and actual numeric IDs remain in diagnostics and the
spec's observation tables. This explicit environment normalization avoids
mistaking host activity for a change in native child-lifetime behavior.

For replay tests, the only golden output in `bash-amp-child-*` that contains
unrelated host processes is `Bash/bash-amp-child-primary.json`, step 2,
`result.content`, from `ps -o pid,pgid,sid,ppid,stat,args -C sleep`; the other
four fixtures (`exit`, `managed`, `timeout`, `term-trap`) contain no such output.
Apply the existing `NativeObserver.normalize` rule to that process table:
identify probe-owned PIDs by `/proc/<pid>/cwd` matching the isolated case cwd,
retain `/proc/<pid>/stat` start ticks to guard against PID reuse, and never
identify ownership from `sleep 20` text alone. Replace all contiguous numeric
rows after the header with exactly one literal `{{UNRELATED_SLEEP_ROWS}}` line
(even when there are no unrelated rows), followed by the owned rows in their
original order. Canonicalize the header to `PID PGID SID PPID STAT COMMAND`
and column separators to single spaces; map PID, PGID, SID and PPID consistently
to the observed role placeholders above, preserving numeric identity and
relationships (the primary child's equal PGID/SID both become
`{{CHILD_PGID}}`, and its parent PID 1 becomes `{{INIT_PID}}`). Keep owned rows,
their `STAT` and command text, surrounding newlines, reminders and other result
content unchanged; an absent owned child or a different process relationship
must remain a detectable difference. Harness-side `ps`/fd/after-exit observations
are diagnostics in `.tmp`, not golden output, and require no replay comparison.

CLI exit is observed before cleanup and before final project-file collection.
Surviving owned children/wrappers are terminated only afterward; the diagnostic
`cleanup_signals` records those harness actions separately. No unrelated host
process is signaled. A trap file created by cleanup cannot be misreported as
evidence of a native exit signal, because fixture `files_after` was captured
before cleanup. All isolated files, descriptors and surviving probe children
are cleaned up per case.

`setup.generate` describes a file without embedding its contents:

```json
{
  "path": "huge.txt",
  "byte": "x",
  "size": 314572800,
  "newline": true,
  "prefix": "0123456789\n"
}
```

`size` is the repeated one-byte payload count, excluding optional UTF-8 `prefix`
and optional final LF. This example has a ten-character first line, then a
300 MiB second-line payload, for 314572812 total file bytes. Without the prefix
it has 314572801 bytes. The generator writes at most 1 MiB chunks under the
existing 022 umask and verifies the final length. The file walker stream-hashes
generated files instead of reading them into a Node string.

Optional `generate.line_bytes` replaces every Nth payload byte with LF (N
includes the LF); a final partial line retains its remaining `x` bytes. It does
not change `size`, prefix or final-newline semantics. For example, `size: 10485759`,
`line_bytes: 100`, `newline: false` creates exactly 10485759 total bytes with
104857 complete 100-byte lines and a 59-byte final line. Without `line_bytes`,
generation is unchanged. `node measure-size-text.mjs` runs all requested default
Read size-text probes, exact integer bisection and both boundary line-structure
checks in separate sessions, without persisting probe fixtures; diagnostics go
to `.tmp/size-text-probes.json`. `node capture.mjs --case 'read-size-text-*'`
captures only the two representative boundary fixtures.

For `summarize_large: true`, result strings of at least 1 MiB are represented by
the following descriptor in `result.content`; generated `files_after` values
always use the same format. Other results, including all existing fixtures,
retain their original format:

```json
{
  "format": "sha256-bytes",
  "length_bytes": 3200090,
  "sha256": "71667e1d8db3607870dd2ff99bdef486a1eeb8f75c0488a44fe7940aff4eeb7a",
  "head_base64": "...",
  "tail_base64": "..."
}
```

The digest and byte length cover UTF-8 encoding of the complete normalized native
result string, including line numbering and reminders, not serialized JSON.
For a generated file they cover its raw bytes. Head and tail each contain the
first/last 200 bytes, stored as base64 to preserve exact byte boundaries. This
descriptor is capture metadata, not native tool output or an extra text block.

Large Read results can trigger native auxiliary summarization and compaction.
The fake gives those calls minimal responses without advancing the script.
Compaction is recognized by the native last-user text beginning
`CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.`; after the first
primary turn, the isolated conversation may continue without its original
marker following compaction. The harness observes tool_result blocks on *all*
Messages requests, independently of script advancement. If the first request
carrying a result is a compaction request, its step has `delivery: "compaction"`
and includes the other blocks in that same user message. This records where
the raw data actually went; it does not claim the post-summary primary model
received the full file. Existing non-compacting fixtures remain unchanged.
