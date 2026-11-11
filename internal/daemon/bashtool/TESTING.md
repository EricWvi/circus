# Capture and store verification

The tests use real Bash process groups, private files, `Run`, the background
worker and `jobs.Manager`. They never manufacture `processOutcome`, add
`ErrKilled`, override an exit code, or call finishing/transfer helpers to drive
a table row. Observers only inspect captures created by `Run`. The protocol
release and pgid-index identity tests deliberately make an invalid request or
index collision; they still use real commands and drivers for completion.

A capture owns its descriptor and budget record throughout its lifetime.
Its state identifies the one driver allowed to finish it: foreground (F),
background job (B), tracker (T), then released (R). A non-owner release errors.
The driver explicitly chooses track **or** release. R means group empty,
descriptor closed and active/budget records gone; retained names belong to
outputStore's retention policy. Snapshot leases identify snapshots directly,
not through cwd scratch paths, and end after shell/cwd work and synchronous
closed-tracker cleanup. A child TERM trap retains the snapshot until that
cleanup finishes; this is tested by ShutdownKeepsSnapshotThroughChildTrap.
Foreground handoff instead releases its shell lease before returning, allowing
a refresh to remove the obsolete snapshot while the child remains alive. Each
store has its own configuration and mutex. `effects` is a separate injected
dependency, not a pointer into Bash's fields.

## Public transition table

`TestCaptureTransitions` exercises the rows below. Every row checks actual
foreground metadata or the job in `jobs.Manager`, phase/driver, linked/deleted
file, exact diagnostic count (using an inspector even after unlink), empty
group, descriptor closure, zero budget/active records, zero leases and no
tracker entries. Natural background exits additionally receive a late TaskStop:
it must report no task and preserve the completed job, including when children
still belong to the internal tracker.

| Row | Initial driver × event | Final phase/driver | Exit / manager state | File | Effects |
| --- | --- | --- | --- | --- | --- |
| foreground-exit | F × exit | exited/R | 0 / — | delete | wait; close |
| foreground-failure | F × exit | exited/R | 7 / — | delete | wait; close |
| foreground-children | F × exit + children | empty/R | 0 / — | delete | handoff to T; keep file/budget until children empty |
| foreground-failed-children | F × exit + children | empty/R | 7 / — | delete | handoff to T; keep file/budget until children empty |
| foreground-persisted | F × persist | exited/R | 0 / — | retain | wait; close |
| foreground-cancel | F × cancel + children | stopped/R | 143 / — | delete | TERM, bounded KILL escalation; wait; close |
| foreground-cap | F × cap + children | killed/R | 137 / — | delete | KILL; wait; one cap diagnostic; close |
| background-exit | B × exit | exited/R | 0 / EXITED | retain | wait; close |
| background-failure | B × exit | exited/R | 7 / EXITED | retain | wait; close |
| background-failed-children | B × exit + children | empty/R | 7 / EXITED | retain | handoff to T; keep file/budget until children empty |
| promoted-failed-children | F → B × exit + children | empty/R | 7 / EXITED | retain | handoff to T; keep file/budget until children empty |
| background-children | B × exit + children | empty/R | 0 / EXITED | retain | handoff to T; keep file/budget until children empty |
| background-stop | B × stop + children | killed/R | 143 / KILLED | retain | TERM, bounded KILL escalation; wait; close |
| background-trap-stop | B × stop | stopped/R | 7 / EXITED | retain | TERM, bounded KILL escalation; wait; close |
| background-lifetime-trap | B × lifetime | killed/R | 7 / KILLED | retain | TERM, bounded KILL escalation; wait; close |
| background-cap | B × cap | killed/R | 137 / KILLED | retain | KILL; wait; one cap diagnostic; close |
| promote-exit | F → B × exit | exited/R | 0 / EXITED | retain | wait; close |
| promote-children | F → B × exit + children | empty/R | 0 / EXITED | retain | handoff to T; keep file/budget until children empty |
| promote-stop | F → B × stop | killed/R | 143 / KILLED | retain | TERM, bounded KILL escalation; wait; close |
| promote-cap | F → B × cap | killed/R | 137 / KILLED | retain | KILL; wait; one cap diagnostic; close |

Additional driven transitions and scheduling windows have focused tests:

