package ledger

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type snapshotEntry struct {
	store.LedgerEntry
	bodyHash [sha256.Size]byte
	blob     plumbing.Hash
}

type snapshot struct {
	entries map[uint32]snapshotEntry
	byBlob  map[plumbing.Hash]snapshotEntry
	trees   map[string]plumbing.Hash
	index   plumbing.Hash
}

type fileObject struct {
	path string
	hash plumbing.Hash
}

func (r *repository) walkFiles(ctx context.Context, hash plumbing.Hash, dir string, visit func(fileObject) error) error {
	return r.walkTree(ctx, hash, dir, nil, visit)
}

func (r *repository) walkTree(ctx context.Context, hash plumbing.Hash, dir string, trees map[string]plumbing.Hash, visit func(fileObject) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tree, err := r.tree(hash)
	if err != nil {
		return err
	}
	if len(tree.Entries) == 0 || len(tree.Entries) > store.MaxLedgerDecisions+1 {
		return fmt.Errorf("ledger: invalid tree size at %q", dir)
	}
	if trees != nil {
		trees[dir] = hash
	}
	seen := make(map[string]struct{}, len(tree.Entries))
	for _, entry := range tree.Entries {
		p := dir + entry.Name
		if entry.Name == "" || strings.Contains(entry.Name, "/") {
			return fmt.Errorf("ledger: invalid tree entry %q", p)
		}
		if _, ok := seen[entry.Name]; ok {
			return fmt.Errorf("ledger: duplicate tree path %q", p)
		}
		seen[entry.Name] = struct{}{}
		switch entry.Mode {
		case filemode.Dir:
			var v validation
			v.checkDir(p, p)
			if len(v.problems) > 0 {
				return &ValidationError{Problems: v.problems}
			}
			if err := r.walkTree(ctx, entry.Hash, p+"/", trees, visit); err != nil {
				return err
			}
		case filemode.Regular:
			if err := visit(fileObject{path: p, hash: entry.Hash}); err != nil {
				return err
			}
		default:
			return fmt.Errorf("ledger: invalid file mode at %q", p)
		}
	}
	return nil
}

func (r *repository) readSnapshot(ctx context.Context, tree plumbing.Hash, cached snapshot, collectTrees bool) (snapshot, error) {
	if tree.IsZero() {
		return snapshot{}, nil
	}
	out := snapshot{entries: make(map[uint32]snapshotEntry), byBlob: make(map[plumbing.Hash]snapshotEntry)}
	if collectTrees {
		out.trees = make(map[string]plumbing.Hash)
	}
	var index, buffer []byte
	var v validation
	files := 0
	err := r.walkTree(ctx, tree, "", out.trees, func(file fileObject) error {
		files++
		if files > store.MaxLedgerDecisions+1 {
			return store.ErrLimit
		}
		if file.path == indexName {
			out.index = file.hash
			var err error
			index, err = r.blob(file.hash, maxIndexBytes, nil)
			return err
		}
		if entry, ok := cached.byBlob[file.hash]; ok {
			entry.Path, entry.Dir = file.path, fileDir(file.path)
			v.checkIdentity(file.path, Decision{Number: entry.Number, Dir: entry.Dir, Title: entry.Title})
			out.entries[entry.Number] = entry
			out.byBlob[file.hash] = entry
			return nil
		}
		data, err := r.blob(file.hash, MaxDecisionFileBytes, buffer)
		if err != nil {
			return fmt.Errorf("ledger: read %q: %w", file.path, err)
		}
		buffer = data
		d, ok := v.parseFile(file.path, data)
		if ok {
			item := snapshotEntry{LedgerEntry: entryFor(file.path, d), bodyHash: sha256.Sum256(d.Body), blob: file.hash}
			out.entries[d.Number] = item
			out.byBlob[file.hash] = item
		}
		return nil
	})
	if err != nil {
		return snapshot{}, err
	}
	entries := make([]store.LedgerEntry, 0, len(out.entries))
	for _, e := range out.entries {
		entries = append(entries, e.LedgerEntry)
	}
	v.checkIndex(index, entries)
	if len(v.problems) != 0 {
		return snapshot{}, &ValidationError{Problems: v.problems}
	}
	return out, nil
}

func (r *repository) putBlob(data []byte) (plumbing.Hash, error) {
	hash := plumbing.ComputeHash(plumbing.BlobObject, data)
	return hash, r.storeObject(plumbing.BlobObject, hash, data)
}

func (r *repository) streamBlob(hash plumbing.Hash, header, body []byte) error {
	return r.storeObject(plumbing.BlobObject, hash, header, body)
}

func (r *repository) putObject(o plumbing.EncodedObject) (plumbing.Hash, error) {
	reader, err := o.Reader()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	data, err := io.ReadAll(reader)
	if err := errors.Join(err, reader.Close()); err != nil {
		return plumbing.ZeroHash, err
	}
	return o.Hash(), r.storeObject(o.Type(), o.Hash(), data)
}

// writeTree processes one directory at a time using sorted, complete paths.
func (r *repository) writeTree(ctx context.Context, files []fileObject, offset int, reusable map[string]plumbing.Hash) (plumbing.Hash, error) {
	if err := ctx.Err(); err != nil {
		return plumbing.ZeroHash, err
	}
	if len(files) > 0 {
		if hash, ok := reusable[files[0].path[:offset]]; ok {
			return hash, nil
		}
	}
	t := &object.Tree{}
	for len(files) > 0 {
		f := files[0]
		name, _, subdir := strings.Cut(f.path[offset:], "/")
		if !subdir {
			t.Entries = append(t.Entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: f.hash})
			files = files[1:]
			continue
		}
		end := offset + len(name) + 1
		prefix := f.path[:end]
		n := 1
		for n < len(files) && strings.HasPrefix(files[n].path, prefix) {
			n++
		}
		hash, err := r.writeTree(ctx, files[:n], end, reusable)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		t.Entries = append(t.Entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: hash})
		files = files[n:]
	}
	sort.Sort(object.TreeEntrySorter(t.Entries))
	o := r.storage.NewEncodedObject()
	if err := t.Encode(o); err != nil {
		return plumbing.ZeroHash, err
	}
	return r.putObject(o)
}

