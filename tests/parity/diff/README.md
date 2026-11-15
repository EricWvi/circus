# Differential tool parity

Run many case definitions through the pinned native Claude Code tools twice,
then replay stable results through the daemon parity tests. Differences are
reported; this runner does not update daemon behavior or golden fixtures.
Node 24, Claude Agent SDK 0.3.319 (bundled CLI 2.1.319), Go, Linux,
`/usr/bin/bwrap`, `/bin/bash`, and the commands used by the scenarios are required.
The capture implementation is imported from `../native-capture/capture.mjs`.

## Setup and commands

From the worktree root, install capture dependencies under the ignored `.tmp`:

```bash
mkdir -p .tmp/diff-deps tests/parity/native-capture/node_modules
cp tests/parity/diff/package.json tests/parity/diff/package-lock.json .tmp/diff-deps/
npm ci --prefix .tmp/diff-deps --no-audit --no-fund --cache .tmp/npm-cache
ln -s ../../../../.tmp/diff-deps/node_modules/@anthropic-ai tests/parity/native-capture/node_modules/@anthropic-ai
node --test tests/parity/diff/runner.test.mjs
node tests/parity/diff/run.mjs --self-check
task parity-diff SEED=1 COUNT=200 DECK_COUNT=70
task parity-diff-sessions LIMIT=60
```

If native-capture already has its pinned dependencies installed, its existing
installation also works; do not replace it. The worktree's root `node_modules`
link is independent of these capture dependencies.

Sources can be combined. Explicit case files contain either a definition or an
array of definitions, in precisely the native capture schema:

```bash
node tests/parity/diff/generate.mjs --mode combo --seed 7 --count 100 > .tmp/cases.json
node tests/parity/diff/run.mjs --cases .tmp/cases.json
node tests/parity/diff/run.mjs --generated --mode combo --seed 1 --count 200 --cases .tmp/extra.json
node tests/parity/diff/extract-sessions.mjs
node tests/parity/diff/run.mjs --sessions .tmp/diff-corpus/sessions.jsonl --limit 60
```

Generation has two deterministic modes, both with one to four steps per case:

- `--mode combo` (the default) draws each case independently. Bash composes one
  to four atoms with independently sampled operators, wrappers and redirects.
  Atoms include byte-oriented printf payloads, arbitrary exit statuses, four
  signals, directory changes, exports, shell options and bounded file/stream
  commands. Later calls inspect cwd, variables and files from preceding calls.
  File sequences sample contents, ranges, matches, replacements and Read order
  independently; earlier Writes supply later Read/Edit inputs. IDs are
  `combo-<seed>-<n>`.
- `--mode deck` retains the shuffled coverage deck and its `gen-...` IDs, including
  explicit timeout and background cleanup coverage. It complements sampling;
  it is not a combinatorial generator.

`task parity-diff` runs a combo pass (COUNT defaults to 200), followed by a deck
pass (DECK_COUNT defaults to 70). Either mode is also runnable directly through
`run.mjs --generated --mode <mode>` or `generate.mjs --mode <mode>`.

The shared payload alphabet includes ASCII, CJK, emoji, quotes, backslashes,
whitespace, ANSI escapes, NUL and malformed UTF-8 bytes. Normal files sample
zero to 60 lines with independent CR/LF/CRLF endings and an optional final line
ending. Occasional repeated motifs create long lines at 262143/262144/262145
bytes; Bash payload lengths also sample 1999/2000/2001 and
29999/30000/30001 bytes. Compact repeated printf formats avoid ARG_MAX.
Malformed bytes remain raw in setup files and Bash output. String-only Write
and Edit parameters decode them with replacement characters. Binary setup
contents use the existing base64 representation. No generated case contains
transcript-derived data.

Extraction recursively scans `~/.claude/projects/**/*.jsonl`, retaining only
assistant `tool_use` inputs for Bash, Read, Write, and Edit. It discards inputs
over 64 KiB, deduplicates keys independently of JSON property order, and writes
only `{tool, input}` rows to `.tmp/diff-corpus/sessions.jsonl`. Conversation text
and tool outputs are never retained. Missing original file contents are not
reconstructed: Reads normally encounter missing files, and Edit gets a preceding
Write containing `old_string`. Absolute file-tool paths and relative paths that could escape the project are
mapped into it using their basename; safe relative paths are retained. Bash input
is unchanged. These are isolated input probes, not reconstructions of the
original session state. The limit applies to deduplicated corpus rows.

## Isolation and privacy

Before entering bubblewrap the runner compiles only the two Go test binaries
into `.tmp/diff-bin/`. Capture and replay then run in one sandbox with a
read-only root, fresh `/dev` and `/proc`, tmpfs `/tmp`, tmpfs over the invoking
HOME, private network and PID namespaces, and `--die-with-parent`. The worktree,
external root `node_modules` target, and Node executable are rebound read-only
when HOME masking hides them. Only the run directory is bound writable. The
sandbox starts its fake Messages API on its own loopback interface. There is no
real model call. Every capture still creates its own HOME, project, configuration,
and TMPDIR under tmpfs, and restores its process-wide umask after cleanup.
Captures are sequential. Go replay scratch files and leak-check HOME also use
the sandbox's `/tmp` through `CIRCUS_PARITY_TMPDIR`, and `CIRCUS_PARITY_CWD_LENGTH`
sizes the replay's working directory like the native one.

