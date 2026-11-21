package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	circusv1 "github.com/EricWvi/circus/gen/go/circus/v1"
	"golang.org/x/sys/unix"
)

type removeResult = circusv1.RemoveWorkspaceResult

// Remove is the remove_workspace tool. It deletes only a real directory
// directly inside <home>/circus/projects, never following a symlink, and
// stops at the call's deadline.
type Remove struct {
	opts Options
	// afterRead, for tests, runs after the walk reads each batch of names.
	afterRead func(dir string)
	// beforeRound, for tests, runs before each round over a directory.
	beforeRound func(dir string)
}

// NewRemove constructs the remove_workspace tool; it uses Home, Logger.
func NewRemove(opts Options) *Remove {
	opts.defaults()
	return &Remove{opts: opts}
}

// Name returns the device call tool name.
func (*Remove) Name() string { return "remove_workspace" }

// Run removes the workspace; see RemoveWorkspaceRequest.
func (r *Remove) Run(ctx context.Context, call *circusv1.DeviceCall) (*circusv1.DeviceResult, error) {
	req := call.GetRemoveWorkspace()
	if req == nil {
		return nil, errors.New("remove_workspace request is required")
	}
	result := r.run(ctx, req)
	r.opts.Logger.Info("workspace removal", "session_id", call.SessionId, "call_id", call.CallId, "path", result.Path, "outcome", result.Outcome.String(), "error", result.Error.String())
	var text string
	switch result.Outcome {
	case circusv1.RemoveWorkspaceResult_OUTCOME_REMOVED:
		text = "Removed " + result.Path
	case circusv1.RemoveWorkspaceResult_OUTCOME_ABSENT:
		text = "Nothing to remove at " + result.Path
	default:
		where := result.Path
		if where == "" {
			where = "the workspace"
		}
		text = fmt.Sprintf("Couldn't remove %s: %s", where, errorText(result.Error))
	}
	return &circusv1.DeviceResult{
		Output: &circusv1.ToolOutput{IsError: result.Outcome == circusv1.RemoveWorkspaceResult_OUTCOME_FAILED, Content: []*circusv1.ContentBlock{{Block: &circusv1.ContentBlock_Text{Text: text}}}},
		Detail: &circusv1.DeviceResult_RemoveWorkspace{RemoveWorkspace: result},
	}, nil
}

func removeFailed(path string, code workspaceError, detail string) *removeResult {
	return &removeResult{Outcome: circusv1.RemoveWorkspaceResult_OUTCOME_FAILED, Path: path, Error: code, ErrorDetail: clipDetail(detail)}
}

func absent(path string) *removeResult {
	return &removeResult{Outcome: circusv1.RemoveWorkspaceResult_OUTCOME_ABSENT, Path: path}
}

const refusedDetail = "only a directory directly inside ~/circus/projects can be removed"

