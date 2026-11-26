# Circus worker

The worker runs Claude Code sessions for the control plane in local mode,
on a machine you enrolled with `circus join`. It ships without any agent:
you install Claude Code per worker from the Web UI.

## Install and start

Each release has a package per platform, `circus-worker-<tag>-<os>-<arch>.tar.gz`
(`linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`; listed with
their SHA-256 digests in the release's `latest.json`, under
`worker_assets`, and in `SHA256SUMS`). It needs Node.js 24 or later and the
`circus` CLI of the same release. The CLI installs it as a service of the
user (a systemd user unit on Linux, a launchd agent on macOS), which is what
the Web UI's "Add device" command does for a worker:

```sh
circus join --role worker   # paste the enrollment link
circus worker install       # download, verify and unpack the package, install and start the service
circus worker status        # version, Node.js, service file and log command
```

`circus worker install` downloads the package of the CLI's own version,
verifies it against the manifest's digest and unpacks it to
`<data>/worker/<version>/` (see `<data>` below); the service runs its
launcher, `<data>/worker/<version>/circus-worker`, with `CIRCUS_NODE` set to
the executable of the Node.js it found and checked (`$CIRCUS_NODE`, else
`node` on `PATH`, as found; resolved only when it lies in a per-shell or
temporary directory such as fnm's) and
`CIRCUS_BIN` to the installing `circus`. It records the shell's `PATH`,
`CIRCUS_HOME`, `HOME`, `USER`, locale, XDG directories, proxy variables,
`NODE_EXTRA_CA_CERTS`, `NODE_USE_SYSTEM_CA` and the variables in the table
below that are set; rerun it to change them. The
proxy variables are stored in the service file (readable by the user only)
as set, credentials in their URLs included. Logs go to the journal
(`journalctl --user -u circus-worker.service -f`) or to
`~/Library/Logs/circus/worker.log`. `circus update` moves the service to the
new release's package and restarts a running worker; `circus worker
uninstall` removes the service and the packages, not the agents or the
credential. See the circus README ("Worker") for the details.

To run a package by hand instead (with no service):

```sh
tar -xzf circus-worker-<tag>-<os>-<arch>.tar.gz   # unpacks circus-worker/
circus-worker/circus-worker
```

`circus-worker/circus-worker` finds Node.js (`$CIRCUS_NODE`, else `node` on
`PATH`) and fails with a clear message when it is missing or older than 24,
with the terminal status below, after writing the reason to the stop file
(only when `CIRCUS_WORKER_STOP_FILE` is set);
it may be symlinked onto `PATH`. The package holds the built worker and its
production dependencies, nothing platform specific: regular files and
directories only, as `circus worker install` accepts nothing else.

Then open the worker in the Web UI and install Claude Code. The worker
downloads the Claude Code build pinned for this worker release (see below),
verifies it against the sha512 digest built into the worker, and installs
it under `<data>/agents/claude-code/<version>/`; the Web UI shows the
progress. Until Claude Code is installed, sessions for this worker wait
("agent not installed"). Uninstalling waits until no session the worker
holds runs Claude Code, then removes only `<data>/agents/claude-code/`:
your own Claude Code configuration and login (`~/.claude`,
`CLAUDE_CONFIG_DIR`) are never touched. A worker upgraded to a release that
pins another Claude Code version shows the old install as outdated and runs
nothing with it until you choose "Update".

`<data>` is `$CIRCUS_HOME`, else `$XDG_DATA_HOME/circus`, else
`~/.local/share/circus`; the agents go to `<data>/agents/` and the worker
packages `circus worker install` manages to `<data>/worker/`. An agent
install interrupted by a stop or crash leaves a temporary directory there,
removed at the next start.

From a source checkout, build and run with `npm ci`, `task worker:build` and
`node worker/dist/main.js`; `node worker/scripts/package.mjs <tag> <out-dir>
<target>...` builds release packages (usage at the top of the script).

The worker attaches with `local_only`, acquires the sessions the control
plane offers it (up to `CIRCUS_WORKER_MAX_SESSIONS`) and drives each with
the Claude Agent SDK and Claude Code's own tools in the session's working
directory. It logs to stderr as JSON lines and shuts down gracefully on
SIGTERM or SIGINT: it stops an install in progress, stops taking leases,
closes every query, releases every lease and closes the link, within 15
seconds. When the control plane refuses its credential (an upgrade answered 401 or
403 that carries the control plane's `Circus-Auth: rejected` marker, or a
rejected Hello), does not accept this worker (an incompatible Welcome), or
the credential or a setting (`CIRCUS_NPM_REGISTRY` included) is missing or
invalid, it writes the reason to its stop file and exits with the terminal
status. The stop file is `$CIRCUS_WORKER_STOP_FILE` (recorded in the service
by `circus worker install`, read by `circus worker status`), else
`<data>/worker-state/stop.json`; it is written atomically with mode 0600,
only its own directory is created (0700), and the worker removes it when it
starts. A 401 or 403 without the marker (a proxy's) is retried.
The terminal status is 78 on
Linux, which the systemd unit of `circus worker install` does not restart
(`RestartPreventExitStatus=78`), and 0 on macOS, since launchd's `KeepAlive`
restarts every failed exit and cannot exempt one status. Any other failure
exits 1 and is restarted. It reads the credential written by `circus join` from
`$CIRCUS_HOME/config/worker.json`, or from
`$XDG_CONFIG_HOME/circus/worker.json` with a default of
`~/.config/circus/worker.json`. The file supplies the server URL, principal ID,
bearer token and optional CA bundle.

| Environment variable | Default | Values |
| --- | --- | --- |
| `CIRCUS_WORKER_MAX_SESSIONS` | `4` | Integer from 1 to 64 |
| `CIRCUS_WORKER_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |
| `CIRCUS_WORKER_IDLE_SECONDS` | `300` | Integer from 1 to 86400: how long an idle session keeps its lease. An idle session releases at once when the control plane asks (`ReleaseIdle`) because a session of the same provider waits for its `max_sessions` |
| `CIRCUS_BIN` | `circus` on `PATH` | The `circus` binary providing `internal exec-guard` |
| `CIRCUS_HOME` | unset | Moves the credential (`config/`), installed agents (`agents/`) and the packages `circus worker install` manages (`worker/`) under one directory |
| `XDG_DATA_HOME` | `~/.local/share` | Installed agents go to `$XDG_DATA_HOME/circus/agents`, and worker packages to `$XDG_DATA_HOME/circus/worker`, when `CIRCUS_HOME` is unset |
| `CIRCUS_NPM_REGISTRY` | `https://registry.npmjs.org` | The npm registry agent packages are downloaded from (a mirror with the same layout); the digest check is the same |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` (or lowercase) | unset | Proxy for agent downloads, chosen as Go's `http.ProxyFromEnvironment` does (see "Agents") |
| `CIRCUS_NODE` | `node` on `PATH` | The Node.js the launcher runs (24 or later) |
| `CIRCUS_WORKER_STOP_FILE` | `<data>/worker-state/stop.json` | Absolute path of the file the worker and its launcher leave the reason of a terminal stop in; `circus worker install` records it |

Claude Code runs with the environment of the worker (`CLAUDE_CONFIG_DIR` is
left alone): with the user's own login on this machine for a session
without a provider, with the provider's credentials otherwise (see
"Providers and the launch environment").

## Agents

Attach reports every agent this worker build supports with its state on
this machine (`AgentStatus`: not installed, installing with progress,
installed, outdated, failed, uninstalling); later changes go in
ReportAgents, progress at most once a second. The control plane sends the
user's wishes in `AgentsWanted` after every Attach and whenever one
changes, and the worker reconciles (`src/agents/manager.ts`): wanted at
this build's version and not installed →
install; wanted at another version → nothing; not wanted and on disk →
uninstall once no held session runs it (a FAILED install is retried only
on the user's request; see below). One operation per agent runs at a
time; a wish that no longer wants a running install stops it.

The pinned version and, per platform (`linux-x64`, `linux-arm64`, their
`-musl` variants, `darwin-x64`, `darwin-arm64`), the npm package's tarball
path and sha512 come from `package-lock.json` at build time
(`scripts/agent-manifest.mjs` writes `src/agents/claude-code.generated.ts`);
nothing is looked up at run time. The tarball is streamed to disk while
hashed, unpacked by a strict reader (regular files under `package/` only)
into a temporary directory next to the install, and renamed into place.
A failed install removes the previous files too and reports why.

A download is bounded: at most 512 MiB downloaded (the declared size is
checked first, then the running count), at most 1 GiB unpacked (checked
before each file is written), no more than 60 s without receiving data, 30
minutes in all, and 5 redirects. Each bound fails the install with an error
naming it ("download timed out (no data for 60 s)", "download took longer
than 30 min", "download larger than 512 MiB", "package larger than 1 GiB
unpacked", "too many redirects").

Downloads use a proxy as the circus CLI does (Go's
`http.ProxyFromEnvironment`): `HTTPS_PROXY`, else `https_proxy`, for an
https URL, through a `CONNECT` tunnel; `HTTP_PROXY`, else `http_proxy`, for
an http URL, as an absolute-URI request; none for `localhost` and loopback
addresses, nor for a host `NO_PROXY` (else `no_proxy`) exempts: `*`, an IP
address or CIDR range, `example.com` (itself and its subdomains),
`.example.com` or `*.example.com` (subdomains only), each optionally with
`:port` (an IPv6 address then in brackets; `[::1]` without a port matches
nothing). Name entries never match an IP address host. A proxy without a
scheme is `http://`; only `http://` proxies are supported (an `https://` or
`socks5://` proxy fails the download with a clear error), with credentials
in the URL sent as Basic authentication and never repeated in an error or
log. `worker/test/testdata/proxy-cases.json`, written by
`go run ./tools/proxycases`, holds Go's choices the tests compare with. Every
redirect chooses its proxy again. The worker's link to the control plane
never uses a proxy.

