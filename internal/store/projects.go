package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Project bounds.
const (
	// MaxProjectNameRunes bounds a project name in Unicode code points.
	MaxProjectNameRunes = 64
	// MaxProjectLocations bounds a project's locations.
	MaxProjectLocations = 16
	// MaxProjectsPerUser bounds the projects of one user (ErrLimit).
	MaxProjectsPerUser = 1000
	// MaxRepoURLBytes bounds a project's repository URL.
	MaxRepoURLBytes = 2048
	// MaxProjectBytesPerUser bounds the bytes of the names, repository URLs
	// and location paths of all of one user's projects (ErrProjectBytes),
	// so a full ListProjects stays bounded.
	MaxProjectBytesPerUser = 4 << 20
)

// ErrLocationIndex reports a LastUsedLocation that does not index the
// project's locations, or is not 0 for a project without locations.
// Nothing was written.
var ErrLocationIndex = errors.New("store: location index out of range")

// ErrProjectStale reports an UpdateProject whose ExpectedUpdatedAt differs
// from the project's UpdatedAt: someone changed the project since the caller
// read it. Nothing was written.
var ErrProjectStale = errors.New("store: project changed since it was read")

// ErrHasSessions reports a project, or a user owning projects, with any
// session. Nothing was written; the service maps it to FAILED_PRECONDITION.
var ErrHasSessions = errors.New("store: project has active sessions")

// ErrProjectBytes reports a project write that would take the user's
// projects past MaxProjectBytesPerUser. It matches ErrLimit. Nothing was
// written.
var ErrProjectBytes = fmt.Errorf("%w: project storage budget of %d bytes", ErrLimit, MaxProjectBytesPerUser)

// LocationDeviceError reports a project location whose device is not a live
// device of the device owner the write named. Index is the location's
// position in the written list. It matches ErrNotFound.
type LocationDeviceError struct{ Index int }

func (e *LocationDeviceError) Error() string {
	return fmt.Sprintf("store: device of location %d is not a live device of the owner", e.Index)
}

// Is makes errors.Is(err, ErrNotFound) match a *LocationDeviceError.
func (e *LocationDeviceError) Is(target error) bool { return target == ErrNotFound }

// Project is a user's named set of locations, directories on devices that
// sessions of the project start in, and an optional repository URL. A
// project may have no locations.
type Project struct {
	ID     string
	UserID string
	// Name is unique per user ignoring case (ProjectNameKey).
	Name      string
	Locations []ProjectLocation
	// LastUsedLocation indexes Locations: the default for new sessions.
	// It is 0 when there are no locations.
	LastUsedLocation int
	// RepoURL is the project's git remote; empty means none. Validating its
	// form is the caller's job; the store bounds it to MaxRepoURLBytes.
	RepoURL   string
	CreatedAt int64
	UpdatedAt int64
}

// ProjectLocation is one directory of a project. The store references the
// device by id, so a location keeps naming the machine it was bound to: a
// revoked device's name may later be taken by another machine.
type ProjectLocation struct {
	DeviceID string
	// DeviceName is the device's name, filled by reads (also for a device
	// deleted since); writes ignore it.
	DeviceName string
	// DeviceRevoked, filled by reads, reports that the device has been
	// deleted (revoked) since the location was saved. The location keeps
	// it; whoever starts work there must check the device again.
	DeviceRevoked bool
	// Path is stored as given; normalizing it for the device's OS is the
	// caller's job. At most MaxPathBytes, unique per device in a project.
	// Empty means the device's default directory for the project, so a
	// project has at most one such default location per device.
	Path string
}

// ProjectPatch is an UpdateProject change. Nil fields leave the project's
// value unchanged.
type ProjectPatch struct {
	Name *string
	// Locations, when non-nil, replaces every location. LastUsedLocation
	// then follows the old default location (same device id and path) to
	// its new index, or is clamped into the new range if it is gone.
	Locations []ProjectLocation
	// LastUsedLocation, when set, wins over that rule and must index the
	// locations after the update (or be 0 when there are none), else
	// ErrLocationIndex.
	LastUsedLocation *int
	// RepoURL, when set, replaces the repository URL; "" clears it.
	RepoURL *string
	// ExpectedUpdatedAt, when set, must equal the project's UpdatedAt, else
	// ErrProjectStale: an edit applies only to the version the caller read.
	// It is checked only when Name, RepoURL or Locations is set; a patch
	// that sets only LastUsedLocation (or nothing) is never stale.
	ExpectedUpdatedAt *int64
	// UpdatedAt is the time of the change (required). The stored UpdatedAt
	// becomes max(UpdatedAt, previous UpdatedAt + 1), so it strictly
	// increases, but only when the name, repository URL or locations
	// actually change: a field set to its current value is no change, and
	// LastUsedLocation alone never moves UpdatedAt.
	UpdatedAt int64
}

