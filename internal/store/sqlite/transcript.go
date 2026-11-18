package sqlite

import (
	"context"
	"fmt"

	"github.com/EricWvi/circus/internal/store"
)

func checkSubpath(subpath string) error {
	return store.CheckText("subpath", subpath, store.MaxSubpathBytes, true)
}

func (o *ops) AppendTranscript(ctx context.Context, f store.Fence, b store.TranscriptBatch, q store.TranscriptQuota) (store.TranscriptAppended, error) {
	if err := checkSubpath(b.Subpath); err != nil {
		return store.TranscriptAppended{}, err
	}
	if b.BatchSeq < 1 || len(b.Entries) == 0 {
		return store.TranscriptAppended{}, invalid("a transcript batch needs a batch sequence of at least 1 and entries")
	}
	if q.MaxEntryBytes < 1 || q.MaxEntryBytes > store.MaxTranscriptEntryBytes || q.MaxSessionBytes < 1 || q.MaxUserBytes < 1 {
		return store.TranscriptAppended{}, invalid("transcript quota needs positive bounds, the entry bound at most %d", store.MaxTranscriptEntryBytes)
	}
	var batchBytes int
	for _, e := range b.Entries {
		if len(e.JSON) == 0 {
			return store.TranscriptAppended{}, invalid("empty transcript entry")
		}
		if err := store.CheckOptionalID("transcript uuid", e.UUID); err != nil {
			return store.TranscriptAppended{}, err
		}
		batchBytes += len(e.JSON) + len(e.UUID)
	}
	if len(b.Entries) > 1 && batchBytes > store.MaxTranscriptBatchBytes {
		return store.TranscriptAppended{}, invalid("a batch of %d bytes: at most %d, or a single entry", batchBytes, store.MaxTranscriptBatchBytes)
	}
	var out store.TranscriptAppended
	err := o.fenced(ctx, f, func(tx *ops) error {
		var exists bool
		var lastSeq, bytes, batchEpoch, lastBatch int64
		err := mapErr(tx.q.QueryRow(ctx, `SELECT last_seq, bytes, batch_epoch, last_batch_seq FROM transcript_subpaths
			WHERE session_id = ? AND subpath = ?`, f.SessionID, b.Subpath).Scan(&lastSeq, &bytes, &batchEpoch, &lastBatch))
		switch {
		case err == nil:
			exists = true
		case !isNotFound(err):
			return err
		}
		s, err := tx.getSession(ctx, f.SessionID, f.Now)
		if err != nil {
			return err
		}
		out.LastSeq, out.SessionBytes = lastSeq, s.TranscriptBytes
		if !exists || batchEpoch != f.Epoch {
			lastBatch = 0 // a new epoch numbers its batches from 1
		}
		switch {
		case b.BatchSeq <= lastBatch:
			out.Deduplicated = true
			return nil
		case b.BatchSeq != lastBatch+1:
			return invalid("batch %d after %d: a batch is missing", b.BatchSeq, lastBatch)
		}
		// A charge cap refuses the batch and stops the session in the same
		// transaction; the refusal is a result, so the stop commits.
		full := func(reason error) error {
			if _, err := tx.exec(ctx, `UPDATE sessions SET state = 'stopped', stopped_at = ?1, reason = 'TRANSCRIPT_FULL',
				updated_at = ?1 WHERE session_id = ?2 AND state <> 'stopped'`, f.Now, f.SessionID); err != nil {
				return err
			}
			out = store.TranscriptAppended{LastSeq: lastSeq, SessionBytes: s.TranscriptBytes, Full: true, FullReason: reason}
			return nil
		}
		seen := map[string]bool{}
		var insert []store.TranscriptEntry
		var add int64
		for _, e := range b.Entries {
			if e.UUID != "" {
				if seen[e.UUID] {
					out.Skipped++
					continue
				}
				seen[e.UUID] = true
				if exists {
					var one int
					err := mapErr(tx.q.QueryRow(ctx, `SELECT 1 FROM transcript_entries
						WHERE session_id = ? AND subpath = ? AND uuid = ?`, f.SessionID, b.Subpath, e.UUID).Scan(&one))
					if err == nil {
						out.Skipped++
						continue
					}
					if !isNotFound(err) {
						return err
					}
				}
			}
			charge := store.TranscriptCharge(e)
			if charge > q.MaxEntryBytes {
				return full(fmt.Errorf("%w: entry of %d bytes over %d", store.ErrTranscriptFull, charge, q.MaxEntryBytes))
			}
			add += charge
			insert = append(insert, e)
		}
		if s.TranscriptBytes+add > q.MaxSessionBytes {
			return full(fmt.Errorf("%w: session at %d + %d bytes over %d", store.ErrTranscriptFull, s.TranscriptBytes, add, q.MaxSessionBytes))
		}
		var userBytes int64
		if err := tx.q.QueryRow(ctx, `SELECT transcript_bytes FROM users WHERE id = ?`, s.UserID).Scan(&userBytes); err != nil {
			return mapErr(err)
		}
		if userBytes+add > q.MaxUserBytes {
			return full(fmt.Errorf("%w: user at %d + %d bytes over %d", store.ErrTranscriptFull, userBytes, add, q.MaxUserBytes))
		}
		if !exists {
			var n int
			if err := tx.q.QueryRow(ctx, `SELECT COUNT(*) FROM transcript_subpaths WHERE session_id = ?`, f.SessionID).Scan(&n); err != nil {
				return mapErr(err)
			}
			if n >= store.MaxTranscriptSubpaths {
				return fmt.Errorf("%w: %d transcript subpaths", store.ErrLimit, n)
			}
			if _, err := tx.exec(ctx, `INSERT INTO transcript_subpaths (session_id, subpath) VALUES (?, ?)`, f.SessionID, b.Subpath); err != nil {
				return err
			}
		}
		for _, e := range insert {
			lastSeq++
			if _, err := tx.exec(ctx, `INSERT INTO transcript_entries (session_id, subpath, seq, uuid, entry, bytes, epoch)
				VALUES (?,?,?,?,?,?,?)`, f.SessionID, b.Subpath, lastSeq, nstr(e.UUID), e.JSON, store.TranscriptCharge(e), f.Epoch); err != nil {
				return err
			}
		}
		if _, err := tx.exec(ctx, `UPDATE transcript_subpaths SET last_seq = ?, bytes = bytes + ?, batch_epoch = ?,
			last_batch_seq = ? WHERE session_id = ? AND subpath = ?`, lastSeq, add, f.Epoch, b.BatchSeq, f.SessionID, b.Subpath); err != nil {
			return err
		}
		if err := tx.chargeTranscript(ctx, f.SessionID, s.UserID, add); err != nil {
			return err
		}
		out.Inserted, out.LastSeq, out.SessionBytes = len(insert), lastSeq, s.TranscriptBytes+add
		return nil
	})
	if err != nil {
		return store.TranscriptAppended{}, err
	}
	return out, nil
}

