package userservice

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/EricWvi/circus/internal/circushome"
	"github.com/EricWvi/circus/internal/credfile"
)

// RemoveRejected removes the service and its credential file at path after
// the control plane proved it revoked that credential (the identity was
// deleted, or replaced by a rejoin): rejected names the credential the
// process ran with. Only the running service calls it (the daemon's
// --service, the worker launcher's --service), just before it exits with
// the terminal status, which the manager does not restart. A process run
// in the foreground touches no service and removes only its credential
// file, with steps 2 and 3 alone, and only when no installed service reads
// that file (RemoveRejectedCredential).
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
	removed, removeErr := removeRejectedCredential(path, rejected, func() error {
		switch m := m.(type) {
		case systemd:
			return m.removeFromInside()
		case launchd:
			return m.removeFromInside(pid)
		default:
			return fmt.Errorf("unknown service manager %T", m)
		}
	})
	if removed != CredentialRemoved {
		return false, removeErr
	}
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

// CredentialOutcome says what RemoveRejectedCredential did.
type CredentialOutcome int

const (
	// CredentialRemoved: the file held the rejected credential and is gone.
	CredentialRemoved CredentialOutcome = iota
	// CredentialKept: the file holds another or an unreadable credential.
	CredentialKept
	// CredentialGone: there was no file at the path.
	CredentialGone
	// CredentialLeftToService: the installed service reads the path, so the
	// file is left to it.
	CredentialLeftToService
)

// RemoveRejectedCredential removes the credential file at path after the
// control plane proved it revoked the credential rejected, which the caller
// ran with in the foreground. It never changes a service: when the
// installed service of s's role reads the same file (through links too),
// the file is left to that service (RemoveRejected), which removes both
// once it gets the same proof, and when that cannot be told (the service
// file cannot be read or parsed), the file is kept and the error says why. Otherwise path is renamed to a unique name in its directory (a join
// may write a new credential to path at any moment and takes no lock) and
// the moved file checked; unless it holds rejected (the same principal and
// token), it is put back with a link, which fails rather than overwrite a
// still newer credential written meanwhile (then the moved one, outdated,
// is dropped).
func (s *Service) RemoveRejectedCredential(path string, rejected Rejected) (CredentialOutcome, error) {
	definition, installed, err := s.Installed()
	if errors.Is(err, ErrUnsupported) {
		installed, err = false, nil
	}
	if err != nil {
		return CredentialKept, fmt.Errorf("cannot tell whether the installed %s service reads it, so it is kept: %w", s.name, err)
	}
	if installed {
		reads, ok := recordedCredentials(definition, s.role)
		if !ok {
			return CredentialKept, fmt.Errorf("cannot tell which credential file the installed %s service reads, so it is kept", s.name)
		}
		if sameFile(reads, path) {
			return CredentialLeftToService, nil
		}
	}
	return removeRejectedCredential(path, rejected, nil)
}

// recordedCredentials returns the credential file of role the service file
// d records: its --credentials argument, else the default path under its
// recorded CIRCUS_HOME, XDG_CONFIG_HOME or HOME. ok is false when d names
// no program (it could not be parsed) or records none of these.
func recordedCredentials(d Definition, role credfile.Role) (string, bool) {
	if d.Program() == "" {
		return "", false
	}
	recorded := filepath.IsAbs(d.Env["XDG_CONFIG_HOME"]) || d.Env[circushome.Env] != "" || d.Env["HOME"] != ""
	for _, arg := range d.Argv[1:] {
		recorded = recorded || arg == "--credentials" || strings.HasPrefix(arg, "--credentials=")
	}
	if !recorded {
		return "", false
	}
	return d.Credentials(role), true
}

// sameFile reports whether paths a and b name the same file, through
// symbolic links too. When either cannot be stat'ed, it compares the
// absolute paths with their directories' links resolved.
func sameFile(a, b string) bool {
	ai, aErr := os.Stat(a)
	bi, bErr := os.Stat(b)
	if aErr == nil && bErr == nil {
		return os.SameFile(ai, bi)
	}
	return resolveDir(a) == resolveDir(b)
}

func resolveDir(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	if dir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(dir, filepath.Base(abs))
	}
	return abs
}

// removeRejectedCredential moves the credential file at path aside and,
// when it holds rejected, runs whileAside (when not nil) and then removes
// the moved file, or else puts it back (see RemoveRejectedCredential). The
// error joins whileAside's and the removal's.
func removeRejectedCredential(path string, rejected Rejected, whileAside func() error) (CredentialOutcome, error) {
	if rejected.PrincipalID == "" {
		return CredentialKept, nil
	}
	aside := fmt.Sprintf("%s.revoked-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.Rename(path, aside); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return CredentialGone, nil
		}
		return CredentialKept, fmt.Errorf("move the credential file aside: %w", err)
	}
	testHookAside()
	if !holds(aside, rejected) {
		// Not ours: put it back, unless a newer one is there by now.
		if err := os.Link(aside, path); err != nil && !errors.Is(err, os.ErrExist) {
			return CredentialKept, fmt.Errorf("put the credential file back from %s: %w", aside, err)
		}
		return CredentialKept, removeAside(aside)
	}
	var err error
	if whileAside != nil {
		err = whileAside()
	}
	return CredentialRemoved, errors.Join(err, removeAside(aside))
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
