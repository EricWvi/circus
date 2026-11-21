package workspace

import "syscall"

// setDeathSignal kills git when the daemon dies, so a crashed daemon does
// not leave a clone writing into a hidden directory that the next attempt
// deletes.
//
// Assumption: the kernel sends Pdeathsig when the OS thread that started
// the child exits, not when the process does. That is safe only while the
// starting thread outlives the child. Go keeps its threads alive unless a
// goroutine exits while locked to one (runtime.LockOSThread without
// UnlockOSThread); nothing in the daemon calls LockOSThread or ends
// threads. Adding such code would make git die early and clones fail
// spuriously, so start git from an unlocked goroutine or drop this.
func setDeathSignal(attr *syscall.SysProcAttr) { attr.Pdeathsig = syscall.SIGKILL }
