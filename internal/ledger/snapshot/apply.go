package snapshot

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// ErrChanged reports a file that no longer holds what an edit expects:
// someone else wrote it. The edit was not applied and the file is left as
// that writer left it.
var ErrChanged = errors.New("the file changed")

// Applier applies edits to a snapshot that its user may be writing at the
// same time, so that no write of theirs is replaced or removed unseen. A
// file to replace or delete is first renamed into the staging directory
// (aside) and hashed there: only the content it was expected to hold is
// discarded; anything else is put back and the edit fails with
// ErrChanged. A new file is installed only where nothing exists (an
// exclusive rename, see renameNoReplace), so a file created at the path
// meanwhile is never replaced. What remains is a write through a
// descriptor the writer opened before the rename that moved its file
// aside: when it lands before the moved file is hashed, it is part of what
// is hashed and goes back; when it lands after (microseconds before the
// moved file is discarded or replaced), it is lost. That is inherent to
// renaming a file someone may hold open.
//
// Before moving a file aside, the Applier records its path and expected
// hash in a journal file beside it, so that a process killed in between
// leaves enough for Recover. Directory syncs are deferred to Sync, once per directory.
type Applier struct {
	dir, staging *os.File
	// tmp, aside and journal are names inside staging.
	tmp, aside, journal string
	touched             map[string]bool
}

// NewApplier applies edits to dir, using staging (a directory on the same
// file system outside dir) for the files named tmp and aside, and
// aside+".path" for the journal.
func NewApplier(dir, staging *os.File, tmp, aside string) *Applier {
	return &Applier{dir: dir, staging: staging, tmp: tmp, aside: aside, journal: aside + ".path", touched: map[string]bool{}}
}

func parentPath(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return ""
}

// Delete removes p if it holds content of hash expected. Nothing at p is
// success: the target is reached.
func (a *Applier) Delete(p, expected string) error {
	if !ValidPath(p) {
		return fmt.Errorf("%w: %q", ErrInvalidPath, p)
	}
	dirFd, name, err := openParent(a.dir, p, false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(dirFd)
	moved, err := a.moveAside(dirFd, name, p, expected)
	if err != nil || !moved {
		return err
	}
	if err := a.checkAside(dirFd, name, expected); err != nil {
		return err
	}
	a.touched[parentPath(p)] = true
	return a.dropAside()
}

// Replace installs content at p if p holds content of hash expected, or
// nothing when expected is empty.
func (a *Applier) Replace(p, expected, content string) error {
	if !ValidPath(p) {
		return fmt.Errorf("%w: %q", ErrInvalidPath, p)
	}
	stagingFd := int(a.staging.Fd())
	if err := unix.Unlinkat(stagingFd, a.tmp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "remove", Path: a.tmp, Err: err}
	}
	fd, err := unix.Openat(stagingFd, a.tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return &os.PathError{Op: "create", Path: a.tmp, Err: err}
	}
	f := os.NewFile(uintptr(fd), a.tmp)
	_, err = io.WriteString(f, content)
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		_ = unix.Unlinkat(stagingFd, a.tmp, 0)
		return err
	}
	defer unix.Unlinkat(stagingFd, a.tmp, 0) //nolint:errcheck // gone after a successful install
	dirFd, name, err := openParent(a.dir, p, true)
	if err != nil {
		return err
	}
	defer unix.Close(dirFd)
	moved := false
	if expected != "" {
		if moved, err = a.moveAside(dirFd, name, p, expected); err != nil {
			return err
		}
		if !moved {
			// Deleted by its writer: that deletion stays.
			return ErrChanged
		}
		if err := a.checkAside(dirFd, name, expected); err != nil {
			return err
		}
	}
	a.touched[parentPath(p)] = true
	err = renameNoReplace(stagingFd, a.tmp, dirFd, name)
	if errors.Is(err, unix.EEXIST) {
		// Created by its writer meanwhile: it stays, and supersedes the
		// expected content moved aside.
		err = ErrChanged
	} else if err != nil {
		err = &os.PathError{Op: "rename", Path: p, Err: err}
		if moved {
			return errors.Join(err, a.putBack(dirFd, name, expected))
		}
		return err
	}
	if moved {
		return errors.Join(err, a.dropAside())
	}
	return err
}

// journalEntry is what the journal records before a file is moved aside.
type journalEntry struct {
	Path string `json:"path"`
	// Expected is the hash of the content the edit expects there.
	Expected string `json:"expected"`
}

// afterMoveAside, for tests, runs right after a file was moved aside; an
// error returns at once, leaving the aside file and the journal as a
// killed process would.
var afterMoveAside func() error

