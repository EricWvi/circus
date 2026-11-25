package snapshot

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// Bounds shared by the daemon and the CLI.
const (
	// MaxEntries bounds the directory entries one scan reads.
	MaxEntries = 8192
	// MaxDepth bounds the directory levels one scan descends, and so the
	// descriptors it holds.
	MaxDepth = 128
	// MaxFileBytes bounds one file's content, except the generated index.
	MaxFileBytes = 256 << 10
	// IndexPath is the ledger's generated index, at the top. It is the one
	// file allowed to exceed MaxFileBytes, up to MaxIndexBytes, and it does
	// not count toward a chunk's or a rebase's content bound.
	IndexPath = "INDEX.md"
	// MaxIndexBytes bounds the generated index: a header and one row per
	// decision, at most 4096, of a number (11 bytes), a directory of at
	// most 256 bytes, which needs no escaping, a title of at most 512
	// bytes, whose '<', '>' and '&' take five bytes each as entities, and
	// the separators.
	MaxIndexBytes = 64 + 4096*(32+256+5*512)
	// MaxPathBytes bounds one path.
	MaxPathBytes = 4096
	// maxSegmentBytes is the longest name common file systems hold.
	maxSegmentBytes = 255
	// MaxChanges and MaxChangedBytes are the default Limits: what one
	// collect_ledger result carries.
	MaxChanges      = 4096
	MaxChangedBytes = 8 << 20
	// maxManifestBytes bounds a manifest file read back: MaxEntries
	// entries whose paths, at their longest, are escaped to twice their
	// size, plus the fixed fields.
	maxManifestBytes = MaxEntries * (2*MaxPathBytes + 256)
	// manifestVersion is the format of manifests written now.
	manifestVersion = 1
)

// FoldKey is the name a case-insensitive, normalization-insensitive file
// system (macOS by default) sees for p: canonical caseless matching. Two
// paths with the same key are one name there.
func FoldKey(p string) string {
	return norm.NFC.String(cases.Fold().String(norm.NFD.String(p)))
}

// MaxBytes is the most content the file at p may hold.
func MaxBytes(p string) int {
	if p == IndexPath {
		return MaxIndexBytes
	}
	return MaxFileBytes
}

// Entry is one base file in a manifest.
type Entry struct {
	Path string `json:"path"`
	// Size is negative for an entry known only by its hash, which no stat
	// ever matches.
	Size    int64 `json:"size"`
	MtimeNs int64 `json:"mtime_ns"`
	// CtimeNs is the inode change time, which no one can set: it catches
	// a rewrite whose modification time was set back. Zero (a manifest
	// written before it was recorded) matches no file.
	CtimeNs int64  `json:"ctime_ns"`
	Inode   uint64 `json:"inode"`
	// SHA256 is the lowercase hex SHA-256 of the content.
	SHA256 string `json:"sha256"`
}

// Manifest describes the base a snapshot directory was written from.
type Manifest struct {
	Version    int    `json:"version"`
	BaseCommit string `json:"base_commit"`
	// WrittenAtNs is a file system timestamp taken when the manifest was
	// made; files modified at or after it are always hashed.
	WrittenAtNs int64 `json:"written_at_ns"`
	// Files are in path order, without duplicates.
	Files []Entry `json:"files"`
}

// ErrInvalidPath reports a path that does not meet ValidPath.
var ErrInvalidPath = errors.New("invalid snapshot path")

// ValidPath reports whether p is a relative, '/'-separated path of valid
// UTF-8 without control characters, empty, "." or ".." segments, at most
// MaxPathBytes long with segments of at most 255 bytes.
func ValidPath(p string) bool {
	if p == "" || len(p) > MaxPathBytes || !utf8.ValidString(p) {
		return false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." || len(seg) > maxSegmentBytes || hasControl(seg) {
			return false
		}
	}
	return true
}

func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

// ValidCommit reports whether c is a full lowercase hex commit id (SHA-1
// or SHA-256).
func ValidCommit(c string) bool {
	return (len(c) == 40 || len(c) == 64) && isLowerHex(c)
}

