# Minimal mutation driver

`mutate` applies one small syntax mutation at a time to a Go package and runs its tests through a Go overlay. It never edits source files: mutated files, overlay descriptions, and scratch directories live under a temporary directory in `.tmp/` and are removed on exit.

Every test run is isolated in a [bubblewrap](https://github.com/containers/bubblewrap) sandbox (`--unshare-pid --die-with-parent --new-session`): the root filesystem is bound read-only, `/tmp` is a fresh tmpfs, and the only writable binds are the Go build cache and a per-mutant scratch directory, which is mounted at the repository's `.tmp` path and exported as `TMPDIR`. A mutant can therefore neither write into the tracked source tree nor leak processes into the host: when the sandboxed `go test` exits or is killed, the PID namespace collapses and every descendant dies with it. The scratch is mounted at the repository `.tmp` path because the daemon test suites create their own scratch directories under the repository `.tmp` by design; each sandbox has its own mount namespace, so parallel workers see their own scratch there, and the directory is deleted with each mutant. `-no-sandbox` disables the sandbox for debugging only.

It supports comparison, logical, arithmetic, increment/decrement, `if`, integer-literal, call-removal, simple error-return, and branch mutations. Test files and files with the standard `Code generated ... DO NOT EDIT.` marker are skipped. Use `-files` to restrict the source files and `-run` to restrict tests.

The default worker count is `max(1, nproc/4)` test processes; the sandboxed runs each compile and run a full package test binary, so leave headroom for the tests' own subprocesses. Override with `-workers`.

Run from the repository root, for example:

```sh
go run ./tools/mutate -timeout 23s \
  -out .tmp/mutation/fstools.json ./internal/daemon/fstools
```

Or use the task wrapper (timeout and report path default to 30s and `.tmp/mutation/report.json`; workers default to the tool's `max(1, nproc/4)` unless `MUTATE_WORKERS` is set):

```sh
task mutate PKG=./internal/daemon/fstools MUTATE_TIMEOUT=23s \
  MUTATE_OUT=.tmp/mutation/fstools.json
```

The JSON report contains every mutant's location, operator, original and mutated text, status, and duration. `invalid` means the mutant did not compile; `timeout` is reported separately from killed and lived mutants. The printed mutation score is `killed / (killed + lived)`, excluding invalid and timed-out mutants.