Installs and uninstalls hold a lock on the agents directory
(`<agents>/.lock`, a file created exclusively that names its holder's pid,
host and process start time), and so does a starting worker while it
removes interrupted installs: two worker processes sharing a data
directory never remove each other's files. A second process waits up to
60 s for the lock, then fails the operation naming the holder; at start it
waits 5 s and then leaves the temporary directories alone (a takeover
blocked by another process never makes it wait longer).

On the holder's own host, the holder process decides whether a lock is
stale: a lock naming a running process with the recorded start time is
never taken over, however long that process is paused (a debugger, a
suspended laptop or VM). A lock naming this process's pid that it does not
hold (a restarted container), a process that is not running, or a pid now
used by a process that started at another time is taken over at once. The
start time is read from `/proc/<pid>/stat` (with the boot id) on Linux and
from `ps -o lstart=` on macOS.

For a lock naming another host, an empty or damaged file, or one without a
start time, the file's age decides: while it holds the lock, a process sets
the file's modification time to now every 10 s, and a lock not refreshed
for 60 s is stale and taken over. This recovers from a holder killed in a
container that restarts with another hostname. It compares the holder's
clock with the waiter's, so hosts that share one data directory (a home on
NFS, say) need synchronised clocks (NTP): a host whose clock runs 60 s or
more ahead would take over a live lock.

