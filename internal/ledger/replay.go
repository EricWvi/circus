package ledger

import (
	"bufio"
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"os"

	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
)

const replayPrefix = ".replay-"

// The caller holds the project's main.lock, or the exclusive startup lock.
func (m *Manager) rebuild(ctx context.Context, r *repository, projectID, indexed string, head plumbing.Hash) error {
	if head.IsZero() {
		return m.st.ReplaceLedgerIndex(ctx, projectID, indexed, "", false, nil, nil)
	}
	hashes, err := os.CreateTemp(m.root, replayPrefix)
	if err != nil {
		return err
	}
	defer os.Remove(hashes.Name())
	defer hashes.Close()
	count, prefix, err := r.spoolMain(ctx, head, indexed, hashes)
	if err != nil {
		return err
	}

	// Finish Git I/O before acquiring SQLite's only connection. Spool
	// versions so neither their number nor history length dictates RAM.
	versions, err := os.CreateTemp(m.root, replayPrefix)
	if err != nil {
		return err
	}
	defer os.Remove(versions.Name())
	defer versions.Close()
	buf := bufio.NewWriter(versions)
	encoder := gob.NewEncoder(buf)
	type latestEntry struct {
		store.LedgerEntry
		position int64
	}
	latest := make(map[uint32]latestEntry)
	suffix := 0
	for v, err := range r.versions(ctx, hashes, count) {
		if err != nil {
			return err
		}
		if err := encoder.Encode(v); err != nil {
			return err
		}
		if v.Position > prefix {
			suffix++
		}
		latest[v.Number] = latestEntry{position: v.Position, LedgerEntry: store.LedgerEntry{
			Number: v.Number, Dir: v.Dir, Title: v.Title, Path: v.Path, Commit: v.Commit, CommittedAt: v.CommittedAt,
		}}
	}
	if err := buf.Flush(); err != nil {
		return err
	}
	if _, err := versions.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// An indexed ancestor already represents this exact prefix, which the
	// replay has validated again. A short suffix is installed in one
	// transaction. A long one, like a missing prefix, replaces the whole
	// index, which the store does in bounded transactions.
	retainPrefix := prefix >= 0 && suffix <= store.MaxLedgerDecisions
	if !retainPrefix {
		prefix = -1
	}
	entries := make([]store.LedgerEntry, 0, len(latest))
	for _, e := range latest {
		if e.position > prefix {
			entries = append(entries, e.LedgerEntry)
		}
	}
	current, err := r.head()
	if err != nil {
		return err
	}
	if current != head {
		return fmt.Errorf("%w: ledger main changed during replay", store.ErrBusy)
	}
	return m.st.ReplaceLedgerIndex(ctx, projectID, indexed, head.String(), retainPrefix, entries, readVersions(versions, prefix))
}

// readVersions streams the spooled versions after the prefix position.
func readVersions(file *os.File, prefix int64) iter.Seq2[store.LedgerVersion, error] {
	return func(yield func(store.LedgerVersion, error) bool) {
		decoder := gob.NewDecoder(bufio.NewReader(file))
		for {
			var v store.LedgerVersion
			err := decoder.Decode(&v)
			if errors.Is(err, io.EOF) {
				return
			}
			if err == nil && v.Position <= prefix {
				continue
			}
			if !yield(v, err) || err != nil {
				return
			}
		}
	}
}

// spoolMain returns the commit count and the root-based position of the
// indexed head, or -1 when there is no reusable prefix.
func (r *repository) spoolMain(ctx context.Context, head plumbing.Hash, indexed string, file *os.File) (int64, int64, error) {
	w := bufio.NewWriter(file)
	var count int64
	indexedOffset := int64(-1)
	indexedHash := plumbing.NewHash(indexed)
	for hash := head; !hash.IsZero(); {
		if err := ctx.Err(); err != nil {
			return 0, -1, err
		}
		commit, err := r.commit(hash)
		if err != nil {
			return 0, -1, err
		}
		if _, err := w.Write(hash[:]); err != nil {
			return 0, -1, err
		}
		if hash == indexedHash {
			indexedOffset = count
		}
		count++
		if len(commit.ParentHashes) == 0 {
			break
		}
		hash = commit.ParentHashes[0]
	}
	prefix := int64(-1)
	if indexedOffset >= 0 && indexedOffset != 0 {
		prefix = count - indexedOffset - 1
	}
	return count, prefix, w.Flush()
}

// Replay remembers the metadata of at most this many recent blobs.
const replayBlobCache = 2 * store.MaxLedgerDecisions

// versions replays the first-parent line from the root. Blobs seen in
// recent snapshots, such as those of a reverted change, reuse their parsed
// metadata. Bodies never accumulate.
func (r *repository) versions(ctx context.Context, file *os.File, count int64) iter.Seq2[store.LedgerVersion, error] {
	return func(yield func(store.LedgerVersion, error) bool) {
		emitError := func(err error) { yield(store.LedgerVersion{}, err) }
		var previous snapshot
		known := snapshot{byBlob: make(map[plumbing.Hash]snapshotEntry)}
		var tree plumbing.Hash
		for position := int64(0); position < count; position++ {
			if err := ctx.Err(); err != nil {
				emitError(err)
				return
			}
			var hash plumbing.Hash
			if _, err := file.ReadAt(hash[:], (count-position-1)*int64(len(hash))); err != nil {
				emitError(err)
				return
			}
			commit, err := r.commit(hash)
			if err != nil {
				emitError(err)
				return
			}
			if commit.TreeHash == tree {
				continue
			}
			if len(known.byBlob) > replayBlobCache-store.MaxLedgerDecisions {
				known.byBlob = maps.Clone(previous.byBlob)
			}
			current, err := r.readSnapshot(ctx, commit.TreeHash, known, false)
			if err != nil {
				emitError(fmt.Errorf("ledger: read tree at %s: %w", hash, err))
				return
			}
			for _, old := range previous.entries {
				if _, ok := current.entries[old.Number]; !ok {
					emitError(&ValidationError{Problems: []Problem{{Path: old.Path, Message: "existing decisions cannot be removed"}}})
					return
				}
			}
			code := codeTrailer(commit.Message)
			for number, entry := range current.entries {
				old, existed := previous.entries[number]
				if existed && entry.Path == old.Path && entry.blob == old.blob {
					continue
				}
				v := version(commit, code, entry, old, existed)
				v.Position = position
				if changed(v) && !yield(v, nil) {
					return
				}
			}
			if known.byBlob == nil {
				known.byBlob = make(map[plumbing.Hash]snapshotEntry)
			}
			maps.Copy(known.byBlob, current.byBlob)
			previous, tree = current, commit.TreeHash
		}
	}
}
