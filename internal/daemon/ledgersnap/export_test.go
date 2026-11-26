package ledgersnap

import (
	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"github.com/EricWvi/circus/internal/ledger/snapshot"
)

// HoldSlots takes n of the service's call slots until release is called.
func HoldSlots(s *Service, n int) (release func()) {
	for range n {
		s.slots <- struct{}{}
	}
	return func() {
		for range n {
			<-s.slots
		}
	}
}

// SetAfterApply runs fn between a rebase's edits and its rebuild.
func SetAfterApply(s *Service, fn func()) { s.afterApply = fn }

// SetBeforeEdit runs fn after an edit's file was checked again and before
// the edit is applied.
func SetBeforeEdit(s *Service, fn func(path string)) { s.beforeEdit = fn }

// SetFoldsNames makes the service refuse names a folding file system
// would merge (as on macOS) or not, until the test ends.
func SetFoldsNames(t interface{ Cleanup(func()) }, folds bool) {
	old := foldsNames
	foldsNames = folds
	t.Cleanup(func() { foldsNames = old })
}

// OrderEdits orders edits as a rebase where names fold does.
func OrderEdits(edits []*circusv1.RebaseLedgerRequest_Edit) []*circusv1.RebaseLedgerRequest_Edit {
	return aliased(&snapshot.Manifest{}, &snapshot.Result{}, edits).order(edits)
}
