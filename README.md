# circus

circus runs an agent loop in a cloud worker and executes tools on the user's own machines through a small Go daemon. A Go control plane will relay calls between them, with shared Protobuf messages for Go and TypeScript. Only the device daemon exists today: a tool layer with the Read, Write, Edit, Bash, and TaskStop tools, a dispatcher with call deduplication, timeouts, cancellation, and result retention, and background job management. It runs locally through `circus daemon call`. Networking and the control plane are not built yet.

| Path | Purpose |
| --- | --- |
| `proto/circus/v1/device.proto` | Device calls, results, lifecycle, and background job messages |
| `buf.yaml`, `buf.gen.yaml` | Protocol checks and local code generation |
| `gen/go/circus/v1/` | Generated Go messages |
| `gen/ts/` | Generated TypeScript workspace package `@circus/proto` |
| `internal/daemon/` | Device daemon: tool registry, call dispatcher, job manager, and dispatcher behavior tests |
| `internal/daemon/fstools/` | Native-compatible Read, Write, and Edit tools with fixture parity tests |
| `internal/daemon/bashtool/` | Bash and TaskStop tools with a captured shell environment and fixture parity tests |
| `internal/daemon/clocktest/` | Manually advanced clock for daemon tests |
| `cmd/circus/` | `version` command and the `daemon call` local stream harness |
| `tests/parity/` | Captured native tool-behavior fixtures, the capture harness, and behavior notes |
| `go.mod`, `go.sum` | Go module and pinned generator tool |
| `package.json`, `package-lock.json` | npm workspace and pinned Node dependencies |
| `Makefile` | Generation, lint, test, build, and generation drift checks |
| `.github/workflows/ci.yml` | Go, protocol, generation, and TypeScript checks |

Development requires Go 1.26, Node 24 with npm, and Buf 1.73.0 on `PATH`.

```sh
npm ci
make gen lint test build
npx tsc --noEmit -p gen/ts
make check-gen
./bin/circus version
```

Generated files are checked in. `make check-gen` requires a clean `gen/` tree after generation. Breaking checks are configured but will enter CI after there is a release to compare against. Set the CLI version at build time with `go build -ldflags "-X main.version=1.0.0" -o bin/circus ./cmd/circus`.

## Try the daemon locally

`circus daemon call` reads one protojson `DeviceCall` per line from stdin and writes one protojson `CallStatus` line per call to stdout. With `-wait=true` (the default), each line reports the completed call in completion order; with `-wait=false`, the status is written right after submit. A long-running `circus daemon` without `call` is not implemented yet.

```sh
./bin/circus daemon call -wait=true <<'EOF'
{"callId":"read-1","sessionId":"demo","read":{"path":"/etc/hostname"}}
{"callId":"bash-1","sessionId":"demo","bash":{"command":"echo hello from circus","cwd":"/tmp"}}
EOF
```
