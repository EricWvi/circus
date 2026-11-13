# circus

circus runs an agent loop in a cloud worker and executes its tools on the
user's own machines through a small Go daemon; a Go control plane relays
calls between them, with Protobuf messages shared by Go and TypeScript.
Today M2 is complete: the device daemon (tool layer, dispatcher, jobs,
outbound link), the control plane server (enrollment, daemon and worker
links, call routing with at-most-once execution), and the `circus call`
debug client. The TypeScript worker itself is M3 and not built yet.

| Path | Purpose |
| --- | --- |
| `proto/circus/v1/` | Device, link, enrollment, daemon-link and worker-link protocol messages |
| `buf.yaml`, `buf.gen.yaml` | Protocol checks and local code generation |
| `gen/go/circus/v1/` | Generated Go messages |
| `gen/ts/` | Generated TypeScript workspace package `@circus/proto` |
| `internal/controlplane/` | circus server: enrollment, auth, HTTP, TLS, health, timers, call-path composition |
| `internal/controlplane/bus/` | Bounded in-process delivery of daemon commands |
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
| `internal/wire/` | Bounded protobuf transport over authenticated binary WebSocket |
| `internal/wire/wiretest/` | Bounded in-memory duplex Conn with directional faults |
| `internal/wirepolicy/` | Shared scheduling table for link callers |
| `internal/store/` | Control-plane domain types and the Store interface |
| `internal/store/sqlite/` | SQLite store backend (WAL, synchronous=FULL, embedded migrations) |
| `internal/store/storetest/` | Store contract suite, fault injection and crash-restart harness |
| `internal/auth/` | One-time enrollment tokens, long-term bearer tokens, revocation |
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
# Terminal 1: the control plane (plaintext, loopback only; --db defaults to
# $XDG_DATA_HOME/circus/circus.db).
circus server --dev-insecure --listen 127.0.0.1:9543 --db /tmp/circus/circus.db

# Terminal 2: create an enrollment link (one token per role; both in one link).
# The URL must match the server's listen address.
circus server enroll-token create --role both --url http://127.0.0.1:9543 --db /tmp/circus/circus.db
# -> http://127.0.0.1:9543/join#<daemonToken>.<workerToken>

# Enroll this machine as both roles (paste the link, or pipe it in).
circus join --name desk

# Start the device daemon; it stays connected and reconnects on failure.
circus daemon

# Terminal 3: run a tool on the device. Tool text goes to stdout; JSON
# status lines go to stderr.
circus call --device desk --cwd /tmp --wait bash 'echo hello from circus'
circus call --device desk read /etc/hostname
echo "new content" | circus call --device desk write /path/to/other.txt
circus call --device desk edit /path/to/note.txt --old circus --new circus-user
```

Query and cancel drive the exit codes: a query of a call still running
exits 4, cancel of a running call exits 0 with `dispatched` and interrupts
the tool (exit code 143, `Command was interrupted`):

```sh
circus call --device desk --cwd /tmp --wait bash 'until [ -f /tmp/done ]; do sleep 2; done' &
sleep 1
circus call query <call-id>   # exit 4 while Accepted/Dispatched
circus call cancel <call-id>  # exit 0
```

The call IDs come from the first JSON line on stderr of each submit. Exit
codes: 0 = tool result (including `is_error` results) or a cancel that was
accepted; 1 = rejected (including an unknown device name, or a revoked
credential) / lost / offline / unknown call ID / usage; 3 = outcome unknown
after submission (query the call ID after reconnecting); 4 = query found the
call still running. `circus doctor` checks both roles' stored credentials
against the server:

```sh
circus doctor
# [daemon] ok   reachable / ready / credential accepted
# [worker] ok   ...
```

`circus server device list --db ...` shows enrolled devices; `circus server
device revoke <name> --db ...` revokes one and frees its name for a new
join. The daemon's local harness also still works without a server, reading
one protojson `DeviceCall` per line from stdin:

```sh
circus daemon call -wait=true <<'EOF'
{"callId":"read-1","sessionId":"demo","read":{"path":"/etc/hostname"}}
{"callId":"bash-1","sessionId":"demo","bash":{"command":"echo hello from circus","cwd":"/tmp"}}
EOF
```

## Operating the server

`circus server` requires exactly one transport mode: `--tls-cert FILE
--tls-key FILE` (TLS, the production mode), `--behind-proxy` (plaintext
behind a TLS-terminating reverse proxy; a non-loopback listener logs a
warning), or `--dev-insecure` (plaintext, loopback addresses only, for
local development). `--public-url` sets the base URL handed to joining
machines when it differs from the listen address; without it the URL is
derived from each request. `--listen` defaults to `:8443`.

The SQLite database defaults to `$XDG_DATA_HOME/circus/circus.db`
(`~/.local/share/circus/circus.db`). Admin subcommands (`enroll-token`,
`device`, `worker`) open only an existing database, so pass `--db` when it
is not the default.

Memory: all daemon links share ONE `wire.Budget`, sized by
`--send-pool-bytes` and `--receive-pool-bytes` (0 = the default, twice
`wire.MinBudget`; the server refuses smaller pools and names the flag and
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
(Rmin) plus a small-message reserve is planned before multi-user (M4). See
`internal/controlplane/gateway` and `internal/controlplane/calls/DESIGN.md`.

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
