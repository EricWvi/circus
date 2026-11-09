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
model, contain the case marker in conversation history and offer native `Read`.
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

Normalization replaces only run-specific identifiers and paths, plus redacts
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
