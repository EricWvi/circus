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
still see the usual XDG defaults.

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
including when the caller is the super user.

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
| Session service (worker-link side) | about 1,030 MiB | 2 read replies built at once, each ≤ 2·M = 128 MiB, so ≤ 256 MiB together; per worker socket one inline operation's converted request (≤ M) plus its queue of inline replies (≤ 16 MiB encoded, about 32 MiB decoded): 8 × 96 MiB = 768 MiB; session table, Wake queue, hint queues and extension keys ≤ about 11 MiB |
| Session watch streams | about 888 MiB | 256 streams × about 3.47 MiB each: the bus queue and the batch being delivered (2 × 272 KiB), one replay page (about 294 KiB), the stored event being converted (256 KiB, plus 512 KiB when its UTF-8 must be repaired) and its JSON encoding (up to 1.5 MiB, in a pooled buffer of up to about 1.9 MiB; SessionService does not compress responses); see `internal/controlplane/sessions/bus.go` |

That is about 6.1 GiB (4,355 MiB for the links and the router, 1,030 MiB
for the session service, 888 MiB for watch streams) before SQLite,
TLS/kernel buffers, bounded operation scratch, the runtime and GC headroom;
set `GOMEMLIMIT` above it, for example `GOMEMLIMIT=7GiB` with at least
8 GiB of memory for the process. The daemon connection limit (100 sockets)
is fixed; worker sockets default to 8 (`worker_connections`, at most 4 per
user and per credential), each adding about 139 MiB to the worker links and 96 MiB to the
session service at the default message limit. Live-only session events
handed to the bus are charged to each watcher's queue (at most 256 KiB of
payload each) and counted in the watch streams' figure. See
`internal/controlplane/gateway`, `internal/controlplane/workerlink`,
`internal/controlplane/sessions` (doc.go and bus.go) and
`internal/controlplane/calls/DESIGN.md`.

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