// ProjectOps are the project operations. Projects belong to one user
// (UserID); every read and write is scoped to it, and a project of another
// user is ErrNotFound, as if absent. Writes check device ownership against
// deviceOwnerID, the user whose devices the project's user may bind (see
// the control plane's mapping); it may differ from the project's user, and
// may be empty when the write binds no device.
type ProjectOps interface {
	// CreateProject inserts p and returns it as stored. p must have 0 to
	// MaxProjectLocations distinct (device, path) locations and a
	// LastUsedLocation indexing them, 0 if there are none (else
	// ErrLocationIndex). Each device must be a live device of
	// deviceOwnerID, else *LocationDeviceError.
	// ErrExists if the id is taken or another project of the user has a
	// name with the same ProjectNameKey; ErrLimit past MaxProjectsPerUser,
	// ErrProjectBytes past MaxProjectBytesPerUser; ErrNotFound if the user
	// does not exist.
	CreateProject(ctx context.Context, p Project, deviceOwnerID string) (Project, error)
	// GetProject returns the user's project, or ErrNotFound.
	GetProject(ctx context.Context, userID, projectID string) (Project, error)
	// ListProjects returns the user's projects ordered by ProjectNameKey,
	// then id.
	ListProjects(ctx context.Context, userID string) ([]Project, error)
	// UpdateProject applies patch to the user's project in one transaction
	// and returns the result. Replaced locations follow CreateProject's
	// rules; a name conflict is ErrExists, an unknown project ErrNotFound,
	// a stale ExpectedUpdatedAt ErrProjectStale, the byte budget
	// ErrProjectBytes. A patch that sets nothing returns the project
	// unchanged.
	UpdateProject(ctx context.Context, userID, projectID string, patch ProjectPatch, deviceOwnerID string) (Project, error)
	// DeleteProject deletes the user's empty project and locations in one
	// transaction. Any session, even deletable, is ErrHasSessions and nothing
	// is deleted. The service preflights all sessions, deletes them one by one
	// with chunked DeleteSession, then calls this method; a session created
	// meanwhile makes this final step refuse, so the client can retry.
	// ErrNotFound if absent or owned by another Web user. now is required
	// Unix milliseconds, retained for the caller's deletion timestamp.
	DeleteProject(ctx context.Context, userID, projectID string, now int64) error
}

// ProjectNameKey is the form project names are compared in for uniqueness:
// every code point replaced by one fixed member of its Unicode simple case
// folding orbit (the lower-case member if it has one, else the smallest).
// Two names have the same key exactly when strings.EqualFold reports them
// equal.
func ProjectNameKey(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		b.WriteRune(foldRepresentative(r))
	}
	return b.String()
}

func foldRepresentative(r rune) rune {
	best, bestLower := r, unicode.IsLower(r)
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		lower := unicode.IsLower(f)
		if (lower && !bestLower) || (lower == bestLower && f < best) {
			best, bestLower = f, lower
		}
	}
	return best
}

// CheckProject validates what every backend requires of a project before
// writing it: ids, a name of 1 to MaxProjectNameRunes code points, times,
// the repository URL's size and the locations (CheckProjectLocations).
func CheckProject(p Project) error {
	if err := CheckID("project id", p.ID); err != nil {
		return err
	}
	if err := CheckID("user id", p.UserID); err != nil {
		return err
	}
	if err := CheckProjectName(p.Name); err != nil {
		return err
	}
	if p.CreatedAt <= 0 || p.UpdatedAt < p.CreatedAt {
		return invalid("project needs creation and update times")
	}
	if err := CheckRepoURLSize(p.RepoURL); err != nil {
		return err
	}
	return CheckProjectLocations(p.Locations)
}

// CheckProjectName validates a stored project name: valid UTF-8 of 1 to
// MaxProjectNameRunes code points.
func CheckProjectName(name string) error {
	if name == "" || !utf8.ValidString(name) || utf8.RuneCountInString(name) > MaxProjectNameRunes {
		return invalid("project name must be 1 to %d code points of valid UTF-8", MaxProjectNameRunes)
	}
	return nil
}

// CheckRepoURLSize validates an optional repository URL of at most
// MaxRepoURLBytes of valid UTF-8.
func CheckRepoURLSize(url string) error {
	if len(url) > MaxRepoURLBytes || !utf8.ValidString(url) {
		return invalid("repository URL must be valid UTF-8 of at most %d bytes", MaxRepoURLBytes)
	}
	return nil
}

// CheckProjectLocations validates 0 to MaxProjectLocations locations with
// device ids, optional paths of at most MaxPathBytes, and no (device, path)
// twice, which also allows one default location (empty path) per device.
func CheckProjectLocations(locs []ProjectLocation) error {
	if len(locs) > MaxProjectLocations {
		return invalid("project has at most %d locations, got %d", MaxProjectLocations, len(locs))
	}
	seen := make(map[[2]string]bool, len(locs))
	for _, l := range locs {
		if err := CheckID("location device id", l.DeviceID); err != nil {
			return err
		}
		if err := CheckText("location path", l.Path, MaxPathBytes, true); err != nil {
			return err
		}
		k := [2]string{l.DeviceID, l.Path}
		if seen[k] {
			return invalid("location listed twice")
		}
		seen[k] = true
	}
	return nil
}