// moveAside records p and the content it is expected to hold in the
// journal and renames p into staging as the aside file. It reports false
// when nothing is at p.
func (a *Applier) moveAside(dirFd int, name, p, expected string) (bool, error) {
	entry, err := json.Marshal(journalEntry{Path: p, Expected: expected})
	if err != nil {
		return false, err
	}
	// An aside left by a killed run is Recover's: never overwrite it or
	// its journal.
	var st unix.Stat_t
	if err := unix.Fstatat(int(a.staging.Fd()), a.journal, &st, unix.AT_SYMLINK_NOFOLLOW); !errors.Is(err, unix.ENOENT) {
		return false, fmt.Errorf("%s from an interrupted run is still there; recover it first", a.journal)
	}
	if err := writeAtomic(a.staging, a.journal, entry, 0o600); err != nil {
		return false, err
	}
	stagingFd := int(a.staging.Fd())
	err = unix.Renameat(dirFd, name, stagingFd, a.aside)
	if errors.Is(err, unix.ENOENT) {
		return false, a.dropJournal()
	}
	if err != nil {
		return false, errors.Join(&os.PathError{Op: "rename", Path: p, Err: err}, a.dropJournal())
	}
	if afterMoveAside != nil {
		if err := afterMoveAside(); err != nil {
			return false, err
		}
	}
	return true, nil
}

// asideHash returns the hash of the aside file, or "" when it is not a
// regular file of at most MaxIndexBytes, the largest file an edit may
// expect (the generated index).
func (a *Applier) asideHash() (string, error) {
	_, data, err := readRegular(int(a.staging.Fd()), a.aside, nil, MaxIndexBytes)
	switch {
	case errors.Is(err, errNotRegular):
		return "", nil
	case err != nil:
		return "", err
	case len(data) > MaxIndexBytes:
		return "", nil
	}
	return Hash(data), nil
}

// checkAside puts the aside file back and returns ErrChanged unless it is
// a regular file of hash expected.
func (a *Applier) checkAside(dirFd int, name, expected string) error {
	sum, err := a.asideHash()
	if err != nil {
		return errors.Join(err, a.putBack(dirFd, name, expected))
	}
	if sum != "" && sum == expected {
		return nil
	}
	return errors.Join(ErrChanged, a.putBack(dirFd, name, expected))
}

// putBack renames the aside file back to name in dirFd. When something
// was created there meanwhile, that newer file stays: an aside holding
// the expected content (known to whoever sent the edit) is dropped;
// anything else, a write nobody collected yet, is kept beside it under a
// conflict name (see restoreConflict).
func (a *Applier) putBack(dirFd int, name, expected string) error {
	err := renameNoReplace(int(a.staging.Fd()), a.aside, dirFd, name)
	if errors.Is(err, unix.EEXIST) {
		return a.settle(dirFd, name, expected)
	}
	if err != nil {
		return err
	}
	return a.dropJournal()
}

// settle disposes of an aside file that cannot go back to its path: it is
// dropped when it holds the expected content, and otherwise restored in
// dirFd under a conflict name derived from name.
func (a *Applier) settle(dirFd int, name, expected string) error {
	sum, err := a.asideHash()
	if err != nil {
		return err
	}
	if sum != "" && sum == expected {
		return a.dropAside()
	}
	return a.restoreConflict(dirFd, name, sum)
}

// conflictName is "<name>.conflict-<tag>", with name shortened (on a rune
// boundary) so the result is a valid segment.
func conflictName(name, tag string) string {
	suffix := ".conflict-" + tag
	if max := maxSegmentBytes - len(suffix); len(name) > max {
		for max > 0 && !utf8.RuneStart(name[max]) {
			max--
		}
		name = name[:max]
	}
	return name + suffix
}

// restoreConflict renames the aside file into dirFd as
// "<name>.conflict-<first 12 digits of its hash>", where the agent sees it
// and the next collection reports it as an added file. A file of the same
// name already holds the same content, and the aside file is dropped.
// Anything but a regular file (sum empty) gets a random tag.
func (a *Applier) restoreConflict(dirFd int, name, sum string) error {
	for range 3 {
		tag := sum
		if tag == "" {
			var b [6]byte
			_, _ = rand.Read(b[:])
			tag = hex.EncodeToString(b[:])
		}
		err := renameNoReplace(int(a.staging.Fd()), a.aside, dirFd, conflictName(name, tag[:12]))
		if err == nil {
			return errors.Join(syncFd(dirFd), a.dropJournal())
		}
		if !errors.Is(err, unix.EEXIST) {
			return err
		}
		if sum != "" {
			return a.dropAside()
		}
	}
	return errors.New("no free conflict name")
}

