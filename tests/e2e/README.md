# Process end-to-end tests

Run on Linux with Go, Bash and the usual coreutils installed:

```sh
go test -tags e2e ./tests/e2e/ -count=1
go test -tags e2e ./tests/e2e/ -count=3
go test -race -tags e2e ./tests/e2e/ -count=1
```

The harness builds `bin/circus` once in a per-run temporary directory under
`.tmp/`, with `e2efault` enabled. A race-enabled test also builds a race-enabled
binary. The build and all test temp directories share that per-run root, removed
by a defer on normal return. Each root holds an flock for its whole lifetime;
startup sweeps a root only when it can acquire that lock, so hard test timeouts
are collected without relying on PID liveness or deleting an active run. Runs
predating the lock file are deleted only when more than one hour old. The
default build contains no crash-injection branch. Tests use isolated HOME/XDG directories, real SQLite
databases, an initial loopback port-0 listener, enrollment links read from
stdin, and real daemon processes. The server uses a short test timer poll
interval instead of the default 1s. Restarts reuse the server's logged address
only after waiting for its exact PID to exit. No port probing or failed-dial
inference is used.

Each subprocess has its own process group and Linux Pdeathsig=SIGKILL. Start and
Wait run on one dedicated runtime.LockOSThread goroutine per child, keeping the
creator thread alive for the whole child lifetime. Cleanup signals only recorded
PIDs, waits for them, escalates to SIGKILL after a grace, and checks that none
remains. The F4 tool writes its exact PID and then execs sleep; the Linux
subreaper collects that one adopted child after killing its daemon. Child stderr
is included in failed-test logs. All ordering uses protocol replies, durable
metadata, marker files or bounded polling; no sleep establishes test ordering.

- Happy path: Bash, read, write and edit through both the CLI and Go client;
  CLI query/cancel; ordinary tool errors still exit zero. Bash passes --cwd;
  file calls leave it empty and use absolute paths.
- Validation and operation answers: invalid IDs fail locally, a raw empty-ID
  rejection closes instead of hanging, and query/cancel NOT_FOUND prints
  "unknown call id" with exit 1 without becoming a call outcome.
- CLI exits: a running query exits 4, running cancel exits 0 with Dispatched,
  accepted cancel exits 0 with Cancelled, and querying that Cancelled state
  exits 1. F4 also proves --wait follows Unknown to terminal Lost. A server
  crash after Submit exits 3 and prints the call ID.
- Worker ownership: closing a client mid-call does not cancel it; a new
  connection obtains its Result. A same-client Cancel reaches a watched call
  and its stream carries the real terminal update. Four running calls plus a
  locally closed stream prove ghost accounting: a fifth is refused locally,
  then admitted after the ghost's terminal update. Six sequential calls exceed
  the four-slot cap and prove terminal slot release. Raw same-link Submit→Cancel
  stays ordered and successful Cancel has no extra reply.
- F4: kill the daemon during a marked Bash call; observe Unknown without ending
  the subscription (CLI query exits 3), restart with a new boot, then observe
  Lost. Equal resubmit stays Lost; the marker contains exactly one line.
- F6 accepted: take the device offline within grace, persist Accepted, kill the
  server, and restart. Query reports Lost with no dispatch boot. Equal resubmit
  executes exactly once when the daemon returns.
- F6 running: block a marked real tool on a FIFO, kill the server, release the
  tool, then restart. The same daemon boot reconciles to Completed; Query
  returns the real retained output, an equal resubmit acts as a per-call ordered
  dedup barrier, and only then is the marker counted (exactly one line).
- F7: the tagged Sender wrapper SIGKILLs the server after dispatch commit and before
  gateway delivery. Restart queries the daemon and settles Lost. Equal retry
  stays Lost and the marker is absent (zero executions).
- F17: a separate CLI process revokes the connected daemon in the same SQLite
  database. Recheck closes the link, its next upgrade receives 403, and the daemon
  exits nonzero. Revoking the worker closes its link while a call subscription
  is active; a new dial gets 403. Assertions allow the 1s recheck interval plus
  5s timeout and scheduling.
- Graceful shutdown: SIGTERM with daemon and worker connected exits zero within
  the configured deadline. The device row records LastDisconnectedAt >=
  LastConnectedAt and has a device_grace timer before Router.Close could lose
  that evidence. The daemon stays alive and executes after the server restarts.

The normal suite completes in seconds; the target is under 60 seconds including
build. Tests have individual deadlines and never start Docker or BuildKit.