// ValidHash reports whether h is a lowercase hex SHA-256.
func ValidHash(h string) bool { return len(h) == 64 && isLowerHex(h) }

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Hash returns the lowercase hex SHA-256 of content.
func Hash[T string | []byte](content T) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func (m *Manifest) validate() error {
	if m.Version != manifestVersion {
		return fmt.Errorf("unknown manifest version %d", m.Version)
	}
	if m.BaseCommit != "" && !ValidCommit(m.BaseCommit) {
		return errors.New("invalid base commit")
	}
	if len(m.Files) > MaxEntries {
		return fmt.Errorf("more than %d files", MaxEntries)
	}
	for i, e := range m.Files {
		if !ValidPath(e.Path) || !ValidHash(e.SHA256) {
			return fmt.Errorf("invalid entry %q", e.Path)
		}
		if i > 0 && m.Files[i-1].Path >= e.Path {
			return errors.New("entries are not in path order")
		}
	}
	return nil
}

// Lookup returns the entry for p, or nil.
func (m *Manifest) Lookup(p string) *Entry {
	i, ok := slices.BinarySearchFunc(m.Files, p, func(e Entry, p string) int { return strings.Compare(e.Path, p) })
	if !ok {
		return nil
	}
	return &m.Files[i]
}

// ErrNoManifest reports a manifest file that does not exist.
var ErrNoManifest = errors.New("no manifest")

// ReadManifest reads the manifest name inside dir, refusing a symlink.
func ReadManifest(dir *os.File, name string) (*Manifest, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, ErrNoManifest
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: name, Err: err}
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, &os.PathError{Op: "stat", Path: name, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("manifest %s is not a regular file", name)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxManifestBytes+1))
	if err != nil {
		return nil, &os.PathError{Op: "read", Path: name, Err: err}
	}
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("manifest %s is larger than %d bytes", name, maxManifestBytes)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", name, err)
	}
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", name, err)
	}
	return &m, nil
}

// ReadManifestFile reads the manifest at path.
func ReadManifestFile(path string) (*Manifest, error) {
	dir, err := os.Open(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoManifest
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return ReadManifest(dir, filepath.Base(path))
}

// WriteManifest replaces the manifest name inside dir atomically: it
// writes name+".tmp", syncs it, renames it onto name and syncs dir. The
// caller serializes writers of one name.
func WriteManifest(dir *os.File, name string, m *Manifest) error {
	m.Version = manifestVersion
	if err := m.validate(); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(m); err != nil {
		return err
	}
	return writeAtomic(dir, name, buf.Bytes(), 0o600)
}

// WriteManifestFile writes the manifest at path as WriteManifest does.
func WriteManifestFile(path string, m *Manifest) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return WriteManifest(dir, filepath.Base(path), m)
}

// RemoveManifest removes the manifest name inside dir and its leftover
// temporary file, then syncs dir. It reports whether anything was there.
func RemoveManifest(dir *os.File, name string) (bool, error) {
	fd := int(dir.Fd())
	removed := false
	for _, n := range []string{name + ".tmp", name} {
		switch err := unix.Unlinkat(fd, n, 0); {
		case err == nil:
			removed = true
		case !errors.Is(err, unix.ENOENT):
			return removed, &os.PathError{Op: "remove", Path: n, Err: err}
		}
	}
	if removed {
		if err := dir.Sync(); err != nil {
			return true, err
		}
	}
	return removed, nil
}

// writeAtomic writes data to name inside dir through name+".tmp", synced
// and renamed into place, and syncs dir.
func writeAtomic(dir *os.File, name string, data []byte, mode uint32) error {
	dirFd, tmp := int(dir.Fd()), name+".tmp"
	if err := unix.Unlinkat(dirFd, tmp, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "remove", Path: tmp, Err: err}
	}
	fd, err := unix.Openat(dirFd, tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return &os.PathError{Op: "create", Path: tmp, Err: err}
	}
	f := os.NewFile(uintptr(fd), tmp)
	_, err = f.Write(data)
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		_ = unix.Unlinkat(dirFd, tmp, 0)
		return err
	}
	if err := unix.Renameat(dirFd, tmp, dirFd, name); err != nil {
		_ = unix.Unlinkat(dirFd, tmp, 0)
		return &os.PathError{Op: "rename", Path: name, Err: err}
	}
	return dir.Sync()
}
