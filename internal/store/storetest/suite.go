// Package storetest holds what every store.Store implementation is tested
// with: the contract suite (Run), a fault-injection wrapper (Faulty) and a
// crash-and-restart harness (Process).
//
// A backend wires the suite once:
//
//	func TestContract(t *testing.T) { storetest.Run(t, backend) }
//
// The suite covers each Ops method's documented behaviour, the call state
// machine edge by edge against an independently written table, the
// at-most-one dispatch of every call (a lost call is resubmitted only if it
// never reached a device, as a fresh attempt), write-once columns, terminal jobs, single winners
// under concurrency (enroll token use, call transitions, timer claims,
// connections; both within one handle and across two handles on the same
// database), serializable transactions, lease expiry and re-claim,
// transaction atomicity of domain writes with their timers, re-entrant use
// failing fast, injected write failures, and crash-and-restart at every
// write point of a scripted sequence. Tests that would hang on a deadlock
// bug run under their own short deadlines.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/EricWvi/circus/internal/store"
)

// Run runs the contract suite against b. Subtests run in parallel, each on
// its own database under t.TempDir.
func Run(t *testing.T, b Backend) {
	for _, tc := range []struct {
		name string
		fn   func(*testing.T, Backend)
	}{
		{"Users", testUsers},
		{"Pages", testPages},
		{"Identities", testIdentities},
		{"DeleteIdentity", testDeleteIdentity},
		{"Credentials", testCredentials},
		{"EnrollTokens", testEnrollTokens},
		{"EnrollTokenConcurrentUse", testEnrollTokenConcurrentUse},
		{"CallCreate", testCallCreate},
		{"CallTransitionTable", testCallTransitionTable},
		{"CallTransitionPreconditions", testCallTransitionPreconditions},
		{"CallPatch", testCallPatch},
		{"CallTransitionConcurrent", testCallTransitionConcurrent},
		{"CallList", testCallList},
		{"CallResubmit", testCallResubmit},
		{"CallWriteOnce", testCallWriteOnce},
		{"LoseCalls", testLoseCalls},
		{"Jobs", testJobs},
		{"JobTerminalIsFinal", testJobTerminalIsFinal},
		{"Timers", testTimers},
		{"TimerLeaseExpiry", testTimerLeaseExpiry},
		{"TimerConcurrentClaim", testTimerConcurrentClaim},
		{"Tx", testTx},
		{"TxReentrant", testTxReentrant},
		{"TxSerializable", testTxSerializable},
		{"SetConnectedConcurrent", testSetConnectedConcurrent},
		{"TxTimerAtomicity", testTxTimerAtomicity},
		{"FaultInjection", testFaultInjection},
		{"FaultyLockHonoursCtx", testFaultyLockHonoursCtx},
		{"FaultySerializesWrites", testFaultySerializesWrites},
		{"CrashAtEveryWritePoint", testCrashAtEveryWritePoint},
		{"CrashKeepsTimerClaim", testCrashKeepsTimerClaim},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.fn(t, b)
		})
	}
}

// deadline returns a ctx for a test whose bugs would hang rather than
// fail, such as a Faulty lock left held: the stuck call then returns the
// ctx's error after a few seconds instead of the test binary timing out.
func deadline(t testing.TB) context.Context {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// open opens a fresh database for one test.
func open(t *testing.T, b Backend) (store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s := b.Open(t, dir)
	t.Cleanup(func() { _ = s.Close() })
	return s, dir
}

// reopen opens a second handle on the database in dir.
func reopen(t *testing.T, b Backend, dir string) store.Store {
	t.Helper()
	s := b.Open(t, dir)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustIs(t testing.TB, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
}

func mustEqual[T any](t testing.TB, what string, got, want T) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\n got %+v\nwant %+v", what, got, want)
	}
}

func ptr[T any](v T) *T { return &v }

func hash(s string) []byte { return []byte("sha256:" + s) }

// fixture is a user with a device and a worker on the same machine.
type fixture struct {
	user   store.User
	device store.Identity
	worker store.Identity
}

const t0 = int64(1_700_000_000_000) // a fixed epoch for every test