func (a *Applier) dropAside() error {
	if err := unix.Unlinkat(int(a.staging.Fd()), a.aside, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "remove", Path: a.aside, Err: err}
	}
	return a.dropJournal()
}

func (a *Applier) dropJournal() error {
	if err := unix.Unlinkat(int(a.staging.Fd()), a.journal, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "remove", Path: a.journal, Err: err}
	}
	return nil
}

// Sync syncs every snapshot directory an edit changed, and the staging
// directory.
func (a *Applier) Sync() error {
	var errs []error
	for _, p := range slices.Sorted(func(yield func(string) bool) {
		for p := range a.touched {
			if !yield(p) {
				return
			}
		}
	}) {
		fd, _, err := openParent(a.dir, p+"/x", false)
		if p == "" {
			fd, err = unix.Openat(int(a.dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		}
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err == nil {
			err = syncFd(fd)
			_ = unix.Close(fd)
		}
		errs = append(errs, err)
	}
	return errors.Join(append(errs, syncFd(int(a.staging.Fd())))...)
}

// Recover finishes what a killed Applier left, whatever the snapshot's
// writer did since, without recreating anything they removed:
//
//   - a file moved aside goes back to its path when that is free;
//   - when the path is taken (by the edit's new file or a newer file of
//     the writer, a directory included), an aside holding the expected
//     content is dropped, and anything else is restored beside it under a
//     conflict name;
//   - when a directory on the way is gone or is no longer a directory, the
//     aside is dropped if it holds the expected content and otherwise
//     restored under a conflict name in the nearest directory that is
//     still there, so a removed directory is never recreated;
//   - a journal that cannot be read or parsed gets the aside restored as
//     "recovered.conflict-<tag>" at the snapshot's top.
//
// A restored conflict file is an ordinary file of the snapshot: the next
// collection reports it, so nothing a writer wrote is lost unseen. It
// reports whether anything was left.
func (a *Applier) Recover() (bool, error) {
	stagingFd := int(a.staging.Fd())
	var st unix.Stat_t
	if err := unix.Fstatat(stagingFd, a.journal, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		// Without a journal nothing was moved aside yet.
		return false, nil
	}
	if err := unix.Fstatat(stagingFd, a.aside, &st, unix.AT_SYMLINK_NOFOLLOW); errors.Is(err, unix.ENOENT) {
		return true, a.dropJournal()
	}
	var entry journalEntry
	data, err := readJournal(stagingFd, a.journal)
	if err != nil || json.Unmarshal(data, &entry) != nil || !ValidPath(entry.Path) || entry.Expected != "" && !ValidHash(entry.Expected) {
		top, err := unix.Openat(int(a.dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if err != nil {
			return true, err
		}
		defer unix.Close(top)
		sum, err := a.asideHash()
		if err != nil {
			return true, err
		}
		return true, a.restoreConflict(top, "recovered", sum)
	}
	dirFd, name, err := openParent(a.dir, entry.Path, false)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ELOOP) {
		near, err := a.nearestDir(entry.Path)
		if err != nil {
			return true, err
		}
		defer unix.Close(near)
		return true, errors.Join(a.settle(near, entry.Path[strings.LastIndexByte(entry.Path, '/')+1:], entry.Expected), syncFd(stagingFd))
	}
	if err != nil {
		return true, err
	}
	defer unix.Close(dirFd)
	if err := a.putBack(dirFd, name, entry.Expected); err != nil {
		return true, err
	}
	return true, errors.Join(syncFd(dirFd), syncFd(stagingFd))
}

// nearestDir opens the deepest directory on the way to p that is still a
// directory (not a symlink), the snapshot's top at least.
func (a *Applier) nearestDir(p string) (int, error) {
	fd, err := unix.Openat(int(a.dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	segments := strings.Split(p, "/")
	for _, seg := range segments[:len(segments)-1] {
		next, err := unix.Openat(fd, seg, dirFlags, 0)
		if err != nil {
			break
		}
		_ = unix.Close(fd)
		fd = next
	}
	return fd, nil
}

// readJournal reads a journal of at most a path's escaped size.
func readJournal(dirFd int, name string) ([]byte, error) {
	fd, err := unix.Openat(dirFd, name, readFlags, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 2*MaxPathBytes+256))
}

// SyncDir syncs the directory fd as os.File.Sync would (F_FULLFSYNC on
// macOS), for callers holding a raw descriptor.
func SyncDir(fd int) error { return syncFd(fd) }