func sortedFiles(files []fileObject) {
	slices.SortFunc(files, func(a, b fileObject) int { return strings.Compare(a.path, b.path) })
}

type preparedDecision struct {
	snapshotEntry
	header []byte
	body   []byte
}

// Prepare and validate the entire input before touching the branch. A
// complete input requires hashing its bytes once, with one reusable file
// buffer. Keep only metadata and references to the caller's bodies.
func prepareSnapshot(ctx context.Context, decisions []Decision) ([]preparedDecision, snapshot, error) {
	v := validation{count: len(decisions)}
	v.checkCount()
	if len(decisions) > store.MaxLedgerDecisions {
		return nil, snapshot{}, &ValidationError{Problems: v.problems}
	}
	prepared := make([]preparedDecision, 0, len(decisions))
	out := snapshot{byBlob: make(map[plumbing.Hash]snapshotEntry, len(decisions))}
	files := make(map[string]struct{}, len(decisions)+1)
	files[indexName] = struct{}{}
	var scratch []byte
	for _, d := range decisions {
		if err := ctx.Err(); err != nil {
			return nil, snapshot{}, err
		}
		p := decisionPath(d)
		initialProblems := len(v.problems)
		v.checkDecision(p, d)
		if _, exists := files[p]; exists {
			v.add(p, "duplicate file path")
		}
		files[p] = struct{}{}
		e := preparedDecision{snapshotEntry: snapshotEntry{LedgerEntry: entryFor(p, d)}, body: d.Body}
		if len(d.Title) <= store.MaxLedgerTitleBytes && utf8.ValidString(d.Title) {
			header := appendHeader(nil, d.Number, d.Title)
			if len(d.Body) > MaxDecisionFileBytes-len(header) {
				v.add(p, "decision file exceeds 256 KiB")
			}
			if len(v.problems) == initialProblems {
				scratch = append(append(scratch[:0], header...), d.Body...)
				parsed, err := Parse(scratch)
				if err != nil {
					v.add(p, "rendered front matter cannot be read: "+err.Error())
				} else if parsed.Number != d.Number || parsed.Title != d.Title || !bytes.Equal(parsed.Body, d.Body) {
					v.add(p, "rendered file does not preserve its number, title and body")
				} else {
					e.header = header
					e.blob = plumbing.ComputeHash(plumbing.BlobObject, scratch)
					out.byBlob[e.blob] = e.snapshotEntry
				}
			}
		}
		prepared = append(prepared, e)
	}
	for _, e := range prepared {
		for dir := e.Dir; dir != ""; dir = fileDir(dir) {
			if _, ok := files[dir]; ok {
				v.add(e.Path, "directory conflicts with file "+dir)
			}
		}
	}
	if len(v.problems) != 0 {
		return nil, snapshot{}, &ValidationError{Problems: v.problems}
	}
	return prepared, out, nil
}

func (r *repository) putSnapshot(ctx context.Context, prepared []preparedDecision, previous snapshot) (plumbing.Hash, snapshot, error) {
	files := make([]fileObject, 0, len(prepared)+1)
	entries := make([]store.LedgerEntry, 0, len(prepared))
	out := snapshot{entries: make(map[uint32]snapshotEntry, len(prepared))}
	indexChanged := previous.index.IsZero()
	invalidate := func(path string) {
		delete(previous.trees, "")
		for dir := fileDir(path); dir != ""; dir = fileDir(dir) {
			delete(previous.trees, dir+"/")
		}
	}
	for _, e := range prepared {
		if err := ctx.Err(); err != nil {
			return plumbing.ZeroHash, snapshot{}, err
		}
		old, exists := previous.entries[e.Number]
		legacy := false
		if exists && old.blob == e.blob {
			e.bodyHash = old.bodyHash
		} else if exists {
			e.bodyHash = sha256.Sum256(e.body)
			if old.Title == e.Title && old.bodyHash == e.bodyHash {
				// A legacy plain title can share the old blob when the
				// new rendering uses quotes but the decision is identical.
				e.blob = old.blob
				legacy = true
			}
		}
		// A legacy blob was written from other bytes than the caller's, so
		// only blobs rendered from the caller's bytes can be repaired here.
		if _, reachable := previous.byBlob[e.blob]; !reachable || !legacy && r.suspectBlob(e.blob) {
			if err := r.streamBlob(e.blob, e.header, e.body); err != nil {
				return plumbing.ZeroHash, snapshot{}, err
			}
		}
		indexChanged = indexChanged || !exists || old.Dir != e.Dir || old.Title != e.Title
		if !exists || old.Path != e.Path || old.blob != e.blob {
			invalidate(e.Path)
			if exists {
				invalidate(old.Path)
			}
		}
		files = append(files, fileObject{path: e.Path, hash: e.blob})
		entries = append(entries, e.LedgerEntry)
		out.entries[e.Number] = e.snapshotEntry
	}
	table := previous.index
	if indexChanged {
		var err error
		table, err = r.putBlob(renderIndex(entries))
		if err != nil {
			return plumbing.ZeroHash, snapshot{}, err
		}
		invalidate(indexName)
	}
	files = append(files, fileObject{path: indexName, hash: table})
	sortedFiles(files)
	tree, err := r.writeTree(ctx, files, 0, previous.trees)
	return tree, out, err
}