func (r *Remove) run(ctx context.Context, req *circusv1.RemoveWorkspaceRequest) *removeResult {
	if runtime.GOOS == "windows" {
		return removeFailed("", errUnsupportedOS, "")
	}
	path, home, code := resolvePath(req.Path, r.opts.lookupHome)
	if code != 0 {
		return removeFailed("", code, "")
	}
	if home == "" {
		return removeFailed(path, errHomeUnknown, "")
	}
	// The lexical path must name one element directly below the projects
	// directory: no "..", no deeper or shallower path, no hidden name (the
	// lock and partial clones are hidden).
	name := filepath.Base(path)
	if filepath.Dir(path) != filepath.Join(home, "circus", "projects") || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return removeFailed(path, errRefused, refusedDetail)
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return r.fsFailure(path, "resolve home", err)
	}
	// Walk down with O_NOFOLLOW from the real home, so neither "circus"
	// nor "projects" can be a symlink to somewhere else.
	homeFd, err := unix.Open(realHome, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return r.fsFailure(path, "open home", err)
	}
	defer unix.Close(homeFd)
	circusFd, err := unix.Openat(homeFd, "circus", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return r.fsFailure(path, "open ~/circus", err)
	}
	defer unix.Close(circusFd)
	projectsFd, err := unix.Openat(circusFd, "projects", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return r.fsFailure(path, "open ~/circus/projects", err)
	}
	projects := os.NewFile(uintptr(projectsFd), filepath.Join(realHome, "circus", "projects"))
	defer projects.Close()
	lock, err := lockIn(projects, name)
	if errors.Is(err, errBusy) {
		return removeFailed(path, errBusyCode, "a preparation of this workspace is in progress")
	}
	if err != nil {
		return r.fsFailure(path, "lock", err)
	}
	defer lock.release()
	// Check under the lock, so a preparation cannot be between its
	// inspection and its rename.
	if result := checkTarget(path, projectsFd, name); result != nil {
		return result
	}
	// Refuse before deleting anything when a mount is known below the
	// workspace. The walk never crosses one anyway (the workspace itself
	// being a mount point included); this only avoids a partial removal.
	// Without a usable openat2 nothing is deleted, not even leftovers.
	if !mountCheckAvailable() {
		return removeFailed(path, errRefused, fmt.Sprintf("%v; nothing was removed", errNoMountCheck))
	}
	if point := mountBelow(filepath.Join(projects.Name(), name)); point != "" {
		rel := strings.TrimPrefix(point, projects.Name()+"/")
		return removeFailed(path, errRefused, fmt.Sprintf("%s is a mount point; nothing was removed", rel))
	}
	if err := removeLeftovers(ctx, projects, name); err != nil && ctx.Err() == nil {
		return r.fsFailure(path, "remove leftovers", err)
	}
	removal := newRemoval(ctx, true)
	removal.afterRead, removal.beforeRound = r.afterRead, r.beforeRound
	err = removal.removeDir(projectsFd, name, 0, true)
	remains := fmt.Sprintf("removed %d entries before that; the rest remains in %s", removal.removed, path)
	switch {
	case err == nil:
		return &removeResult{Outcome: circusv1.RemoveWorkspaceResult_OUTCOME_REMOVED, Path: path}
	case errors.Is(err, errNotRemovable):
		return removeFailed(path, errRefused, refusedDetail)
	case errors.Is(err, errNoMountCheck), errors.Is(err, errMountPoint) && removal.removed == 0:
		return removeFailed(path, errRefused, fmt.Sprintf("%v; nothing was removed", err))
	case ctx.Err() != nil:
		why := "the call's time ran out"
		if errors.Is(ctx.Err(), context.Canceled) {
			why = "the call was cancelled"
		}
		return removeFailed(path, errIO, fmt.Sprintf("stopped because %s after removing %d entries; the rest remains in %s", why, removal.removed, path))
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return removeFailed(path, errPermissionDenied, fmt.Sprintf("%v (%s)", err, remains))
	}
	r.opts.Logger.Warn("workspace removal failed", "path", path, "error", err)
	return removeFailed(path, errIO, fmt.Sprintf("%v (%s)", err, remains))
}

// checkTarget refuses anything at name but a real directory, and reports
// ABSENT for nothing. Mount points are refused by the walk, which checks
// the descriptor it reads.
func checkTarget(path string, projectsFd int, name string) *removeResult {
	var st unix.Stat_t
	err := unix.Fstatat(projectsFd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case errors.Is(err, unix.ENOENT):
		return absent(path)
	case errors.Is(err, unix.EACCES):
		return removeFailed(path, errPermissionDenied, "")
	case err != nil:
		return removeFailed(path, errIO, err.Error())
	case st.Mode&unix.S_IFMT != unix.S_IFDIR:
		return removeFailed(path, errRefused, "the path is a symlink or not a directory")
	}
	return nil
}

func (r *Remove) fsFailure(path, step string, err error) *removeResult {
	switch {
	case errors.Is(err, unix.ENOENT):
		return absent(path)
	case errors.Is(err, unix.ELOOP), errors.Is(err, unix.ENOTDIR):
		return removeFailed(path, errRefused, refusedDetail)
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.EPERM):
		return removeFailed(path, errPermissionDenied, fmt.Sprintf("%s: %v", step, err))
	}
	r.opts.Logger.Warn("workspace removal failed", "step", step, "path", path, "error", err)
	return removeFailed(path, errIO, fmt.Sprintf("%s: %v", step, err))
}