The self-check runs the same Bash case through both native and daemon tools:
it touches `$HOME/escape` and `/tmp/escape`, and requires an HTTPS request to
fail. It also verifies that writing outside the run directory is rejected and
that host escape sentinels are unchanged. A sandbox startup failure is fatal;
there is no automatic fallback. `--no-sandbox` is an explicit debugging option
for generated or explicit cases and is refused with `--sessions`.

Session corpus inputs, local diagnostics, expected files, and reports can contain
private commands and file contents. They stay under ignored `.tmp/`; do not
commit, upload, or paste them into network services. Progress prints case IDs
and statuses, never corpus commands. Extraction itself is local and read-only.

## Results and promotion

Each run creates `.tmp/diff-runs/<timestamp>-<suffix>/`:

- `expected/<first-step-tool>/<case-id>.json`: stable native fixture, in the
  existing golden format; never derived from daemon output.
- `diagnostics/`: capture diagnostics, timings, and both results for unstable cases.
- `replay/<case-id>.txt`: verbose output and exit code evidence from the Go tests.
- `daemon/<case-id>/`: normalized Bash step results and the actual daemon shell
  snapshot, collected before test cleanup through `CIRCUS_PARITY_ARTIFACT_DIR`.
- `report.json` and `report.md`: counts, per-case status, elapsed times, mismatch
  steps and compact differences. Average case time includes both native captures
  and daemon replay, excluding initial compilation.

The two complete normalized capture results, including tool schemas, must agree.
`unstable` and `capture-failed` cases are excluded from replay. The other statuses
are `matched`, `mismatched`, and `deviation`. Counts are mutually exclusive and
sum to `total`. A completed comparison exits zero even if differences or capture
failures were reported; infrastructure failures and failed self-checks exit
nonzero. No daemon differences are repaired automatically.

Replay uses the existing `TestNativeParity` subtests, with the ID in each subtest
name, and checks output plus project-file side effects. `CIRCUS_PARITY_DIR`
accepts an absolute path or a path relative to the repository root; absent tool
directories are allowed for external runs. With no override, committed-fixture
behavior is unchanged. `CIRCUS_PARITY_TMPDIR` changes only test scratch storage.
File-only sequences use fstools; sequences containing Bash or TaskStop use
bashtool, including sequences starting with a file tool.

Existing CLI reminder and path normalization still applies. Known shell
exceptions are recognized by command and startup-file input rather than case ID,
then validated with the existing semantic checks. Missing-path grep warnings
are recognized only when replacing the bundled shim's diagnostic prefix makes
the entire body identical. A blocked FIFO Read is
recognized by setup and path. These are reported as deviations only if the
semantic check passes. Arbitrary shell introspection, differently spelled
profile/shim probes, resource caps, malformed UTF-16 writes, and unmeasured
filesystem errors cannot all be inferred safely from an input. Such differences
remain mismatches for manual classification. Native-only Read options absent
from the daemon protocol also require manual classification. Sibling native
attachments and Bash filesystem metadata are captured as evidence but are not
additional daemon comparisons in the existing replay contract.

## Known mismatches and probes

`known-mismatches.json` contains synthetic, small case definitions for daemon
and replay differences that the runner once reported. They all match now and
serve as a regression set:

```bash
node tests/parity/diff/run.mjs --cases tests/parity/diff/known-mismatches.json
```

They cover the status-1 rule for grep-like commands after a failed cd, signal
statuses, the system missing-command handler, whitespace/CR/EOL policies, NUL
validation, empty-file Write wording, large-output formatting, partial-read
context and path-sensitive previews. `probe-cases.json` holds the probes that
established each rule; `NATIVE-BEHAVIOR.md` lists them by ID prefix. Some
probes still report differences on purpose: image and PDF Reads are an intended
deviation, and the open groups listed in `NATIVE-BEHAVIOR.md` keep their
minimal probes. Two runner-side effects also remain: when one case persists
several large outputs, capture normalizes only the last path to
`{{OUTPUT_PATH}}`, and commands whose concurrent pipeline members both write
stderr can order those lines differently between native and daemon runs.
Grouped classifications require reviewing the report; the runner does not
infer a root cause from a case ID.

Two of these were replay limitations rather than daemon defects, and the
replay now models them. Native adds the context-current parenthetical to an
Edit only when the session holds the whole file; external replays therefore
forget a file after a Read that does not cover every line, and after an Edit of
a file that was not held. Committed fixtures keep recording every successful
Read. Output that contains the working directory has a size and preview cut
that depend on the path length, so the runner exports the native capture's cwd
length as `CIRCUS_PARITY_CWD_LENGTH`, and external replays create their working
directory with exactly that length below the short `CIRCUS_PARITY_TMPDIR`
(`$TMPDIR/replay`).

On the measured Linux host, `/etc/profile` sources `/etc/bash.bashrc`, which
defines `command_not_found_handle`, only for a non-empty `PS1`. Native's
noninteractive collector never has one; the daemon's interactive collector now
starts with an empty `PS1`, so its snapshot no longer serializes that system
function. Snapshots and step results in the run directory are the evidence;
personal shell files are never used.

Promotion is explicit and copies only one stable expected fixture:

```bash
node tests/parity/diff/run.mjs --run-dir .tmp/diff-runs/<run> --promote <case-id>
```

Review the resulting `tests/parity/fixtures/native/<Tool>/<case-id>.json` before
committing. The runner never invokes Git or promotes a result automatically.
These tasks are intentionally excluded from CI and nightly jobs.