Reports: one ReportAgents is in flight at a time and the latest state goes
next. A report that fails while the link is up (a refusal, the control
plane's store unavailable, a timeout) is sent again after 1 s, doubling up
to 30 s; one that fails because the link is down is not, since the next
Attach carries the state. A FAILED install is never retried by the worker
itself: each wish carries an attempt, which the control plane raises on
every install request of the user, and the worker installs a FAILED agent
again only for an attempt greater than the one its failure belongs to,
whether it arrives at once, with the next Attach (asked while the worker
was offline) or in place of the Attach's own AgentsWanted. A worker restart
starts over from what is on disk. A failed uninstall is retried after 1 s,
doubling up to 5 minutes, while the agent is still not wanted.

Every session starts the installed binary (`pathToClaudeCodeExecutable`);
a lease whose session is not a Claude Code session, or that arrives while
Claude Code is not installed at this build's version, is released with
ERROR and an error event. The SDK's own platform packages are never needed.

## Claude Agent SDK

The worker pins `@anthropic-ai/claude-agent-sdk` **0.3.330**, released with
Claude Code **2.1.330** (the version the worker installs), and its peers
`@anthropic-ai/sdk` 1.12.0, `@modelcontextprotocol/sdk` 1.32.2 and `zod`
4.6.7. Attach reports the SDK version; the agent status carries Claude
Code's.

Each session runs one `query()` on the main thread with: `cwd` the
`SessionConfig.cwds` entry of `default_device`; `settingSources: ['user',
'project', 'local']`; the `claude_code` preset system prompt, with a section
describing the session's ledger snapshot appended when the lease has a
`ledger_path` (`src/runner/ledger.ts`); the session's
permission mode (`auto` when empty); partial messages; `sessionId`, or
`resume` once the session has a transcript; the control-plane SessionStore
with eager flushing; a PreToolUse hook; the installed Claude Code
executable and the CLI launcher below; and, for a
session with a provider, the launch environment below. No tool list, MCP
server, alias or agent is set. The model is `SessionConfig.model`; when it
is empty, none is passed for local login, and a session with a provider
gets `opus[1m]` (see "Providers and the launch environment").
`persistSession` is left at its default (see below).

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
  input instead. The control plane's ledger notes (`ledger_problems`,
  `ledger_merged`, `ledger_discarded`) are pushed without it, as ordinary
  user messages starting with `[circus ledger]`, which the ledger section of
  the system prompt explains (the model refused the prefixed ones as
  possible injection); other control-plane notes stay synthetic.
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

