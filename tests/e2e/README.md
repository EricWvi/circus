# Process end-to-end tests

Run on Linux with Go, Bash, the usual coreutils and Node.js 24 or later installed.
Run `npm ci` from the repository root before running the suite.

```sh
go test -tags e2e ./tests/e2e/ -count=1
go test -tags e2e ./tests/e2e/ -count=3
go test -race -tags e2e ./tests/e2e/ -count=1
```

The harness builds `bin/circus` once in a per-run temporary directory under
`.tmp/`, with `e2efault` enabled. Under `-race` the harness builds only a
race-enabled binary instead. The build and all test temp directories share that per-run root, removed
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
remains. The daemon-crash and revocation tools write their exact PID and then execs sleep; the Linux
subreaper collects that one adopted child after killing its daemon
(TestRunDirectorySweepUsesFlock checks the run-directory lock separately). Child stderr
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
  exits 1. The daemon-crash test also proves --wait follows Unknown to terminal
  Lost. A server crash after Submit exits 3 and prints the call ID
  (TestCLITransportFailureIsUnknown).
- Worker ownership: closing a client mid-call does not cancel it; a new
  connection obtains its Result. On one connection a worker can Query and
  Cancel a call it is watching: Query answers Dispatched, Cancel answers
  Dispatched (intent recorded) and the stream carries the real terminal update.
  66 sequential calls exceed the 64-slot cap and prove terminal slot release.
  Raw same-link Submit→Cancel (request IDs 1 and 2) stays ordered: both the
  Cancel reply and the watched stream's terminal update are Cancelled.
- TestUnwatchFreesSlotImmediately: with the device offline inside its grace
  period, 64 Accepted watches fill one connection; the next Submit is refused
  locally, and after closing one stream (Unwatch) it is admitted at once by
  the server while the unwatched call stays Accepted and uncancelled.
- TestSessionTurn: a session with pending input (written to the server's
  database) is offered to a worker that attaches over the test client
  (`internal/testutil/workerclient`); the worker acquires it, takes the
  input, reports the turn, runs a device call admitted by its lease through
  the real daemon (a call at another epoch is `SESSION_FENCED`) and
  releases.
- TestTypeScriptWorkerLink: the worker workspace builds and connects using a
  credential from `circus join`. It takes input for a session the server
  assigns to the worker on the daemon's machine. It loses an in-flight reply
  during a control-plane restart, resumes the same lease, receives a replayed
  Nudge and retries successfully. It round-trips a fragmented transcript entry,
  publishes a result, ends the turn and releases the lease.
- TestWorkerRestartWhileHalfOpen: a worker connection goes half-open (a
  local proxy stops moving bytes without closing), a CLI call is running on
  the same credential, and a restarted worker still connects and gets a
  result well inside the heartbeat timeout.
- TestCancelReplies: Cancel of a finished call answers its result, of an
  accepted call (device offline) Cancelled (the watched stream ends
  Cancelled too), and of an unknown ID Rejected NOT_FOUND.
- TestCancelErrorDoesNotBlockLaterCancel: at the cap (63 Accepted watches
  with the device offline inside its grace period), a Cancel of an unknown ID
  answers NOT_FOUND in the last slot and gives it back, so a later real
  Cancel still fits and answers Cancelled; the link stays open.
- TestDaemonCrashLeavesCallUnknown: kill the daemon during a marked Bash call; observe Unknown without ending
  the subscription (CLI query exits 3), restart with a new boot, then observe
  Lost. Equal resubmit stays Lost; the marker contains exactly one line.
- TestServerCrashLosesAcceptedCallThenResubmits: take the device offline within grace, persist Accepted, kill the
  server, and restart. Query reports Lost with no dispatch boot. Equal resubmit
  executes exactly once when the daemon returns.
- TestServerCrashWithRunningToolRecoversResult: block a marked real tool on a FIFO, kill the server, release the
  tool, then restart. The same daemon boot reconciles to Completed; Query
  returns the real retained output, an equal resubmit acts as a per-call ordered
  dedup barrier, and only then is the marker counted (exactly one line).
- TestServerCrashAfterDispatchCommitLosesCall: the tagged Sender wrapper
  SIGKILLs the server after dispatch commit and before gateway delivery. Restart queries the daemon and settles Lost. Equal retry
  stays Lost and the marker is absent (zero executions).
- TestRevocationEndsDaemon: a separate CLI process revokes the connected daemon in the same SQLite
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
