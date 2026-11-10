// Package leakcheck checks the resource invariants of daemon tests: after a
// test and its tools' Shutdown, nothing it started may remain on the machine.
//
// A test package wires it once, in TestMain:
//
//	func TestMain(m *testing.M) { os.Exit(leakcheck.Run(m, leakcheck.Config{})) }
//
// Run makes the test process a child subreaper (PR_SET_CHILD_SUBREAPER), so
// every descendant, including setsid and double-fork escapees, stays below it.
// It points HOME, XDG_CACHE_HOME and XDG_STATE_HOME at a private directory
// under the repository's .tmp, so default daemon paths never reach the real
// ~/.cache/circus or ~/.local/state/circus. After all tests it fails the
// package if descendants, extra file descriptors, deleted-but-open files,
// files under the isolated HOME, or a VmHWM above Config.MemoryLimit remain.
//
// Test helpers that create daemon resources call Track with the test's
// temporary root, and OutputRoot and StateDir for the injected bash output
// root and state directory. Track registers a t.Cleanup check; register it
// before the cleanups that shut tools down, so it runs after them. The check:
//
//   - waits up to two seconds for descendant processes of this test and
//     reports pid, pgid, sid, state, cwd and cmdline, then kills exactly those
//     pids. It reaps them itself only 100 ms after KILL, so a real waiter,
//     such as the cmd.Wait of an abandoned call, collects them first; it never
//     waits for any other pid. Live leftovers (Processes)
//     and unreaped zombies (Zombies) are separate checks; the wait ends early
//     once only unchanged zombies remain;
//   - reports file descriptors opened during the test whose targets are below
//     a tracked root, again after a two-second grace;
//   - reports files in output roots that no result announced (Expect), state
//     directory entries beyond the documented instance lock and current
//     snapshot, and deleted-but-open files below tracked roots;
//   - bounds the output roots' total size (DiskLimit, default 64 MiB).
//
// The process VmHWM is checked once, at package end, against
// Config.MemoryLimit (default 128 MiB, tripled under -race).
//
// A process belongs to a test when its cwd, an open file or its command line
// is below one of the test's roots. A background sampler (every 25 ms, read
// only) remembers the owning test of each process group and session, so a
// zombie, which has no cwd, files or command line, is still attributed and
// reported with the command it was last seen running. Verdicts scan all of
// /proc, so a process being reparented to the subreaper is not missed.
//
// When a test and all of its ancestors are sequential, no other test runs
// concurrently, so every new descendant and every new descriptor counts,
// attributed or not. Parallel tests rely on attribution; a parallel test's
// escapee that leaves every tracked root (for example "cd /; setsid ...")
// is caught by the package-end check, which names it but not its test.
//
// Before sampling descriptors the check forces a GC and waits for the queued
// finalizers, so a descriptor whose *os.File became unreachable is always
// closed and forgiven; descriptors of reachable objects, such as tools kept
// alive by their test's cleanups, are always reported.
//
// AuditTools wraps the tools given to a dispatcher. It fails a test when one
// call_id executes more than once, when a tool returns five seconds or more
// after its deadline (the dispatcher's abandonment grace), or when a Run is
// still executing two seconds after the test.
//
// Every check is on by default. Allow, AllowReexecution, DiskLimit and
// Config.AllowFDs opt out with a
// recorded reason; allowed findings are logged, not hidden. Leftover
// processes are killed and reaped even when allowed. On other systems than
// Linux every function is a no-op.
package leakcheck
