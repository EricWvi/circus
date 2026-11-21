# Circus worker

Build with Node.js 24 or later and run from the repository root.

```sh
npm ci
task worker:build
circus join --role worker
node worker/dist/main.js
```

The entry point sends Attach with local mode enabled and logs readiness and
session hints to stderr as JSON lines. It reads the credential written by
`circus join` from `$CIRCUS_HOME/config/worker.json`, or from
`$XDG_CONFIG_HOME/circus/worker.json` with a default of
`~/.config/circus/worker.json`. The file supplies the server URL, principal ID,
bearer token and optional CA bundle.

| Environment variable | Default | Values |
| --- | --- | --- |
| `CIRCUS_WORKER_MAX_SESSIONS` | `4` | Integer from 1 to 64 |
| `CIRCUS_WORKER_LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error` |

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
