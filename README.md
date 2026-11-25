# circus

circus runs an agent loop in a cloud worker and executes its tools on the
user's own machines through a small Go daemon; a Go control plane relays
calls between them, with Protobuf messages shared by Go and TypeScript.
Built so far: the device daemon (tool layer, dispatcher, jobs,
outbound link), the control plane server (enrollment, daemon and worker
links, call routing with at-most-once execution), and the `circus call`
debug client. The TypeScript worker is planned for M3 and not built yet.

| Path | Purpose |
| --- | --- |
| `proto/circus/v1/` | Device, link, enrollment, daemon-link and worker-link protocol messages |
| `docs/wire.md` | Language-neutral link wire protocol: transport, handshake, framing, limits |
| `buf.yaml`, `buf.gen.yaml` | Protocol checks and local code generation |
| `gen/go/circus/v1/` | Generated Go messages |
| `gen/ts/` | Generated TypeScript workspace package `@circus/proto` |
| `web/` | The Web UI (React, Vite); see [web/README.md](web/README.md) for running it against the control plane |
| `internal/controlplane/` | circus server: enrollment, auth, HTTP, TLS, health, timers, call-path composition |
| `internal/controlplane/calls/` | Metadata-only routing with boot-fenced at-most-once dispatch |
| `internal/controlplane/gateway/` | Authenticated daemon links, their shared wire budget and registry |
| `internal/controlplane/timers/` | Lease-based runner for durable control-plane timers |
| `internal/controlplane/workerlink/` | The worker/CLI side of the call path at /v1/link/worker |
| `internal/workerclient/` | Single authenticated worker connection used by `circus call` |
| `internal/daemon/` | Device daemon: tool registry, call dispatcher, background-job wiring |
| `internal/daemon/bashtool/` | Bash and TaskStop tools: snapshots, output caps, process groups |
| `internal/daemon/fstools/` | Native-compatible Read, Write and Edit tools |
| `internal/daemon/jobs/` | Background job lifecycle, ownership, events and retention |
| `internal/daemon/link/` | The daemon's single outbound control-plane connection |
| `internal/daemon/toolcontext/` | Execution timer signal shared by dispatcher and tools |
| `internal/daemonservice/` | Per-user systemd and launchd services for the device daemon |
| `internal/wire/` | Bounded protobuf transport over authenticated binary WebSocket, its scheduling table and the raw Hello handshake |
| `internal/wire/wiretest/` | Bounded in-memory duplex Conn with directional faults |
| `internal/store/` | Control-plane domain types and the Store interface |
| `internal/store/sqlite/` | SQLite store backend (WAL, synchronous=FULL, embedded migrations) |
| `internal/store/storetest/` | Store contract suite, fault injection and crash-restart harness |
| `internal/auth/` | Machine credentials, password and OIDC human login, setup tokens and browser sessions |
| `internal/config/` | Strict server YAML configuration and OIDC secret override |
| `internal/credfile/` | Local credential files written by `circus join` |
| `internal/machineid/` | Machine identity reported at enrollment |
| `internal/identifier/` | Wire grammar for call, session and agent IDs |
| `internal/loopback/` | The shared loopback-only plaintext rule |
| `internal/clock/` | Shared clocks and a manually advanced fake |
| `internal/textutil/` | UTF-8 decoding and UTF-16LE conversion helpers |
| `internal/testutil/leakcheck/` | Test leak detection for goroutines, processes, files and disk use |
| `cmd/circus/` | The circus CLI: server, join, daemon, doctor, call, version |
| `tests/e2e/` | Process end-to-end tests: real server, daemon and CLI binaries |
| `tests/parity/` | Captured native tool-behavior fixtures, the capture harness and behavior notes |
| `go.mod`, `go.sum` | Go module and pinned generator tool |
| `package.json`, `package-lock.json` | npm workspace and pinned Node dependencies |
| `Taskfile.yml` | Generation, lint, test, race, e2e, fuzz, seeds, build and drift checks ([Task](https://taskfile.dev)) |
| `.github/workflows/ci.yml` | Go, protocol, generation, TypeScript, e2e and race checks on every push |
| `.github/workflows/nightly.yml` | Nightly router seed sweep and e2e race |

Development requires Go 1.26, Node 24 with npm, Buf 1.73.0 and Task 3 on `PATH`.

```sh
npm ci
task gen lint test build
npx tsc --noEmit -p gen/ts
task check-gen
./bin/circus version
```

Generated files are checked in. `task check-gen` requires a clean `gen/` tree
after generation. Breaking checks are configured but will enter CI after there
is a release to compare against. Set the CLI version at build time with
`go build -ldflags "-X main.version=1.0.0" -o bin/circus ./cmd/circus`.

## Try it on loopback

Three terminals and one machine are enough: a plaintext control plane on
loopback, an enrolled device daemon, and `circus call`. `circus` below is
`./bin/circus` from `task build`; all commands were verified end to end.

```sh
# Terminal 1: write a local server config (plaintext, loopback only).
mkdir -p .tmp
cat > .tmp/config.yaml <<'YAML'
listen: 127.0.0.1:9543
db: .tmp/circus.db
dev_insecure: true
public_url: http://127.0.0.1:9543
YAML
circus server --config .tmp/config.yaml

# Open the setup link logged by the server and create owner@example.com.
# Terminal 2: issue an enrollment link for that user's email.
circus server enroll-token create --user owner@example.com --role both \
  --url http://127.0.0.1:9543 --config .tmp/config.yaml
# -> http://127.0.0.1:9543/join#<daemonToken>.<workerToken>

# Enroll this machine as both roles (paste the link, or pipe it in).
circus join --name desk

# Start the device daemon in the foreground. It reconnects after network
# and server failures. It exits if the server refuses its credential
# (HTTP 401/403) or rejects its handshake (such as an unsupported version).
circus daemon

# Terminal 3: run a tool on the device. Tool text goes to stdout; JSON
# status lines go to stderr.
circus call --device desk --cwd /tmp --wait bash 'echo hello from circus'
circus call --device desk read /etc/hostname
echo "new content" | circus call --device desk write /path/to/other.txt
circus call --device desk edit /path/to/note.txt --old circus --new circus-user
```

Query and cancel drive the exit codes. Cancel prints the server's single
reply, the call's state read after the cancellation was recorded: a query
of a call still running exits 4, cancel of a running call exits 0 with
`dispatched` and interrupts the tool (exit code 143, `Command was
interrupted`):

```sh
circus call --device desk --cwd /tmp --wait bash 'until [ -f /tmp/done ]; do sleep 2; done' &
sleep 1
circus call query <call-id>   # exit 4 while Accepted/Dispatched
circus call cancel <call-id>  # exit 0
```

The call IDs come from the first JSON line on stderr of each submit. Exit
codes:

- 0: a tool result (including `is_error` results), or a cancel answered
  Cancelled or Dispatched (intent recorded);
- 1: rejected (including an unknown device name, or a revoked credential),
  lost, offline, cancelled (also when `query` finds a cancelled call), an
  unknown call ID, usage, or another local error;
- 3: outcome unknown: an Unknown answer to a submit, `query` or `cancel`, or a
  transport failure or interrupt after the submit was sent (query the call ID
  after reconnecting);
- 4: `query` found the call still Accepted or Dispatched.

A cancel of a call that had already finished answers with that outcome and
exits as above: 0 for a result (the full tool output is printed to stdout,
fetched from the daemon) or for an earlier cancellation, 1 for lost,
offline or rejected.

`circus doctor` checks each role's stored credential against the server, one
line per check:

```sh
circus doctor
# [daemon] control plane http://127.0.0.1:9543, principal <id>
# [daemon] ok   reachable (healthz 200)
# [daemon] ok   ready
# [daemon] ok   credential accepted
# [worker] control plane http://127.0.0.1:9543, principal <id>
# [worker] ok   reachable (healthz 200)
# ...
```

`circus server device list --config config.yaml` and `circus server worker list
--config config.yaml` show each machine's owner email. Use `circus server device
revoke --user <email> <name> --config config.yaml` or the corresponding
`worker revoke` command to revoke an identity and free its name for that user.
The daemon's local harness also works without a server, reading
one protojson `DeviceCall` per line from stdin:

```sh
circus daemon call <<'EOF'
{"callId":"read-1","sessionId":"demo","read":{"path":"/etc/hostname"}}
{"callId":"bash-1","sessionId":"demo","bash":{"command":"echo hello from circus","cwd":"/tmp"}}
EOF
```

## Device daemon service

After `circus join`, use an installed `circus` executable to run the device
daemon as a per-user service. The service starts immediately and restarts
after failures. If the daemon cannot load its credentials or the control plane
rejects its credentials or handshake, the daemon logs the reason and stays
stopped. Each daemon state directory admits one daemon process at a time.
Daemons using the same credentials in different state directories displace
each other on the control plane. Stop a foreground daemon before installing
the service in the same state directory.

```sh
circus daemon install                   # optionally pass --credentials path
circus daemon status                    # includes the service file and log command
circus daemon stop                      # stop until the next boot/login or start
circus daemon start
circus daemon restart
circus daemon uninstall                # stop and remove the service
```

- **Linux (systemd)** writes `${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user/circus-daemon.service`.
  Read logs with `journalctl --user -u circus-daemon.service -f`.
  The service starts when the user logs in and stops at logout. Run
  `sudo loginctl enable-linger <user>` to start it at boot and keep it running
  after logout. Use the same `XDG_CONFIG_HOME` for subsequent service commands.
- **macOS (launchd)** writes `~/Library/LaunchAgents/com.github.ericwvi.circus.daemon.plist`.
  The agent starts at login and writes stdout and stderr to
  `~/Library/Logs/circus/daemon.log`.

Installation records the daemon's environment from the current shell,
including `PATH`, proxy and TLS certificate settings, XDG directories,
`CIRCUS_HOME`, `HOME`, `SHELL`, `USER` and locale settings. It resolves
`CIRCUS_HOME` and any explicit credentials path to absolute paths. Rerun
`circus daemon install` after changing the daemon's settings. Bash commands
rebuild their environment from login Bash startup files, using the daemon's
home, shell and `LANG`. `circus update` restarts a running service when it
replaces the executable that service uses.
On Linux, `circus update` may start a service that stopped itself with exit
status 78, even after a subsequent `circus daemon stop`.
A daemon newer than its control plane keeps retrying a revoked credential
until the server is updated to identify its authentication rejections.

## Development CLI

The CLI keeps its local files in the XDG directories: credentials in
`$XDG_CONFIG_HOME/circus` (`daemon.json`, `worker.json`), daemon state in
`$XDG_STATE_HOME/circus` and background command output in
`$XDG_CACHE_HOME/circus`. Set `CIRCUS_HOME` to move all three under one
directory instead (`$CIRCUS_HOME/config`, `$CIRCUS_HOME/state`,
`$CIRCUS_HOME/cache`; a relative value is relative to the working
directory). It changes only circus's own paths, so tools the daemon runs
still see the usual XDG defaults. The daemon keeps sessions' ledger
snapshots in the state directory too, at `ledger/<session>/` (docs/wire.md,
"Session ledger snapshots").

`task cli -- <args>` runs the CLI from source with `CIRCUS_HOME=.tmp/dev`,
next to an installed circus without sharing its credentials:

```sh
task cli -- join --name dev-box     # paste a link from the Web UI
task cli -- daemon
task cli -- doctor
```

Run the development daemon in the foreground with `task cli -- daemon`.
Service installation requires an installed executable.

## Operating the server

`circus server --config config.yaml` reads a required YAML file (default:
`config.yaml` in the working directory). Unknown keys are errors. Copy
[config.example.yaml](config.example.yaml) and choose exactly one transport
mode: `tls_cert` plus `tls_key`, `behind_proxy` for a TLS reverse proxy, or
`dev_insecure` for plaintext on loopback only. The only other server flag is
`--debug`; when supplied it overrides the `debug` key, including `--debug=false`.
`listen` defaults to `:8443`. `public_url` is the browser origin and the base
URL handed to joining machines; without it enrollment uses the request URL.
`public_url` must be an origin: URLs with a path (other than `/`), query or
fragment are rejected. Behind a proxy without `public_url`, the setup banner
prints a relative path and a hint to configure the browser origin.

On each start without a super user, the JSON log prints one setup banner
with a `/setup?token=...` link. This is the only logged secret: treat the log
as sensitive until setup is complete. The token exists only as a hash in
memory, changes on restart, and becomes unusable after setup. `AuthService.Setup`
creates the super user using email and an 8–72 byte password; `Login`,
`Logout` and `WhoAmI` serve browser sessions. Password hashes use bcrypt cost
12. Session cookies are HttpOnly, SameSite=Lax and Secure unless `dev_insecure`
is enabled. Idle sessions expire after 7 days; all sessions expire after
30 days. Expired sessions are deleted by the periodic cleanup. Failed password
attempts reserve both an email/IP bucket and an independent IP bucket before
bcrypt. The IP limit is 50 failures per 10 minutes, even when emails change.
Four password comparisons run concurrently; requests queue for at most 3 seconds
or until their context ends. Queue timeout returns `RESOURCE_EXHAUSTED`.
With `behind_proxy`, limiters use the
rightmost `X-Forwarded-For` entry when it is a valid IP, falling back to the
peer address otherwise. The proxy must append or replace that header. The
first request without this header logs one warning per server run. State-changing requests reject an
Origin different from `public_url`;
non-browser requests without Origin are accepted, as in homepage.

OIDC is optional. Configure `oidc.issuer`, `oidc.client_id`, and optionally
`oidc.client_secret` (prefer `CIRCUS_OIDC_CLIENT_SECRET`, which overrides the file value only when
non-empty), plus `oidc.display_name`. OIDC requires `public_url`; register
`<public_url>/auth/oidc/callback` with the provider. Login starts at
`/auth/oidc/login?next=/`; setup via OIDC also supplies `setup_token`.
Discovery is lazy so the server starts while the provider is offline.
PKCE, state and nonce protect the flow. Normal OIDC login is disabled until
the super user has been created. Verified UserInfo email can bind an existing
account only if it has no identity at that issuer yet; a different subject at
the same issuer is rejected as `account_conflict`. A password-created super
user can therefore bind its first OIDC identity. This trusts the configured
IdP to mark an email verified only when that account owns the address.
Existing issuer/subject identities continue to identify their original user.

`ProjectService` and `MachineService` (`proto/circus/v1/project.proto`,
`machine.proto`) use the same cookie and Origin rules. Any signed-in user
can call `MachineService.CreateEnrollLink` to enroll devices, workers, or both
for their account. The link uses `public_url`, expires after one hour, and
carries one token per role. Web issuance allows up to 16 unused, unexpired
tokens per user. CLI-issued tokens count toward this allowance, and the CLI
can issue tokens regardless of that count. A user can enroll up to 32 live
devices and 4 live workers. `ListDevices` lists only the caller's devices,
including when the caller is the super user. `SetWorkerGrant` turns the
integrator grant of one of the caller's workers on or off (see Ledger
below); `ListWorkers` shows it. The grant ends when the worker is revoked.

Machines joined before this version must join again using a link issued for
their Web account. Run `circus join --force` to replace the machine's old
credentials.
A project belongs to one Web user and has a name (trimmed, 1–64 characters,
unique per user ignoring case), an optional git repository URL and 0–16
locations. A location is a device and, optionally, an absolute path
normalized for that device's OS; without a path it stands for the device's
default directory `~/circus/projects/<project_id>` (at most one per device),
reported as `effective_path`. A user's projects hold at most 1000 entries and
4 MiB of names, repository URLs and paths together. `UpdateProject` with
`expected_updated_at_unix_ms` applies only to the version the caller read
and otherwise answers `ABORTED`. Projects bind devices owned by their
Web user. A device's `online` comes from the live daemon links of this process.

`SessionService` (`proto/circus/v1/session.proto`) serves sessions to the
Web UI with the same cookie and Origin rules. A session belongs to the Web
user who owns its project. Only workers of that user serve its sessions,
and session quotas apply per user. Every call resolves the session through a
project of the caller and answers `NOT_FOUND` for anything else, and
`ListSessions` without a project lists only the caller's projects'
sessions. `CreateSession` starts in a project location (`location_index`,
or the project's last used one, which it then records) on a device of the
caller that is not revoked (`FAILED_PRECONDITION` otherwise); the server
fills `default_device` and the working directory (the location's path, or
its default directory, not created yet). The server also decides local
mode (`config.local`, which clients may not set): a session whose worker
shares the device's machine (the `pinned_worker_id` given, or else the
caller's live worker on that machine) is local and pinned to that worker,
which then runs Claude Code's built-in tools on the machine directly.
`SendMessage` records the
message as a durable `KIND_INPUT_ACCEPTED` view event and returns its seq; a repeated
`client_msg_id` returns the same input and seq. `WatchSession` replays the
durable events after `after_seq`, then follows live events, ephemeral ones
included; idle streams get a keepalive every 25 s (an ephemeral event of
kind `KIND_UNSPECIFIED`). A stream ends after about an hour (±10 %), and within 30 s of
its browser session being signed out or expiring. At most 32 streams per
Web user and 256 in all.
See `docs/wire.md` section 7, "User-side API".

`worker_connections` (default 8, at least 1; zero selects the default) bounds
authenticated worker connections. Each Web user and each credential may
hold at most 4 connections. Two users can fill the default global worker
connection cap. Increase `worker_connections` as the number of users grows.
A full cap answers HTTP 503. Each connection may have 64 operations open.

Each user gets half of each global router budget for subscription slots,
pending call count and bytes, and buffered update bytes. This keeps one
user from exhausting those budgets alone. Two users together can still
fill them.

The SQLite database defaults to `$XDG_DATA_HOME/circus/circus.db`
(`~/.local/share/circus/circus.db`). Admin subcommands (`enroll-token`,
`device`, `worker`) also accept `--config` and read its `db` key; they open
only an existing database.

Model provider secrets (API keys and custom variable values) are stored
encrypted with AES-256-GCM. The key is `provider.key` beside the database
(`provider_key_file` overrides the path): 32 random bytes created with mode
0600 on first start. The server refuses a key file of another size or one
readable by group or others. Back it up with the database; if it is lost or
replaced the server still starts, but every provider reports its secrets
unreadable and its sessions wait until the user enters the key and the
variable values again.

Memory. All daemon links share ONE `wire.Budget`, sized by
`send_pool_bytes` and `receive_pool_bytes` (0 = the default, twice
`wire.MinBudget`; the server refuses smaller pools and names the key and
minimum). Sizing rule: with the default 64 MiB message limit,
one link's minimum is about 262 MiB receive (4·M + 6 MiB + 4 KiB), so the
pools start at twice that. Let K be the number of devices that may stream
large (8–64 MiB) results at the same time; size receive at
max(2·Rmin, K·Rmin) plus headroom — large links measured about 250–300 MiB
each, and the conservative K × 462 MiB (one link's peak receive phase)
showed no starvation at 4/16/64 devices. `ErrBudgetExhausted` errors or
forced reconnects on large-result links mean K exceeds what the pool was
sized for. There is no per-link share yet: a few large transfers can delay
or starve other devices' results when K is not bounded; a per-link cap
(Rmin) plus a small-message reserve is planned before multi-user (M4).

The daemon-link pools are not the whole process budget. At the default
64 MiB message limit (M), live ownership bounds are:

| Part | Bound | Arithmetic |
| --- | --- | --- |
| Daemon links, wire | about 1,286 MiB | pools 256 + 524 MiB (2·Smin, 2·Rmin) plus 100 sockets × (5 MiB + 64 KiB) = 506 MiB; one borrowed inbound message per socket comes on top |
| Worker links (own fixed pools; scales with `worker_connections`, default 8) | about 2,557 MiB | pools 256 + 524 MiB; 8 sockets × (5 MiB + 64 KiB + one borrowed inbound message of 2·M + 6 MiB + 4 KiB) = 1,112 MiB; 4 push permits shared by all worker sockets, each one large outbound message (2·M + 6 MiB + 4 KiB) = 536 MiB; small outbound updates (≤ 256 KiB charged) sent without a permit, at most one per operation: 8 × 64 × 256 KiB = 128 MiB |
| Call router | 512 MiB | pending requests 256 MiB plus distinct buffered updates 256 MiB (one global charge per shared immutable clone) |
| Session service (worker-link side) | about 1,094 MiB | 2 read replies built at once, each ≤ 2·M = 128 MiB, so ≤ 256 MiB together; per worker socket one inline operation's converted request (≤ M) plus its queue of inline replies (≤ 16 MiB encoded, about 32 MiB decoded): 8 × 96 MiB = 768 MiB; session table, Wake queue, provider scan and reclaim, stale-read and fallback ids, hint queues and extension keys ≤ about 20 MiB; log record batches held at once: (8 + 1) × 5.4 MiB = 49 MiB; see `internal/controlplane/sessions/doc.go` |
| Session watch streams | about 888 MiB | 256 streams × about 3.47 MiB each: the bus queue and the batch being delivered (2 × 272 KiB), one replay page (about 294 KiB), the stored event being converted (256 KiB, plus 512 KiB when its UTF-8 must be repaired) and its JSON encoding (up to 1.5 MiB, in a pooled buffer of up to about 1.9 MiB; SessionService does not compress responses); see `internal/controlplane/sessions/bus.go` |
| Ledger | about 1,291 MiB | `LedgerService`: 4 requests, of which at most 2 read a session's ledger (about 95 MiB each) and the others about 45 MiB = 280 MiB; the 3 ledger write slots (checks, merges and conflict reads, sessions' collections and rebases included) of up to about 199 MiB each plus 4 lighter changeset requests of up to about 36 MiB = 741 MiB; session snapshot operations, 4 at once, each holding up to about 67 MiB of device results and requests = 268 MiB; snapshot queues under 2 MiB; see the Ledger section below |

That is about 7.4 GiB (4,355 MiB for the links and the router, 1,094 MiB
for the session service, 888 MiB for watch streams, 1,291 MiB for the
ledger: 7,628 MiB) before SQLite, TLS/kernel buffers, bounded operation scratch, the
runtime and GC headroom; set `GOMEMLIMIT` above it, for example
`GOMEMLIMIT=8GiB` with at least 9 GiB of memory for the process. The daemon connection limit (100 sockets)
is fixed; worker sockets default to 8 (`worker_connections`, at most 4 per
user and per credential), each adding about 139 MiB to the worker links and 101 MiB to the
session service (96 MiB of inline operations and 5.4 MiB of log records) at the default message limit. Live-only session events
handed to the bus are charged to each watcher's queue (at most 256 KiB of
payload each) and counted in the watch streams' figure. See
`internal/controlplane/gateway`, `internal/controlplane/workerlink`,
`internal/controlplane/sessions` (doc.go and bus.go) and
`internal/controlplane/calls/DESIGN.md`.

### Ledger

Each project with ledger entries has a bare Git repository at
`ledger/<project_id>.git` beside the SQLite database (`/data/circus/ledger/`
in the container). Back up these repositories with the rest of the data
volume. At startup the server rebuilds any stale SQLite ledger index from
the repository's main line and removes repositories for deleted projects.

A snapshot permits up to 4096 decision files of 256 KiB each, about 1 GiB of
uncompressed data plus the generated index. CommitMain scans the complete
submitted snapshot to validate and hash it. It reads prior bodies when
their blob hashes differ, reuses unchanged subtrees, streams new blobs one
at a time, and updates the changed index rows. The caller owns the input
bodies. Repository work uses bounded file buffers and metadata for up to
4096 decisions and their directory paths. Git objects and the SQLite
history index grow with recorded changes.

Each new Git object is compressed into a temporary file, fsynced and only
then renamed to its final name, and main moves only after the object
directories are fsynced. An object file that already exists but is not
reachable from the parent commit, for example one left by a crash, is
inflated and hashed before it is reused, and replaced when it is wrong. A
blob reused from the parent only has to exist and be nonempty, unless an
earlier read of it failed; then it is verified in full as well, so the
next commit that supplies its bytes repairs it.

On Linux amd64 with Go 1.26.4, a local run with 4096 files of about 256 KiB
each in 16 directories measured the following. Allocation is cumulative
within the operation. The peak RSS of the run with repeating text was
1.21 GiB, including the roughly 1 GiB of caller-owned input; the run with
random text peaked at 1.74 GiB because generating its input leaves garbage.

| Full-size workload | Time | Allocated |
| --- | ---: | ---: |
| Initial commit, repeating text | 3.95 s | 41 MiB |
| One changed body, repeating text | 0.92 s | 12 MiB |
| All bodies changed, repeating text | 6.27 s | 125 MiB |
| Initial commit, seeded random Base64 text | 17.66 s | 41 MiB |
| One changed body, seeded random Base64 text | 0.88 s | 12 MiB |
| All bodies changed, seeded random Base64 text | 25.53 s | 124 MiB |

Compression, fsync latency and directory depth affect wall time.

A rebuild replays main's first-parent line from Git before it writes to
SQLite, and spools 20 bytes per commit plus the encoded versions on
temporary disk. When the indexed head is on that line and at most 4096
versions follow it, one transaction keeps the indexed prefix and adds the
rest. Otherwise the index is replaced in transactions of at most 64 rows
each: the new rows are staged under a new generation, readers keep the
previous complete index until a final one-row update switches them over,
and the old rows are deleted in batches afterwards. Rows staged by an
install that failed are deleted right away, even when its request was
cancelled; rows left by a crash are deleted by the next commit or
replacement. Deleting a project, or a user with projects, first clears
its ledger index and deletes the rows in transactions of 64 rows, then
removes the project; rows written meanwhile are deleted the same way
first. These steps continue when the request ends. If the project
survives, for example because a session was created meanwhile, the next
read rebuilds its index from the repository. In the same local run,
replacing an index of 102,400 versions took 4.1 s while other store calls
waited at most 7 ms.

The server isolates unreadable repositories during startup. A read of such
a project returns an internal error and, once a retry delay has passed,
starts a check of the repository, so a transient error clears without a
restart. The check runs apart from the request, with a budget of ten
minutes, which covers rebuilding a long history. One check per project
and two in total run at a time, and the server stops them on shutdown. The delay starts at one
second and doubles after each failed or timed-out check, up to one minute.

The Web API serves up to four authenticated, decoded ledger requests at
once, at most two for one user: one user's clients that stop reading cannot
keep others out, though two users' clients together can. Each response is
bounded by its procedure, with a full listing capped at about 31 MiB of
encoded data. A body over a procedure's limit is refused without reading
the rest of it.

#### Changesets

The ledger changes only through changesets (`LedgerChangesetService` in
`proto/circus/v1/ledger.proto`), which `circus ledger` drives with the
worker credential from `circus join`:

```sh
circus ledger open <project>             # prints the changeset id and its directory
# edit, add, move files in the directory with any tool
circus ledger check <changeset>          # problems, fast-forward, conflicts
circus ledger merge <changeset> [--commit <sha>] [--resolved <dir>]
circus ledger discard <changeset> --reason <text>
circus ledger list <project> [--state open|merged|discarded] [--session <id>]
```

`open` writes the main line's files to
`$XDG_STATE_HOME/circus/ledger/cli/<changeset>/` (under
`$CIRCUS_HOME/state` when that is set), or `--dir`, and a manifest of their
sizes, modification times, inodes and hashes beside it
(`<changeset>.manifest.json`), so `check` and `merge` find the changes by
the id alone and read only the files whose size, modification time or
inode changed (its inode change time included, as git does). Symlinks
and other non-regular files, files over 256 KiB and invalid UTF-8 are
never followed or sent: `check` prints them as `problem:` lines, and
`check` and `merge` stop with exit 3, as for the server's problems.
A new decision is `<dir>/D?<letters>-<title>.md` with `number: D?<letters>`
(one to four lowercase letters, unique in the changeset), and other changed
files may refer to it by that number. A merge gives new decisions the next
numbers, in the order a, b, …, z, aa, …, renames their files, and replaces
every reference `D?<letters>` without a letter, digit or underscore next to
it in every changed file. References to letters no new decision has are
kept, and `check` warns of them without stopping the merge. Merged files
are written with the canonical front matter, and every changed file's name
is derived again from its title: a title that refers to a new decision
renames its file.

Each check replaces the changeset's content on the `changesets/<id>`
branch, committed with its opener as author on top of its base; a check
without changes removes the branch. A discarded changeset's branch moves to
`refs/discarded/<id>`, on a commit that records the reason. The worker
that opened a changeset may check, discard and merge it while it
fast-forwards: its base's tree is the main line head's tree, which for a
changeset opened here means its base is the head. Otherwise only a worker
with the integrator grant, set on the Web devices page, may merge it, even
when the main line changed other files only, and every file that both
changed after the base must be resolved: `check` writes the versions that
exist to `<changeset>.conflicts/<path>.base`, `.changeset` and `.main` in
the state directory above, whatever `--dir` is, and `merge --resolved
<dir>` sends `<dir>/<path>`, or removes the file for an empty
`<dir>/<path>.deleted`. When the conflicts' contents exceed what one
response carries (8 MiB), the check reports the rest without contents, and
`check` reads them with `ReadChangesetConflicts`, in pages, from the
changeset's branch and the main-line commit the check compared with. Each
page after the first is bound to the branch commit the first was read
from. When the main line moved meanwhile, or the changeset was checked
again between pages, it writes nothing for them and says to check again.
`merge --resolved` does not read them. A page reads the trees of the base,
the main line and the branch to find the conflicts, about 3.5 MiB at most,
and then the blobs of its own conflicts and at most one more, so a check
reads each conflicting file's versions about once: at the bounds, about
2 GiB of contents (a base and a main-line version of up to 256 KiB for each
of 4097 files, and 8 MiB of the changeset's), and with a binary client about
260 pages, about 0.9 GiB of trees. A JSON client, whose pages fill up to six
times faster with control characters, reads trees for up to about 1,500
pages, about 5 GiB. The merge validates the
resolutions like any change, checks everything again under the project's
lock and is one ordinary commit on the main line; a merge that would change
nothing, as when every conflict is resolved to the main line's version, is
refused like a changeset without changes. Merging a merged changeset again
returns the same result, and `circus ledger merge` of a merged changeset
prints its numbers again.

A merge records the commit it is about to publish, with the numbers it
assigned, before main moves. A merge interrupted after main moved is
recorded as merged by the next operation on the project, by reading it or
at startup, and one interrupted before is forgotten; an interrupted archive
is finished the same way. A changeset whose repair fails, as when one of
its objects is damaged, is logged and refuses its own operations until a
repair succeeds; the project's other changesets stay usable, since the main
line remains what later merges build on. Listing and getting it show the
state as recorded, with the reason in `repair_failure`; this covers a
discarded changeset whose archive cannot be finished too. Changeset rows are deleted with their project, after it and in
transactions of 64 rows; rows of a deleted project are never read, and
startup deletes any that a crash left.

A project has at most 64 open changesets. A check carries at most 8 MiB of
changed content in at most 8194 files, and a merge's resolutions as much
again. `LedgerChangesetService` admits up to four requests at once, two per
user, before reading their bodies, and of them at most three checks,
merges or conflict reads, one per user. One user's clients that stop
sending or reading hold only that user's share: keeping everyone else out
takes the clients of two users together (every request slot), or of three
(every check and merge slot). A refusal, of a credential or for want of a
slot, is answered at once and closes the connection, without waiting for
the request's body; after a request, the server waits at most a second
for any of its body the handler left unread. A refusal for want of a slot
carries the ErrorInfo reason `LEDGER_BUSY`; `circus ledger` then tries again
with jittered exponential backoff, from a quarter of a second up to five
seconds, for up to two minutes, and says once that it waits, so agents of
one user that check or merge at the same time take turns. The worst cases
at these bounds, in memory held at once:

| Request | Memory |
| --- | ---: |
| Check: request buffer 32 MiB (it grows by doubling, so about 48 MiB while the last 24 MiB body is read), message 13 MiB, file copies 8 MiB, base and main snapshots 14 MiB with the index read once (up to 12 MiB), conflict contents 8 MiB and their copies 8 MiB, response 21 MiB, object cache 4 MiB | ≈ 120 MiB |
| Merge: request buffer 32 MiB and message 13 MiB, resolutions 8 MiB, snapshots and index 26 MiB, changeset content 8 MiB, rewritten files 48 MiB (a reference of at least 4 bytes grows to at most 12), object cache 4 MiB | ≈ 140 MiB |
| A conflicts page: contents 8 MiB, or a single conflict alone up to 30.5 MiB (two versions of the generated index and an 8 MiB changeset file), their copies 30.5 MiB, the encoded page up to 70 MiB (six bytes per control character), which JSON encoding grows by doubling, so about 126 MiB while the last copy is made, the three file lists about 8 MiB, object cache 4 MiB: 30.5 + 30.5 + 126 + 8 + 4 | ≈ 199 MiB |
| A base page: files 8 MiB, or the generated index alone up to 12 MiB, their copies and the encoded page | ≈ 36 MiB |
| Open, discard, list, get | < 1 MiB |

The three write slots are the server's, shared with the session
snapshots below, whose collections and rebases wait for one instead of
being refused. With every write slot held by a conflict read (3 × 199) the
four admitted requests can all be lighter ones, such as base pages or a
session changeset's check after its collection (4 × 36): the service and
the snapshots' engine work hold at most about 741 MiB. With the four
requests of `LedgerService` (a full listing about 45 MiB each with its index
rows and message; a session's ledger about 95 MiB, of which at most two
run at once, see below) and the snapshot operations (268 MiB), the ledger
holds at most about 1,291 MiB. These are live bytes: with the default
`GOGC=100` and no `GOMEMLIMIT` the heap may grow to about twice the live
data before a collection, about 2.6 GiB. With `GOMEMLIMIT` set as the
process budget above recommends, that budget, which counts these
1,291 MiB, applies instead.

#### Session snapshots

Every session created on a server with a ledger gets a **ledger snapshot**:
a copy of the project's ledger on the session's device, outside the
workspace, at `$XDG_STATE_HOME/circus/ledger/<session>/` (docs/wire.md,
"Session ledger snapshots"). The control plane prepares it from the main
line's head when the preparation starts, sending the files in chunks of at
most 8 MiB (and half the message limit) and 4096 files, the generated
`INDEX.md` apart (up to about 11 MiB, the most 4096 decisions make). Every
snapshot holds `INDEX.md`, that of a project without decisions included:
the empty ledger (no commit yet) is its index alone, an empty table that
says the ledger has no decisions yet, so the agent never has to look for
it anywhere else. The session's first turn waits while the device is offline or the snapshot is
prepared, as for a workspace, and the worker gets the snapshot's path in
the lease (`Lease.ledger_path`) and tells the model about it. A failure,
a daemon without the ledger calls or a revoked device included, leaves the
session without a ledger (`FAILED`). Sessions created before snapshots
existed have none.

After every turn the control plane collects the snapshot in the
background: the daemon compares it with its manifest and sends the
contents only of files that differ from what the control plane already
holds. The changes become the session's changeset, opened by the first
collection that finds any (at most one open per session, no opener,
counted in the project's 64 open changesets); a collection is skipped when
a new turn has started and runs when that one ends, and one due while the
device is offline runs when it connects (the session's status shows
`collection_due` meanwhile, and the turn's end is announced as a
`KIND_LEDGER` event). Nothing changes a snapshot while a turn runs. At most
four snapshot operations run at once, two per device; the others wait in a
queue that holds only session ids, and work that finds its device busy is
queued again when one of the device's operations ends. Work for an offline
device does nothing but a cheap check until the device connects; a revoked
device fails the snapshot. The check of a collection takes one of the
ledger write slots and waits for it rather than failing. Problems (a file
that is not a decision, a changed number, a removed decision, a symlink or
a file over 256 KiB, or two paths that differ only in case or Unicode
normalization, which are one name on macOS; ledger validation refuses
those in every changeset) are told to the agent in a note,
`ledger_problems`, which starts a turn when the session is idle; the same
set of problems is not told again. Each such note starts a turn without
the user, so after three of them in a row with no input of the user
between them, new problems are told only after the user's next input: an
agent that cannot fix its snapshot does not run on forever. The notes of a
merge and a discard (below) start no turn: they wait, pending, for the
next input that starts one (the user's message, an approval's retry or a
problems note) and reach the agent in that turn, before it; they do not
count towards the three. A turn the CLI starts by itself (a background task
ending) takes no input and so carries none of them. Held notes count towards
no bound of the session's open inputs, so they never keep the user's
messages or an approval's retry out; at most eight are held per session,
and one more folds them into a single note telling the agent that earlier
ledger changes were merged or discarded and to re-read the snapshot's
`INDEX.md` before relying on decision numbers. If a lease is lost after
the CLI got the input a held note was taken with but not the note itself,
the note comes back as one that starts a turn: it can no longer go before
that input, so it goes as soon as possible. The reason an integrator gives
for a discard is quoted in the note as data (escaped, at most 300 bytes),
marked as not an instruction. A note is written in the transaction that records
what it tells, so it is given exactly when that is recorded, and it survives
a worker's restart, a lost lease and a stopped session like the user's
inputs. A held note is never dropped silently: past the bound its text is
replaced, together with the other held notes', by the one summary note
above, which carries none of their details. A session takes notes before
the user's messages that arrived with them. On a lost lease, an input
counts as received when the transcript holds an entry with its id or the
`queued_command` attachment Claude Code writes for a message it absorbed
into a running turn (whose `source_uuid` is the input's id); such an input
is not given again.
The worker pushes ledger notes to the agent as ordinary user messages,
not as synthetic ones (which Claude Code marks as not coming from the
user), each starting with `[circus ledger]`; the system prompt's ledger
section tells the agent that such a message comes from circus's ledger
collector on the user's behalf.

The project's owner merges a session's changeset from the session page
(`LedgerService.MergeSessionLedger`) when it fast-forwards; the server
collects the snapshot first and refuses a merge while a turn runs, while
the device is offline after a turn ended, with problems, or for another
changeset than the one shown. Otherwise an integrator merges it with
`circus ledger merge`: `circus ledger list <project> --session <id>` lists
a session's changesets, and `check`, `merge` and `discard` take a session's
changeset by its id without a local copy (the server collects the snapshot,
or with the device offline uses the last collection and says so); only a
worker with the integrator grant may change a session's changeset.

After a merge the control plane builds the session's **view**: the
changeset's base with the merged files (new decisions renamed, their
references rewritten, conflicts as resolved), kept on `refs/sessions/<id>`
as a commit on top of the merge commit. The next changeset starts from the
view, so it fast-forwards until someone else merges, and the snapshot
never takes in others' changes. An integrator's merge or discard of a
session's changeset is refused (`TURN_RUNNING`) while a turn of the
session runs or is about to start (its worker took inputs for it), as the
owner's is, checked in the transaction that records the merge or the
discard; once it succeeds, the session's turns are
held until the snapshot follows, as for a preparation, the session's status
says so (`applying`), and a worker that keeps its lease takes no inputs
meanwhile and is not nudged to start a turn (the session is woken when the
hold ends). So between the merge's collection and the rebase only a
process the agent left running in the background, or a turn the Claude CLI
starts on its own (a subagent or background task finishing, which nothing
can hold back), can change the snapshot. The control plane collects the
snapshot and applies the merge's numbers to what the agent holds now
(`rebase_ledger`): a new decision whose temporary number the merge
assigned becomes `D<n>` where the agent left it, under its current title,
its references rewritten as the merge rewrote them, also when the agent
moved, retitled, revised or rewrote it after the collection the merge
used; other files keep the agent's content with the temporary references
replaced. A file stands for the merged decision only when it is at the
decision's collected path, or that path is gone and it is the only file
with the decision's temporary number; any other file with that number is
another new decision, which gets fresh letters while the merged file is
kept, and the agent is asked to check it; its references to the reused
letters stay, and are problems of the next changeset ("refers to D?a,
which no decision of this changeset has") until the agent resolves them. A merged new decision the agent
deleted is put back, since the ledger keeps every decision. When nothing
changed since that collection the snapshot is the view; otherwise its base
is a commit on top of the view holding those later changes, which then go
into the next changeset as changes of `D<n>` (never a second new
decision). The agent is told (`ledger_merged`) with one list of the
renames, what was put back and what to check, in its next turn.
Every rebase's base commit names exactly the files it installs, and is
recorded before it is sent; each collection and rebase answer names the
base the snapshot's manifest records (`current_base_commit`), so a
collection on that commit finds an attempt whose answer was lost
finished, and one on a base the control plane does not expect is collected
again relative to it rather than mixed with the old base. A rebase
refused as stale (the snapshot changed meanwhile) is planned again from a
new collection. Files a collection cannot take (a symlink, say) stop the
rebase: the agent is told, its turns are not held meanwhile, and the
rebase is tried again at the end of its next turn or after a backoff. A
rebase or reset the device keeps refusing (permissions, a full disk, a
file in the way, or a snapshot that keeps changing) is retried after a
backoff, and after three failed attempts in a row (about 15 seconds) is
recorded as a problem of the snapshot the same way, so a session is never
held without a word. A file the daemon could not put back
(`<name>.conflict-<hash>`) is collected as a file that is not a decision,
and the agent is told what to do with it. After an integrator discards a
session's changeset the snapshot is written again from the view, between
turns as well, and the agent told (`ledger_discarded`). Deleting a session
whose changeset has changes, or whose collection is due, needs
`discard_ledger_changes`; once the session is deleted the changeset is
discarded (a periodic sweep, one page at a time from where the last one
stopped, discards what a failure left open) and the snapshot removed from
the device, when the device connects if it is offline (a removal waits at
most 30 days). Deleting a project removes its sessions' snapshots the same
way. A daemon without the ledger calls fails the snapshot for good: the
session runs without a ledger even after the daemon is updated.

The snapshot's memory: a collection holds the device's result (8 MiB of
contents, 4096 paths of up to 4 KiB with their hashes, 4096 reverted paths
and 1024 problems: about 46 MiB), a copy of its contents for the check
(8 MiB; the decoded result is dropped before the check) and the known
changes it sent (about 5 MiB); a rebase holds first such a collection
(its contents are dropped before the rebase is sent), then its request:
edits of at most 8 MiB plus the generated index (about 11 MiB; a larger
merge fails the snapshot) and the known changes, copied into the request
and encoded (about 48 MiB); a preparation holds one chunk, or the
generated index alone, and its copy in the request: at most about 67 MiB
for one operation, 268 MiB for four. The engine work of a collection's
check (about 70 MiB) and of a rebase's plan (the view, about 106 MiB with
the index, and the agent's later changes renumbered, up to 8 MiB more:
about 114 MiB) runs in a write slot, below a conflict read's 199 MiB. `GetSessionLedger`
reads the changeset's base and its changed files one at a time (about
27 MiB) and answers with up to 4096 entries (titles cut to 512 bytes, their
control characters replaced, so JSON at most doubles them), the last
collection's problems and warnings and the merged changesets' numbers:
about 28 MiB of JSON at most, about 95 MiB with its message and the
encoding buffer's growth. At most two session ledger reads run at once,
one per user, within the four `LedgerService` requests.

## Releases and deployment

Pushing a `v*` tag publishes the Linux amd64 image to GitHub Container
Registry and CLI archives for Linux and macOS on amd64 and arm64 to
[GitHub Releases](https://github.com/EricWvi/circus/releases).

The image serves the embedded Web UI, APIs, and links from one origin. Copy
`config.example.yaml` to `config.yaml`, set `listen: "0.0.0.0:8443"` inside the
container, and set `public_url` to the browser-facing origin. Set
`behind_proxy: true` when a TLS reverse proxy terminates HTTPS. The port
mapping below is for a proxy running on the Docker host. Make the mounted
configuration readable by UID 65532. For OIDC, you can pass the client secret
in the container environment as `CIRCUS_OIDC_CLIENT_SECRET`.

```sh
docker run --rm --name circus -p 127.0.0.1:8443:8443 \
  --mount "type=bind,source=$PWD/config.yaml,target=/etc/circus/config.yaml,readonly" \
  --mount type=volume,source=circus-data,target=/data \
  ghcr.io/ericwvi/circus:latest
```

Open the setup link logged by the container and create the Web user before
issuing an enrollment link for that account.

```sh
docker exec circus /usr/local/bin/circus server enroll-token create \
  --config /etc/circus/config.yaml --user owner@example.com --role both \
  --url https://circus.example.com
```

To install the CLI, download the archive for your OS and architecture and
extract its `circus` binary into a directory on your `PATH`. The CLI archives
omit the Web UI, so `circus server` from an archive serves only the APIs.

```sh
tar -xzf circus-vX.Y.Z-linux-amd64.tar.gz
mkdir -p ~/.local/bin
install -m 0755 circus ~/.local/bin/circus
```

Run `circus update` to install the latest CLI release. It verifies the
download's SHA-256 before replacing the executable. Use `circus update --force`
to install an older release when GitHub's latest release is older than yours.

## Testing tiers

| Tier | Command | Where |
| --- | --- | --- |
| Unit | `task test` | every push/PR (CI `check` job) |
| Race | `task race` | every push/PR (parallel CI `race` job) |
| Process e2e | `task e2e` (`-count=1`) | every push/PR (CI `check` job) |
| E2e race | `task e2e-race` (`-count=3`) | nightly |
| Router seed sweep | `task seeds` (500 seeds) | nightly, 5 × 100-seed segments |
| Fuzz targets | `task fuzz` (FUZZTIME, default 30s) | local, on demand |

The fuzz targets today are `FuzzDecode` and `FuzzReassembly` in
`internal/wire/`. Measurements (`CIRCUS_MEASURE=1`, e.g.
`TestMeasureReceivePoolShare` in `internal/controlplane/gateway`, see
`internal/controlplane/calls/DESIGN.md`) and mutation testing are local
only and never run in CI.

The nightly workflow also splits the 500-seed sweep into five segments so
a hang is attributed to one segment (`CIRCUS_ROUTER_FIRST_SEED`); replay
one failing seed with `CIRCUS_ROUTER_SEED=<n> go test -run TestRandomSequences ./internal/controlplane/calls/`.
The nightly sweep runs with `CIRCUS_ROUTER_TIMEOUT_SCALE=2` (it multiplies the
sequence test's waits for slow runners); set it as well when a replayed
failure looks timing related.