func seed(t testing.TB, ops store.Ops) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{
		user: store.User{ID: "u1", Name: "admin", CreatedAt: t0},
		device: store.Identity{
			Kind: store.KindDevice, ID: "d1", UserID: "u1", Name: "laptop", MachineID: "m1",
			OS: "linux", Arch: "amd64", Hostname: "host1", ClientVersion: "0.1.0", CreatedAt: t0,
		},
		worker: store.Identity{
			Kind: store.KindWorker, ID: "w1", UserID: "u1", Name: "laptop", MachineID: "m1",
			OS: "linux", Arch: "amd64", Hostname: "host1", ClientVersion: "0.1.0", CreatedAt: t0,
		},
	}
	must(t, ops.CreateUser(ctx, f.user))
	must(t, ops.CreateIdentity(ctx, f.device))
	must(t, ops.CreateIdentity(ctx, f.worker))
	return f
}

func newCall(id string, f fixture) store.Call {
	return store.Call{
		CallID: id, UserID: f.user.ID, DeviceID: f.device.ID, SessionID: "s1", AgentID: "a1",
		Tool: "bash", RequestHash: hash(id), RequestBytes: 42, State: store.CallAccepted, SubmittedAt: t0,
	}
}

// pathTo lists the transitions that take a new call from accepted to state.
var pathTo = map[store.CallState][]store.CallState{
	store.CallAccepted:   nil,
	store.CallDispatched: {store.CallDispatched},
	store.CallRunning:    {store.CallDispatched, store.CallRunning},
	store.CallUnknown:    {store.CallDispatched, store.CallUnknown},
	store.CallCompleted:  {store.CallDispatched, store.CallRunning, store.CallCompleted},
	store.CallOffline:    {store.CallOffline},
	store.CallCancelled:  {store.CallCancelled},
	store.CallRejected:   {store.CallRejected},
	store.CallLost:       {store.CallLost}, // lost before dispatch; see createLostAfterDispatch
}

// wantCallEdges is the call state machine of outline 4.2 plus the
// resubmission of a lost call that never reached a device (D43 item 5),
// written out independently of the implementation's table.
var wantCallEdges = map[store.CallState][]store.CallState{
	store.CallAccepted:   {store.CallDispatched, store.CallOffline, store.CallCancelled, store.CallRejected, store.CallLost},
	store.CallDispatched: {store.CallRunning, store.CallCompleted, store.CallUnknown, store.CallRejected, store.CallLost},
	store.CallRunning:    {store.CallCompleted, store.CallUnknown, store.CallLost},
	store.CallUnknown:    {store.CallRunning, store.CallCompleted, store.CallLost},
	store.CallLost:       {store.CallAccepted},
}

// wantCallTerminal lists the outcome states.
var wantCallTerminal = []store.CallState{
	store.CallCompleted, store.CallOffline, store.CallCancelled, store.CallRejected, store.CallLost,
}

// wantJobTerminal lists the ended job states; running is the only other.
var wantJobTerminal = []store.JobState{store.JobExited, store.JobKilled, store.JobLost}

var allStates = []store.CallState{
	store.CallAccepted, store.CallDispatched, store.CallRunning, store.CallUnknown, store.CallCompleted,
	store.CallOffline, store.CallCancelled, store.CallRejected, store.CallLost,
}

// createIn creates call c and drives it to state through pathTo.
func createIn(t testing.TB, ops store.Ops, c store.Call, state store.CallState) {
	t.Helper()
	ctx := context.Background()
	must(t, ops.CreateCall(ctx, c))
	cur := store.CallAccepted
	for _, next := range pathTo[state] {
		ok, err := ops.TransitionCall(ctx, c.CallID, store.CallExpect{States: []store.CallState{cur}}, next, dispatchPatch(next, "b1"))
		must(t, err)
		if !ok {
			t.Fatalf("drive %s: %s → %s did not apply", c.CallID, cur, next)
		}
		cur = next
	}
}

// dispatchPatch records boot when moving to dispatched and is empty
// otherwise.
func dispatchPatch(to store.CallState, boot string) store.CallPatch {
	if to == store.CallDispatched {
		return store.CallPatch{DispatchedBootID: boot}
	}
	return store.CallPatch{}
}

func getCall(t testing.TB, ops store.Ops, id string) store.Call {
	t.Helper()
	c, err := ops.GetCall(context.Background(), id)
	must(t, err)
	return c
}

func label(parts ...any) string { return fmt.Sprint(parts...) }
