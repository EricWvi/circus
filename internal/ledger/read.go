package ledger

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/EricWvi/circus/internal/store"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// DecisionAt is a decision read from a specific main-line snapshot.
type DecisionAt struct {
	Decision
	Path        string
	Commit      string
	CommittedAt int64 // Unix milliseconds
}

// GetDecision reads the number at the specified first-parent main commit.
// An empty commit selects Git's current main head. A missing decision,
// empty ledger, or commit outside that line returns store.ErrNotFound.
func (m *Manager) GetDecision(ctx context.Context, projectID string, number uint32, commit string) (DecisionAt, error) {
	if number > store.MaxLedgerNumber || commit != "" && !store.IsLedgerCommitID(commit) {
		return DecisionAt{}, store.ErrInvalid
	}
	if !ValidProjectID(projectID) {
		return DecisionAt{}, fmt.Errorf("%w: invalid ledger project id", store.ErrInvalid)
	}
	if err := m.CheckProject(ctx, projectID); err != nil {
		return DecisionAt{}, err
	}
	release, err := m.acquire(ctx, projectID)
	if err != nil {
		return DecisionAt{}, err
	}
	defer release()
	r, err := m.open(m.repoPath(projectID))
	if err != nil {
		return DecisionAt{}, err
	}
	if r == nil {
		return DecisionAt{}, store.ErrNotFound
	}
	defer r.storage.Close()

	c, err := r.mainCommit(ctx, commit)
	if err != nil {
		return DecisionAt{}, err
	}
	var out DecisionAt
	found := errors.New("ledger: found decision")
	prefix := numberName(number) + "-"
	err = r.walkFiles(ctx, c.TreeHash, "", func(file fileObject) error {
		if !strings.HasPrefix(path.Base(file.path), prefix) || !strings.HasSuffix(file.path, ".md") {
			return nil
		}
		data, err := r.blob(file.hash, MaxDecisionFileBytes, nil)
		if err != nil {
			return err
		}
		d, err := Parse(data)
		if err != nil {
			return err
		}
		d.Dir = fileDir(file.path)
		var v validation
		v.checkDecision(file.path, d)
		if len(v.problems) != 0 {
			return &ValidationError{Problems: v.problems}
		}
		out = DecisionAt{Decision: d, Path: file.path, Commit: c.Hash.String(), CommittedAt: c.Committer.When.UnixMilli()}
		return found
	})
	if errors.Is(err, found) {
		return out, nil
	}
	if err != nil {
		return DecisionAt{}, err
	}
	return DecisionAt{}, store.ErrNotFound
}

func (r *repository) mainCommit(ctx context.Context, id string) (*object.Commit, error) {
	head, err := r.head()
	if err != nil {
		return nil, err
	}
	target := head
	if id != "" {
		target = plumbing.NewHash(id)
	}
	for hash := head; !hash.IsZero(); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c, err := r.commit(hash)
		if err != nil {
			return nil, err
		}
		if hash == target {
			return c, nil
		}
		if len(c.ParentHashes) == 0 {
			break
		}
		hash = c.ParentHashes[0]
	}
	return nil, store.ErrNotFound
}

// ListDecisionHistory reads indexed history newest first. The cursor is
// the last commit of the preceding page.
func (m *Manager) ListDecisionHistory(ctx context.Context, projectID string, number uint32, after string, limit int) ([]store.LedgerVersion, bool, error) {
	return m.st.ListLedgerHistory(ctx, projectID, number, after, limit)
}