| State × event | Test | Required observation |
| --- | --- | --- |
| B or promoted × Bash.Shutdown **before** natural handover | ShutdownBeforeHandover | closed tracker stops children; EXITED/0; retained output; accounting zero |
| F aggregate victim × TERM-ignoring children after natural exit | TotalVictimChildrenUseKILL | code 137, one total-cap line; group gone in <800 ms, below 2 s TERM grace; smaller job survives |
| selected aggregate victim × exits before KILL | TotalVictimExitedBeforeKill | actual code 0, no cap explanation; smaller job survives |
| F natural exit with children × ReadAt/Stat failure | FormatFailureStopsChildren | original I/O error; children stopped before unlink/close |
| B × marker/explanation/Close error | BackgroundWriteErrors | Wait retains injected error; actual manager EXITED/-1, EXITED/7 or KILLED/original-code as appropriate |
| promoted, launch callback held × cap | CapDuringPromotion | real worker finishes while registration pending; later published KILLED/137, one explanation |
| launch callback held × jobs.Shutdown, before or after tracker handover | RejectedRegistration | ErrShutdown, no registered job, no unannounced file, empty group and zero records |
| B × wrong-driver release | WrongDriverRelease | error, live descriptor unchanged; normal Run/manager completion |
| B natural exit × tracker registration gate | TrackerHandoverOrder | owner T and its index entry are installed atomically before the registration gate opens, even if children exit at the gate |
| F natural exit with children × snapshot refresh | ForegroundHandoffReleasesSnapshotBeforeChildExit | Run removes cwd scratch and drops its lease; refresh removes the old snapshot while the child is still alive |
| live or closed T × Close/Remove failure | TrackerReleaseErrorsIgnored | real cleanup completes; injected errors do not change Run or Shutdown; failed Remove leaves a file for test cleanup |
| closed T × cap selected during child cleanup | ClosedTrackerExplainsLateCap | exactly one cap explanation follows synchronous cleanup |
| T empty × entry removal | TrackerEntriesReleased | g.stops really becomes empty |
| old T empty × reused pgid index | TrackerEntryIdentity | stop uses capture's own entry; old defer cannot remove replacement |
| T × shutdown | TrackerShutdown, TrackerStopUsesTERM | group stopped within bound; TERM trap actually runs |
| T × cap | TrackedCap | empty group, exactly one cap line, correct file deletion |
| KILL × actual wait-probe gate | WaitOwnership | actual group-wait predicate is blocked; file, descriptor and running budget remain owned until allowed to query the real kernel |
| stopped B × bounded wait still reports alive | StoppedGroupFallback | no handover to unbounded tracker; KILLED/143 and released records |
| ended F × blocked formatting | FinishedBudgetBeforeFormatting | descriptor remains for formatting, but finished file no longer consumes running budget |

The wait and final-alive probes are test-only read adapters. The fallback test simulates a
kernel verdict after bounded cleanup, without an unkillable privileged process
or a five-second delay; Run, Stop, Wait and jobs.Manager remain real. Removing
these probes would make the nonempty-group handoff guard and wait-ownership
invariants impractical to test deterministically. In particular M18 is only
equivalent if stopping always empties the group. Existing bounded-wait behavior
permits a nonempty final verdict, so the terminationRequested guard is retained
and tested, preserving that behavior.

## Failures versus scheduling delays

`attempt` propagates errors. `pause` cannot return one. There is no cleanup hook
that discards an injected error, no fallible budget-add rollback and no no-op
track action. The 12 fallible operations are cwd-open, output open, file-lock,
process start, register-job, promotion-stat, format-read, format-stat,
exit-marker, explain, close and remove. Promotion's foreground-to-background
state move is not injectable: no production state can make it fail.

`TestCaptureFaults` checks nine acquisition/format/transition/unlink branches.
The file-lock case observes a successful real Close through the after-Close
adapter, so it detects rollback descriptor leaks without relying on GC or a
finalizer. `BackgroundWriteErrors` checks marker, explanation and Close errors
for exit 0 and exit 7, including the actual manager mapping and Wait error.
`BackgroundCapAndMarkerErrors` uses distinct marker and explanation failures
and requires Wait to retain both. Close errors are injected **after real Close**,
matching a reported close I/O error without inventing an open-descriptor leak.
Foreground Close errors remain ignored, as before. Live and closed tracker
cleanup ignore both Close and Remove errors; tests require unchanged Run and
Shutdown results. Remove failure genuinely leaves the name: tests check the
returned error where the policy reports it and released descriptor/accounting,
then remove the artificial inaccessible file. They do not claim that a failed
unlink deletes a file. No allowance is added.

`reject` may call Stop after `jobs.Manager.Start` rejects a launched job. This
relies on Manager.Start's closed-after-launch contract: it calls job.Stop before
returning ErrShutdown. The capture's Stop is idempotent; Wait then reclaims
resources without publishing a job.

