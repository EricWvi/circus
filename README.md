# circus

circus runs an agent loop in a cloud worker and executes tools on the user's own machines through a small Go daemon. A Go control plane will relay calls between them, with shared Protobuf messages for Go and TypeScript. This pre-release skeleton defines the device protocol and generation pipeline; networking and tool execution come later.

| Path | Purpose |
| --- | --- |
| `proto/circus/v1/device.proto` | Device calls, results, lifecycle, and background job messages |
| `buf.yaml`, `buf.gen.yaml` | Protocol checks and local code generation |
| `gen/go/circus/v1/` | Generated Go messages |
| `gen/ts/` | Generated TypeScript workspace package `@circus/proto` |
| `internal/daemon/` | Stateless tool interface and protocol round-trip test |
| `cmd/circus/` | Minimal `version` and placeholder `daemon` CLI |
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
