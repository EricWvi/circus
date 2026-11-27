package userservice

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/EricWvi/circus/internal/credfile"
)

// RemoveRejected removes the service and its credential file at path after
// the control plane proved it revoked that credential (the identity was
// deleted, or replaced by a rejoin): rejected names the credential the
// process ran with. Only the running service calls it (the daemon's
// --service, the worker launcher's --service), just before it exits with
// the terminal status, which the manager does not restart.
//
// A join (a rejoin's `join --force`) may write a new credential to path at
// any moment and takes no lock, so the credential is moved aside first:
//
//  1. The installed service file must still record path as the credential
//     it reads; else nothing is touched.
//  2. path is renamed to a unique name in its directory. A join's Save
//     renames its file into place too, so the moved file is exactly the one
//     checked next, not one written after the check.
//  3. Unless the moved file is rejected (the same principal and token), it
//     is put back with a link, which fails rather than overwrite a still
//     newer credential written meanwhile (then the moved one, outdated, is
//     dropped), and nothing else is touched. For that very short moment
//     path is empty: a service starting just then stops for good, and the
//     install that follows a join restarts it.
//  4. It is ours: the service file is removed without stopping the service,
//     which would kill the caller: systemd disables the unit and drops it
//     once its process exits; on macOS a detached helper in its own session
//     waits up to 10 s for the process pid (the service's own) to exit and
//     then boots the agent out, since `launchctl bootout` from inside the
//     agent would end the caller first. Then the moved file is removed.
//  5. If a credential appeared at path meanwhile (a join raced in after
//     step 2), the service file read in step 1 is written back (and the
//     unit enabled again) and the error says to run the install command.
//
// It reports whether the credential was the rejected one and so removed.
// Worker packages and installed agents stay.
func (s *Service) RemoveRejected(path string, rejected Rejected, pid int) (bool, error) {
	m, err := s.current()
	if errors.Is(err, ErrUnsupported) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	data, err := readServiceFile(m)
	if data == nil || err != nil {
		return false, err
	}
	if filepath.Clean(m.definition(data).Credentials(s.role)) != filepath.Clean(path) || rejected.PrincipalID == "" {
		return false, nil
	}
	aside := fmt.Sprintf("%s.revoked-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.Rename(path, aside); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("move the credential file aside: %w", err)
	}
	testHookAside()
	if !holds(aside, rejected) {
		// Not ours: put it back, unless a newer one is there by now.
		if err := os.Link(aside, path); err != nil && !errors.Is(err, os.ErrExist) {
			return false, fmt.Errorf("put the credential file back from %s: %w", aside, err)
		}
		return false, removeAside(aside)
	}
	var removeErr error
	switch m := m.(type) {
	case systemd:
		removeErr = m.removeFromInside()
	case launchd:
		removeErr = m.removeFromInside(pid)
	default:
		removeErr = fmt.Errorf("unknown service manager %T", m)
	}
	removeErr = errors.Join(removeErr, removeAside(aside))
	testHookRemoved()
	if _, err := os.Stat(path); err == nil {
		// A join wrote a new credential while the service was removed: the
		// service is that credential's again.
		restoreErr := s.restoreService(m, data)
		removeErr = errors.Join(removeErr, fmt.Errorf("a new credential was written to %s meanwhile; %s (restoring its service file: %v)",
			path, "run `"+s.installCommand()+"` to make sure its service runs", restoreErr))
	}
	return true, removeErr
}

// restoreService writes the service file back and, on Linux, enables the
// unit again.
func (s *Service) restoreService(m manager, data []byte) error {
	if err := writePrivate(m.file(), string(data)); err != nil {
		return err
	}
	if sd, ok := m.(systemd); ok {
		if _, err := sd.systemctl("daemon-reload"); err != nil {
			return err
		}
		_, err := sd.systemctl("enable", sd.unit())
		return err
	}
	return nil
}

func removeAside(aside string) error {
	if err := os.Remove(aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", aside, err)
	}
	return nil
}

// Test hooks: after the credential is moved aside, and after the service
// and the moved file are removed.
var testHookAside, testHookRemoved = func() {}, func() {}

// Rejected names the credential the control plane revoked: its identity
// and the SHA-256 of its token, which, unlike the token, may be passed on a
// command line.
type Rejected struct {
	PrincipalID string
	TokenSHA256 [sha256.Size]byte
}

// RejectedCredential names f.
func RejectedCredential(f credfile.File) Rejected {
	return Rejected{PrincipalID: f.PrincipalID, TokenSHA256: sha256.Sum256([]byte(f.Token))}
}

// holds reports whether the credential file at path is the rejected one:
// the same principal and token.
func holds(path string, rejected Rejected) bool {
	f, err := credfile.Load(path)
	return err == nil && rejected.PrincipalID != "" && RejectedCredential(f) == rejected
}

// removeFromInside disables the unit and removes its file without
// stopping it: `disable --now` from inside the unit would kill the caller
// before the file is gone. Once the process exits (with a status the unit
// does not restart), systemd drops the unit.
func (s systemd) removeFromInside() error {
	if err := s.reachable(); err != nil {
		return err
	}
	if err := s.disable(); err != nil {
		return err
	}
	if err := restoreServiceFile(s.file(), nil); err != nil {
		return err
	}
	_, err := s.systemctl("daemon-reload")
	return err
}

// removeFromInside removes the plist and starts the detached helper that
// boots the agent out once pid has exited.
func (l launchd) removeFromInside(pid int) error {
	if err := restoreServiceFile(l.file(), nil); err != nil {
		return err
	}
	return startDetachedUnload(l.home, l.svc, pid)
}

// startDetachedUnload starts `circus --internal-launchd-reload <home>
// <service>=unload:<pid>` in its own session and does not wait for it.
// Tests replace it.
var startDetachedUnload = func(home string, svc *Service, pid int) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, LaunchReloadFlag, home, svc.name+"="+stepUnload+":"+strconv.Itoa(pid))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start launchd unload: %w", err)
	}
	return cmd.Process.Release()
}

// unloadWait bounds how long the unload helper waits for the service's
// process to exit before it boots the agent out anyway.
var unloadWait = 10 * time.Second

// waitForExit waits up to unloadWait for process pid to be gone.
func waitForExit(pid int) {
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(unloadWait)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