## Providers and the launch environment

Before every CLI start (a new session, a resume, the replacement of a query
whose login failed or whose transcript was repaired) the runner calls
GetLaunchEnv, with the retries of any session read (a lost link is retried
after readiness); the reply is never kept for a later start. A new CLI
also waits until the previous CLI of the session has exited, and so does
every ReleaseLease. Once the CLI has ended on its own, no other CLI starts:
the session ends with ERROR.

- `local_login`: the CLI gets the worker's environment unchanged.
- A provider: the worker's environment without every variable that selects
  a credential, an endpoint or a model, plus the returned variables (which
  win over inherited ones of the same name), plus
  `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1`. Removed: every `ANTHROPIC_*`
  (`ANTHROPIC_MODEL`, `ANTHROPIC_SMALL_FAST_MODEL` and the alias variables
  included) and `CLAUDE_CODE_3P_*` variable; `CLAUDE_CODE_OAUTH_TOKEN` and its `_FILE_DESCRIPTOR`,
  `_REFRESH_TOKEN`, `_SCOPES`, `_CLIENT_ID` and `CLAUDE_CODE_CUSTOM_OAUTH_URL`;
  `CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR`, `CLAUDE_CODE_API_BASE_URL`,
  `CLAUDE_CODE_SESSION_ACCESS_TOKEN`; `CLAUDE_CODE_USE_{BEDROCK,VERTEX,FOUNDRY,
  ANTHROPIC_AWS,ANTHROPIC_GOOGLE_CLOUD,MANTLE,GATEWAY}` and the matching
  `CLAUDE_CODE_SKIP_*_AUTH`; `AWS_BEARER_TOKEN_BEDROCK`, `CLOUD_ML_REGION`;
  `CLAUDE_CODE_GATEWAY_TOKEN` (and `_FILE_DESCRIPTOR`); the host-auth
  hand-offs (`CLAUDE_CODE_HOST_AUTH_*`, `CLAUDE_CODE_HOST_CREDS_FILE`,
  `CLAUDE_CODE_SDK_HAS_*_REFRESH`, `CLAUDE_CODE_LAUNCHER_RUNS_API_KEY_HELPER`);
  the mTLS client certificate (`CLAUDE_CODE_CLIENT_CERT`, `_KEY`,
  `_KEY_PASSPHRASE`); `CLAUDE_CODE_RUNNER_MODEL_SOCKET` and
  `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL`; and the model choices
  `CLAUDE_CODE_SUBAGENT_MODEL`, `CLAUDE_CODE_SUBAGENT_MODEL_FORCE`,
  `CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL`, `CLAUDE_CODE_AUTO_MODE_MODEL`,
  `CLAUDE_CODE_BG_CLASSIFIER_MODEL`, `CLAUDE_CODE_MODEL_CATALOG` and
  `CLAUDE_CODE_MODEL_CATALOG_URL`. Proxy, CA and general AWS/GCP variables
  stay. Returned variables named `HOME`, `PATH`, `CLAUDE_CONFIG_DIR`,
  `CLAUDE_CODE_OAUTH_TOKEN`, `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST` or
  `CIRCUS_*`, malformed names and values with NUL are not applied (logged
  by name only).
