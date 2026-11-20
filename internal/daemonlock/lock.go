// Package daemonlock coordinates device daemon processes in the same state directory.
package daemonlock

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/EricWvi/circus/internal/circushome"
)

// Mode identifies how a daemon was started.
type Mode string

const (
	// Foreground is a daemon launched directly by the user.
	Foreground Mode = "foreground"
	// Service is a daemon launched by a service manager.
	Service Mode = "service"
)

// HeldError identifies the daemon that currently holds the lock.
type HeldError struct {
	PID  int
	Mode Mode
}

func (e *HeldError) Error() string {
	if e.PID == 0 {
		return "another circus daemon is running (lock owner unavailable)"
	}
	message := fmt.Sprintf("another circus daemon is running (pid %d, %s)", e.PID, e.Mode)
	if e.Mode == Service {
		message += "; `circus daemon stop` it first"
	}
	return message
}

// Lock holds an exclusive flock for the daemon's lifetime.
type Lock struct{ file *os.File }

// Acquire records this process as the daemon for its state directory. A
// conflicting owner is reported as a HeldError.
func Acquire(mode Mode) (*Lock, error) {
	if mode != Foreground && mode != Service {
		return nil, fmt.Errorf("unknown daemon lock mode %q", mode)
	}
	files, err := open(true)
	if err != nil {
		return nil, err
	}
	defer releaseGuard(files.guard)
	f := files.file
	if err := check(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	lock := &Lock{file: f}
	if err := f.Truncate(0); err != nil {
		_ = lock.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(f, "%d %s\n", os.Getpid(), mode); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

// Holder returns the owner, if any, without creating the state directory.
func Holder() (*HeldError, error) {
	files, err := open(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer releaseGuard(files.guard)
	err = check(files.file)
	if err == nil {
		return nil, (&Lock{file: files.file}).Close()
	}
	_ = files.file.Close()
	var held *HeldError
	if errors.As(err, &held) {
		return held, nil
	}
	return nil, err
}

// Close releases the flock before closing the descriptor.
func (l *Lock) Close() error {
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return errors.Join(err, l.file.Close())
}

type lockFiles struct {
	file  *os.File
	guard *os.File
}

func open(create bool) (*lockFiles, error) {
	dir, err := circushome.DaemonState("")
	if err != nil {
		return nil, err
	}
	if create {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	guard, err := os.OpenFile(dir, syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	// Serialize publishing and reading the owner record, including after a crash.
	// The directory flock is released as soon as the record is published or read.
	if err := syscall.Flock(int(guard.Fd()), syscall.LOCK_EX); err != nil {
		_ = guard.Close()
		return nil, err
	}
	flags := os.O_RDONLY
	if create {
		flags = os.O_CREATE | os.O_RDWR
	}
	path := filepath.Join(dir, "daemon.lock")
	// os.OpenFile also sets close-on-exec so tools cannot inherit this lock.
	f, err := os.OpenFile(path, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		releaseGuard(guard)
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil {
		st := fi.Sys().(*syscall.Stat_t)
		if !fi.Mode().IsRegular() || st.Nlink != 1 || st.Uid != uint32(os.Getuid()) {
			err = fmt.Errorf("unsafe daemon lock %s", path)
		}
	}
	if err != nil {
		_ = f.Close()
		releaseGuard(guard)
		return nil, err
	}
	return &lockFiles{file: f, guard: guard}, nil
}

func releaseGuard(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func check(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return err
	}
	var held HeldError
	if _, err := fmt.Fscan(io.LimitReader(f, 128), &held.PID, &held.Mode); err != nil ||
		held.PID <= 0 || (held.Mode != Foreground && held.Mode != Service) {
		return &HeldError{}
	}
	return &held
}