func (o *ops) LoadTranscript(ctx context.Context, f store.Fence, r store.TranscriptPageRequest) (store.TranscriptPage, error) {
	if err := checkSubpath(r.Subpath); err != nil {
		return store.TranscriptPage{}, err
	}
	if r.Limit < 1 || r.Limit > store.MaxTranscriptPageEntries || r.MaxBytes < 1 || r.MaxBytes > store.MaxTranscriptPageBytes || r.AfterSeq < 0 {
		return store.TranscriptPage{}, invalid("transcript page needs a limit of 1..%d and a byte budget of 1..%d",
			store.MaxTranscriptPageEntries, store.MaxTranscriptPageBytes)
	}
	var page store.TranscriptPage
	err := o.fenced(ctx, f, func(tx *ops) error {
		// Plan the page from sizes first, so no entry past the budget is
		// read into memory.
		rows, err := tx.q.QueryContext(ctx, `SELECT seq, bytes FROM transcript_entries
			WHERE session_id = ? AND subpath = ? AND seq > ? ORDER BY seq LIMIT ?`,
			f.SessionID, r.Subpath, r.AfterSeq, r.Limit+1)
		if err != nil {
			return mapErr(err)
		}
		sizes, err := collect(rows, func(s scanner) ([2]int64, error) {
			var v [2]int64
			return v, mapErr(s.Scan(&v[0], &v[1]))
		})
		if err != nil {
			return err
		}
		n, total := 0, int64(0)
		for n < len(sizes) && n < r.Limit && (n == 0 || total+sizes[n][1] <= r.MaxBytes) {
			total += sizes[n][1]
			n++
		}
		page.Done = n == len(sizes)
		if n == 0 {
			return nil
		}
		rows, err = tx.q.QueryContext(ctx, `SELECT subpath, seq, uuid, entry, epoch FROM transcript_entries
			WHERE session_id = ? AND subpath = ? AND seq BETWEEN ? AND ? ORDER BY seq`,
			f.SessionID, r.Subpath, sizes[0][0], sizes[n-1][0])
		if err != nil {
			return mapErr(err)
		}
		page.Entries, err = collect(rows, func(s scanner) (store.TranscriptEntry, error) {
			var e store.TranscriptEntry
			err := s.Scan(&e.Subpath, &e.Seq, nullStr{&e.UUID}, &e.JSON, &e.Epoch)
			return e, mapErr(err)
		})
		return err
	})
	if err != nil {
		return store.TranscriptPage{}, err
	}
	return page, nil
}

func (o *ops) ListTranscriptSubpaths(ctx context.Context, f store.Fence) ([]store.TranscriptSubpath, error) {
	var out []store.TranscriptSubpath
	err := o.fenced(ctx, f, func(tx *ops) error {
		rows, err := tx.q.QueryContext(ctx, `SELECT subpath, last_seq, bytes FROM transcript_subpaths
			WHERE session_id = ? AND last_seq > 0 ORDER BY subpath`, f.SessionID)
		if err != nil {
			return mapErr(err)
		}
		out, err = collect(rows, func(s scanner) (store.TranscriptSubpath, error) {
			var x store.TranscriptSubpath
			return x, mapErr(s.Scan(&x.Subpath, &x.LastSeq, &x.Bytes))
		})
		return err
	})
	return out, err
}

func (o *ops) HasTranscriptUUID(ctx context.Context, f store.Fence, uuid string) (bool, error) {
	if err := store.CheckID("transcript uuid", uuid); err != nil {
		return false, err
	}
	var found bool
	err := o.fenced(ctx, f, func(tx *ops) error {
		return mapErr(tx.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM transcript_entries WHERE session_id = ? AND uuid = ?)`,
			f.SessionID, uuid).Scan(&found))
	})
	return found, err
}