`TestCaptureDelays` separately delays all 32 named operation/ownership points
by at least the 100 ms poll interval. Budget add/start/finish/remove, active-set
removal, shell wait/exit, signals, group wait, output checks, lease retain/release,
tracker registration/removal and job-launch publication are scheduling-only;
they are **not fault cases**. Gates cover actual wait, registration rejection,
transfer ordering and pid-index replacement. Signal semantics are established
by the elapsed-time and TERM-trap behavior tests, not by hook names.

## Multi-capture reference model

`TestCaptureModelSequences` first executes all 40 reachable
(mode, operation, children) cells. Modes are foreground, background and promoted;
operations are natural exit, Cancel, TaskStop, cap, total pressure, jobs.Shutdown
and Bash.Shutdown. Only foreground TaskStop is excluded because it has no job
ID. Background Cancel is tested as a no-op rather than rewritten into TaskStop.
Every cell starts two concurrent captures, interleaves nonempty writes and
Cancel, then exercises its selected event and cleans up via further public
operations. A total-budget event must select the unique largest writer while
the smaller job stays running.

After enumeration, 12 randomized multi-step sequences (2 with `-short`) start
at least three captures and issue weighted fills, promotion, effective
foreground Cancel, natural exit and TaskStop in concurrent waves. Randomized
jobs.Shutdown occurs between waves, and Bash.Shutdown may precede final pressure.
Each wave operates on at least two distinct captures behind a common start barrier.
Per-request shell gates hold writes until every participant is in flight; only
the parent test goroutine then settles replies and updates the model. A Cancel
target is selected from any of the initial, still-foreground captures. The
aggregate victim is recomputed from actual descriptor sizes after the final
concurrent write wave, including tracked background exit markers, and is not
fixed by capture construction. Its KILL is then held while a second writer
exceeds the aggregate budget and exits; pending selection must suppress a second
victim. The model checks phase/driver, exact live bytes,
file/descriptor lifetime, running budget identity/count, tracker count, actual
manager state/code and cap explanations after every settled operation or wave.
Request contents are atomically published via rename so a shell cannot consume
a half-written test request.

`CIRCUS_CAPTURE_SEED` accepts an integer (including 0x-prefixed hexadecimal).
The default is the current Unix nanosecond time. Every failing case prints the
seed and its operation trace. Replay, for example:

```sh
CIRCUS_CAPTURE_SEED=0xc1ac05 go test -count=1 -run TestCaptureModelSequences ./internal/daemon/bashtool
```

## Mutation observations

The runner restores exact source bytes in finally blocks, without git checkout
or other state-changing git commands. Patterns follow operations when they move.

These are observations about the listed mutations, not a claim that all
possible regressions are covered. Every non-equivalent mutation below fails
a test aimed at its effect; hook visitation alone is not its signal.

