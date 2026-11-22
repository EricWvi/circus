package ledger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/EricWvi/circus/internal/store"
	"golang.org/x/sys/unix"
)

// Reconcile repairs stale indexes and removes repositories whose projects
// have been deleted. The server calls this before serving requests.
func (m *Manager) Reconcile(ctx context.Context) error {
	unlockRoot, lockErr := m.lockRoot(unix.LOCK_EX)
	exclusive := lockErr == nil
	if exclusive {
		defer unlockRoot()
	} else {
		m.logger.WarnContext(ctx, "ledger startup cleanup deferred", "error", lockErr)
	}
	if err := scanDir(ctx, m.root, func(name string) error {
		id, ok := strings.CutSuffix(name, repoSuffix)
		if !ok {
			if exclusive && (strings.HasPrefix(name, initPrefix) || strings.HasPrefix(name, replayPrefix)) {
				if err := os.RemoveAll(filepath.Join(m.root, name)); err != nil {
					m.logger.WarnContext(ctx, "ledger startup cleanup failed", "directory", name, "error", err)
				} else {
					m.logger.InfoContext(ctx, "removed abandoned ledger temporary data", "directory", name)
				}
			}
			return nil
		}
		if !ValidProjectID(id) {
			m.logger.ErrorContext(ctx, "ledger repository skipped at startup", "repository", name, "error", "invalid project id")
			return nil
		}
		if err := m.reconcileProject(ctx, id, exclusive); err != nil {
			m.logger.ErrorContext(ctx, "ledger repository skipped at startup", "project_id", id, "error", err)
			if !errors.Is(err, store.ErrBusy) {
				m.setProjectError(id, err)
			}
		} else {
			m.setProjectError(id, nil)
		}
		return ctx.Err()
	}); err != nil {
		return fmt.Errorf("ledger: list repository directory: %w", err)
	}

	for after := ""; ; {
		ids, err := m.st.ListLedgerProjects(ctx, after)
		if err != nil {
			m.logger.ErrorContext(ctx, "ledger startup index scan failed", "error", err)
			break
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if err := m.clearMissingIndex(ctx, id, exclusive); err != nil {
				m.logger.ErrorContext(ctx, "ledger index skipped at startup", "project_id", id, "error", err)
			}
		}
		after = ids[len(ids)-1]
	}
	if exclusive {
		trash := filepath.Join(m.root, trashName)
		if _, err := os.Lstat(trash); err == nil {
			if err := os.RemoveAll(trash); err != nil {
				m.logger.WarnContext(ctx, "ledger trash cleanup failed", "error", err)
			} else {
				m.logger.InfoContext(ctx, "cleared ledger trash")
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			m.logger.WarnContext(ctx, "ledger trash inspection failed", "error", err)
		}
	}
	return ctx.Err()
}

func (m *Manager) clearMissingIndex(ctx context.Context, id string, exclusive bool) error {
	if !ValidProjectID(id) {
		return fmt.Errorf("invalid project id")
	}
	if _, err := os.Lstat(m.repoPath(id)); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !exclusive {
		// A different process may be preparing the initial repository.
		return store.ErrBusy
	}
	head, err := m.st.GetLedgerHead(ctx, id)
	if err != nil {
		return err
	}
	if err := m.st.ReplaceLedgerIndex(ctx, id, head, "", false, nil, nil); err != nil {
		return err
	}
	m.setProjectError(id, nil)
	m.logger.InfoContext(ctx, "cleared index for missing ledger repository", "project_id", id)
	return nil
}

func (m *Manager) reconcileProject(ctx context.Context, id string, exclusive bool) error {
	// Acquire the in-process lock before accessing the repository. Runtime
	// writes try, rather than wait for, the startup lock.
	release, err := m.acquire(ctx, id)
	if err != nil {
		return err
	}
	defer release()
	if !exclusive {
		// Like a writer, hold the root shared so that another process's
		// exclusive startup sweep cannot remove the main.lock taken below.
		unlockRoot, err := m.lockRoot(unix.LOCK_SH)
		if err != nil {
			return err
		}
		defer unlockRoot()
	}
	name, exists, err := m.st.LedgerProjectName(ctx, id)
	if err != nil {
		return err
	}
	if !exists {
		if !exclusive {
			return store.ErrBusy
		}
		if err := m.remove(id); err != nil {
			return err
		}
		m.logger.InfoContext(ctx, "removed orphan ledger repository", "project_id", id)
		return nil
	}
	r, err := m.open(m.repoPath(id))
	if err != nil || r == nil {
		return err
	}
	defer r.storage.Close()
	if exclusive {
		if err := os.Remove(r.mainLockPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		for dir, prefix := range map[string]string{r.dir: refPrefix, filepath.Join(r.dir, "objects"): objectTempPrefix} {
			if err := scanDir(ctx, dir, func(name string) error {
				if strings.HasPrefix(name, prefix) {
					return os.Remove(filepath.Join(dir, name))
				}
				return nil
			}); err != nil {
				return err
			}
		}
	} else {
		unlock, err := r.lockMain()
		if errors.Is(err, store.ErrBusy) {
			// Without the exclusive lock, the lock may belong to a live
			// writer in another process, so it cannot be removed here.
			m.logger.WarnContext(ctx, "ledger main.lock kept because another process uses the ledger directory; commits to this project fail until the lock is released or the server starts alone",
				"project_id", id, "project", name, "lock", r.mainLockPath())
		}
		if err != nil {
			return err
		}
		defer unlock()
	}

	head, err := r.head()
	if err != nil {
		return err
	}
	if !head.IsZero() {
		if _, err := r.commit(head); err != nil {
			return fmt.Errorf("read main commit %s: %w", head, err)
		}
	}
	indexed, err := m.st.GetLedgerHead(ctx, id)
	if err != nil || indexed == commitID(head) {
		return err
	}
	if err := m.rebuild(ctx, r, id, indexed, head); err != nil {
		return err
	}
	m.logger.InfoContext(ctx, "rebuilt ledger index", "project_id", id, "head", commitID(head))
	return nil
}

func scanDir(ctx context.Context, dir string, visit func(string) error) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	for {
		names, err := f.Readdirnames(128)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := visit(name); err != nil {
				return err
			}
		}
	}
}
