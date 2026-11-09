package daemon

import "github.com/EricWvi/circus/internal/daemon/fstools"

var (
	_ Tool = (*fstools.Read)(nil)
	_ Tool = (*fstools.Write)(nil)
	_ Tool = (*fstools.Edit)(nil)
)