| ID | Mutation | Result | Aimed test |
| --- | --- | --- | --- |
| M1 | drop lifetime ErrKilled on background deadline | KILLED | `TestCaptureTransitions/background-lifetime-trap` |
| M2 | abandonForeground skips stopLeftovers after shell exit | KILLED | `TestCaptureFormatFailureStopsChildren` |
| M3 | closed tracker does not stop leftovers | KILLED | `TestCaptureShutdownBeforeHandover` |
| M4 | reject ignores tracker-owned children | KILLED | `TestCaptureRejectedRegistration` |
| M5 | promote failure leaks snapshot lease | REMOVED: the test-only promote fault point and unreachable state guard were deleted | `—` |
| M6 | total victim children get TERM grace instead of KILL | KILLED | `TestCaptureTotalVictimChildrenUseKILL` |
| M7 | background cap uses TERM path | KILLED | `TestOutputCapKillsTERMIgnoringWriters/background` |
| M8 | tracker release keeps active/budget records | KILLED | `TestCaptureTrackerEntriesReleased` |
| M9 | background release deletes its output | KILLED | `TestCaptureTransitions/background-exit` |
| M10 | truncated foreground output not retained | KILLED | `TestCaptureTransitions/foreground-persisted` |
| M11 | hand to tracker before state move | REMOVED: ownership transfer and index registration now occur under the same locks | `TestCaptureTrackerHandoverOrder` |
| M12 | Shutdown does not stop tracked groups | KILLED | `TestCaptureTrackerShutdown` |
| M13 | tracker cap does not kill group | KILLED | `TestCaptureTrackedCap` |
| M14 | background children output deleted when group empties | KILLED | `TestCaptureTransitions/background-children` |
| M15 | late terminate after exit skips leftover stop | REMOVED_EQUIVALENT: Unreachable repeated terminate branch removed. | `—` |
| M16 | reject skips final unannounced-file removal | EQUIVALENT: original edit only renames an observation label; os.Remove still runs | `TestCaptureRejectedRegistration` |
| M6b | total-victim children TERM grace, seam name kept | KILLED | `TestCaptureTotalVictimChildrenUseKILL` |
| M17 | background drops marker/limit write errors | KILLED | `TestCaptureBackgroundWriteErrors/exit-marker/7` |
| M18 | background leftovers ignore terminationRequested | KILLED | `TestCaptureStoppedGroupFallback` |
| M19 | promotion formatErr path drops explicit terminate | REMOVED_EQUIVALENT: Explicit promotion cleanup duplicates the foreground defer; removed. | `—` |
| M20 | stopLeftovers sends KILL instead of TERM | KILLED | `TestCaptureTrackerStopUsesTERM` |
| M21 | kill path skips group wait | KILLED | `TestCaptureWaitOwnership` |
| M22 | tracker map entry deleted unconditionally replaced by no delete | KILLED | `TestCaptureTrackerEntriesReleased` |
| M16b | reject keeps rejected job output file | KILLED | `TestCaptureRejectedRegistration` |
| O01 | promotion-phase | KILLED | `TestCaptureModelSequences/cells/promoted/natural` |
| O02 | promotion-owner | KILLED | `TestCaptureTransitions/background-exit` |
| O03 | promotion-guard | KILLED | `TestCaptureTransitions/background-exit` |
| O04 | drop-budget-finish | KILLED | `TestCaptureFinishedBudgetBeforeFormatting` |
| O05 | finish-live-budget | KILLED | `TestCaptureFinishedBudgetBeforeFormatting` |
| O06 | drop-cap-explanation | KILLED | `TestCapturePerFileNaturalExit/foreground` |
| O07 | drop-foreground-cap-code | KILLED | `TestCapturePerFileNaturalExit/foreground` |
| O08 | drop-background-killed | KILLED | `TestCapturePerFileNaturalExit/background` |
| O09 | drop-unlink | KILLED | `TestCaptureTransitions/foreground-exit` |
| O10 | drop-close | KILLED | `TestCaptureTransitions/foreground-exit` |
| O11 | drop-budget-release | KILLED | `TestCaptureTransitions/foreground-exit` |
| O12 | drop-active-release | KILLED | `TestCaptureTransitions/foreground-exit` |
| O13 | retain-inline-leftovers | KILLED | `TestCaptureTransitions/foreground-children` |
| O14 | skip-group-wait | KILLED | `TestCaptureWaitOwnership` |
| O15 | suppress-natural-total-guard | KILLED | `TestCaptureTotalVictimExitedBeforeKill` |
| O16 | drop-budget-register | KILLED | `TestCaptureModelSequences/cells/foreground/natural` |
| O17 | drop-running-budget | KILLED | `TestCaptureModelSequences/cells/foreground/natural` |
| O18 | lose-driver-on-track | KILLED | `TestCaptureTrackerHandoverOrder` |
| X1 | old tracker unconditionally deletes replacement entry | KILLED | `TestCaptureTrackerEntryIdentity` |
| X2 | stopTracked takes entry from reused pgid index | KILLED | `TestCaptureTrackerEntryIdentity` |
| X3 | release snapshot before closed-tracker child cleanup | KILLED | `TestCaptureShutdownKeepsSnapshotThroughChildTrap` |
| N3 | foreground tracker handoff retains its shell lease | KILLED | `TestCaptureForegroundHandoffReleasesSnapshotBeforeChildExit` |
| N6 | tracker Close errors become visible | KILLED | `TestCaptureTrackerReleaseErrorsIgnored/closed/close` |
| N7 | tracker Remove errors become visible | KILLED | `TestCaptureTrackerReleaseErrorsIgnored/closed/remove` |
| N8 | file-lock rollback skips Close | KILLED | `TestCaptureFaults/file-lock` |
| N10 | closed tracker omits a late cap explanation | KILLED | `TestCaptureClosedTrackerExplainsLateCap` |
| N12 | cap explanation overwrites the marker error | KILLED | `TestCaptureBackgroundCapAndMarkerErrors` |

M15's repeated-terminate branch and M19's explicit pre-return terminate calls
were removed. M16 was already marked invalid in the review: changing only a
hook label has no production effect. M18's guard is intentionally retained for
the bounded-wait fallback, and the corresponding mutation is now killed.
X1, X2 and X3 are extra tests of identity-safe deletion and stopping after index
replacement and snapshot lifetime through child cleanup; the collision uses real captures without requiring PID rollover.