- The model: with an empty `SessionConfig.model` a provider session passes
  `opus[1m]`, the alias Claude Code 2.1.330 uses by default with an API key
  or auth token on the Anthropic API (the opus alias target, with the 1M
  context where it applies), so the provider's
  `ANTHROPIC_DEFAULT_OPUS_MODEL` maps it and no `model` in a settings file
  chooses it. A non-empty model is passed as it is.
- `PROVIDER_UNAVAILABLE`: no CLI starts; the query is closed and the lease
  released with `PROVIDER`. Fenced, STOPPED and other rejections end the
  session as for any session operation.

The reply holds secrets: the worker itself puts no value in its logs,
events, errors or process records, only the provider id and the number of
variables (`test/runner/launch.test.ts` checks a debug-level run). Claude
Code hides the API key and auth token from the tools the model runs; the
provider's custom variables (and the base URL and alias variables) stay
visible to them, so a tool such as `env` can show their values in the
transcript and the session's events.

Measured with Claude Code 2.1.330 against the fake Messages API
(`internal/testutil/fakemodel`), the CLI in a private network namespace,
`settingSources: ['user', 'project', 'local']`, fake keys only:

- The `env` of the user's `settings.json` overrides the process
  environment: with `ANTHROPIC_BASE_URL`, `ANTHROPIC_API_KEY`,
  `ANTHROPIC_AUTH_TOKEN` and `ANTHROPIC_DEFAULT_OPUS_MODEL` there, the
  request went to the settings' URL with the settings' `x-api-key`, an
  `Authorization: Bearer` of the settings' token, and the settings' model.
  An `apiKeyHelper` in the user's settings added `Authorization: Bearer
  <helper output>` next to the process's `x-api-key`.
- Flag settings (the SDK's `settings` option) win over the user's settings
  for the names they set, but the user's other variables still apply (the
  user's `ANTHROPIC_AUTH_TOKEN` was still sent as a bearer). Flag
  `apiKeyHelper: ""` disables the user's helper.
- With `CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST=1` in the process
  environment, Claude Code ignores the provider, credential and model-alias
  variables and `apiKeyHelper` of every settings file, flag settings
  included: only the process's values were sent (`x-api-key` alone, or
  `Authorization: Bearer` alone, and the process's alias model). It also
  drops the user settings' proxy and certificate variables, and removes the
  API key and auth token from the environment of Bash commands (the
  provider's other variables stay). Hence the worker passes everything in
  the process environment with that variable set. It does not drop a
  top-level `model` of user, project or local settings (only of managed
  settings): with no model passed, the request carried the user settings'
  `user-settings-model`, or the project's `.claude/settings.local.json`
  `project-settings-model`, to the provider's endpoint.
- The default model: without any `model`, the request carried
  `ANTHROPIC_DEFAULT_OPUS_MODEL` (`provider-opus`) with the 1M context beta
  (`context-1m` in `anthropic-beta`), or `claude-opus-5-5` without that
  beta when the provider sets no opus alias, for an API key and for a
  bearer token alike. Passing `opus[1m]` gave exactly the same requests in
  all three cases (`opus` alone dropped the 1M context), and with
  `opus[1m]` passed, the settings' `model` (user or project) no longer
  applied: `provider-opus` with `context-1m` was sent. This matches the
  CLI's source, whose default is the opus alias target with `[1m]` where
  the 1M context applies.
- A provider's `ANTHROPIC_API_KEY` or `ANTHROPIC_AUTH_TOKEN` wins over a
  stored login (`.credentials.json`) and over an inherited
  `CLAUDE_CODE_OAUTH_TOKEN`: neither was sent. Without a key, the stored
  login's OAuth token goes to `ANTHROPIC_BASE_URL`, so the base URL is never
  applied without the provider's credential. With both a key and a token,
  both headers are sent.
- `persistSession: false` with a `sessionStore` is refused by the SDK
  (`query()` throws). Forcing `--no-session-persistence` through
  `extraArgs` mirrors nothing (no `append` call) and a resume from the
  store finds no history; without it every entry is mirrored and a resume
  keeps every uuid in order. The worker therefore keeps the default, and
  Claude Code also writes its transcript under `<config dir>/projects/`.

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

The reason is the permission mode's own, clipped to 1 KiB, or empty when
it gave none (a denial known only from `result.permission_denials`, or
one without `decision_reason`); clients word an empty reason themselves.
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
