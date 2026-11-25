// Package snapshot keeps a plain directory holding a copy of a ledger (a
// snapshot) and finds what was changed in it, for the device daemon's
// session snapshots and for the circus ledger CLI's local copies.
//
// A Manifest records, for each file of the base the directory was written
// from, its path, size, modification and inode change times (ns), inode
// and SHA-256, plus
// the base commit and the time the manifest was taken (WrittenAtNs). It is
// stored as JSON outside the directory and replaced atomically (temporary
// file, fsync, rename, fsync of the directory).
//
// Compare works as git's index does (with core.trustctime, git's
// default): a regular file whose size, modification time, inode change
// time and inode equal its manifest entry is unchanged and is not opened;
// any other file is read and hashed. The inode change time, which no one
// can set, catches a same-size rewrite whose modification time was set
// back (touch -d, cp -p). A file whose modification
// time is not earlier than WrittenAtNs is "racy" and always hashed: it may
// have been changed again within the same timestamp tick after it was
// recorded. WrittenAtNs is a timestamp of the directory's own file system,
// taken by creating and removing a probe file in a Clock directory on that
// file system but outside the snapshot (a process killed while the probe
// exists leaves it where no scan sees it), so it has that file system's
// granularity. Writer tries its Clocks in order and uses the first on the
// directory's file system whose probe can be made; without one it takes
// the newest file's modification time instead, and the files of that last
// tick stay racy.
// Before taking it, Writer and Rebuild wait (at most
// 50 ms) for that clock to move past the files already written, so a
// freshly written file is not racy; where the clock does not move in time,
// such files stay racy, which costs a read, never a missed change.
//
//   - Writer records each file's stat right after writing it and takes
//     the time after all files are written.
//   - Rebuild takes the time before it reads anything, so a file changed
//     while or after it is read has a modification time at or after it
//     and is hashed by every later Compare.
//
// Nothing is ever followed: directories are opened relative to their
// already-open parent with O_NOFOLLOW, other entries are examined with
// lstat semantics and regular files are opened with O_NOFOLLOW|O_NONBLOCK
// and checked again on the descriptor, so a symlink, FIFO, socket or
// device is reported as a Problem and never opened, and a FIFO cannot
// block a scan.
//
// Hidden names are ordinary names: a dot file or dot directory inside the
// snapshot (an editor's swap file, say) is compared, collected and
// rebuilt like any other, and whoever validates the changes decides what
// the ledger accepts. The callers keep their manifests, staging areas,
// locks and clock probes outside the directory, so a scan never meets
// them.
//
// Applier applies edits to a snapshot its user may be writing at the same
// time without replacing or removing any write of theirs unseen: see its
// documentation.
//
// Bounds: a scan reads at most MaxEntries directory entries (more is a
// Problem of the whole snapshot), descends at most MaxDepth directories, reads
// at most MaxFileBytes+1 bytes of any file (MaxIndexBytes+1 of the generated
// index, IndexPath; a larger one is a Problem with its size), and keeps contents only while the changed files total at most
// Limits.MaxChangedBytes; more than Limits.MaxChanges changes, or more
// content, sets Result.TooLarge. Paths are valid UTF-8 without control
// characters, at most MaxPathBytes long, with segments of at most 255 bytes.
//
// Memory of one Compare is bounded by those numbers: the changed contents
// kept (MaxChangedBytes, 8 MiB by default) plus one read buffer
// (MaxFileBytes+1, or MaxIndexBytes+1 while the index is read), the manifest (at most MaxEntries entries), the changes
// and problems (at most MaxChanges+1 and MaxEntries entries, each holding a
// path of at most MaxPathBytes) and one batch of names per open directory
// level. Typical ledgers use a few megabytes; the theoretical worst case,
// with every path at its 4096-byte limit, is about 8 MiB of content plus
// roughly 100 MiB of paths across the manifest's JSON, the manifest, the
// changes and the problems.
package snapshot
