package sqlite

import (
	"context"
	"database/sql"

	"github.com/EricWvi/circus/internal/store"
)

// Projects.

// projectQuery reads projects with their locations, one row per location
// (one row with NULL location columns for a project without locations), in
// one statement (one snapshot).
const projectQuery = `SELECT p.id, p.user_id, p.name, p.repo_url, p.last_used_location, p.created_at, p.updated_at,
	l.device_id, d.name, l.path, d.deleted_at IS NOT NULL
	FROM projects p LEFT JOIN project_locations l ON l.project_id = p.id LEFT JOIN devices d ON d.id = l.device_id
	WHERE p.user_id = ?`

func (o *ops) queryProjects(ctx context.Context, where string, args ...any) ([]store.Project, error) {
	rows, err := o.q.QueryContext(ctx, projectQuery+where+` ORDER BY p.name_key, p.id, l.position`, args...)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()
	var out []store.Project
	for rows.Next() {
		var p store.Project
		var l store.ProjectLocation
		var hasLocation sql.NullString
		if err := rows.Scan(&p.ID, &p.UserID, &p.Name, nullStr{&p.RepoURL}, &p.LastUsedLocation, &p.CreatedAt, &p.UpdatedAt,
			&hasLocation, nullStr{&l.DeviceName}, nullStr{&l.Path}, &l.DeviceRevoked); err != nil {
			return nil, mapErr(err)
		}
		if n := len(out); n == 0 || out[n-1].ID != p.ID {
			out = append(out, p)
		}
		if hasLocation.Valid {
			l.DeviceID = hasLocation.String
			last := &out[len(out)-1]
			last.Locations = append(last.Locations, l)
		}
	}
	return out, mapErr(rows.Err())
}

func (o *ops) GetProject(ctx context.Context, userID, projectID string) (store.Project, error) {
	if err := store.CheckID("user id", userID); err != nil {
		return store.Project{}, err
	}
	if err := store.CheckID("project id", projectID); err != nil {
		return store.Project{}, err
	}
	ps, err := o.queryProjects(ctx, ` AND p.id = ?`, userID, projectID)
	if err != nil {
		return store.Project{}, err
	}
	if len(ps) == 0 {
		return store.Project{}, store.ErrNotFound
	}
	return ps[0], nil
}

func (o *ops) ListProjects(ctx context.Context, userID string) ([]store.Project, error) {
	if err := store.CheckID("user id", userID); err != nil {
		return nil, err
	}
	return o.queryProjects(ctx, ``, userID)
}

// nameTaken reports whether another project of the user (not exceptID) has
// a name with this key.
func (o *ops) nameTaken(ctx context.Context, userID, key, exceptID string) error {
	n, err := o.count(ctx, `SELECT COUNT(*) FROM projects WHERE user_id = ? AND name_key = ? AND id <> ?`, userID, key, exceptID)
	if err != nil {
		return err
	}
	if n > 0 {
		return store.ErrExists
	}
	return nil
}

// checkProjectBytes returns ErrProjectBytes when the user's projects hold
// more than store.MaxProjectBytesPerUser bytes of names, repository URLs
// and paths. Writes call it last, inside their unit, so a refusal rolls
// them back.
func (o *ops) checkProjectBytes(ctx context.Context, userID string) error {
	var n int64
	err := o.q.QueryRow(ctx, `SELECT
		(SELECT COALESCE(SUM(length(CAST(name AS BLOB)) + COALESCE(length(CAST(repo_url AS BLOB)), 0)), 0) FROM projects WHERE user_id = ?1) +
		(SELECT COALESCE(SUM(length(CAST(l.path AS BLOB))), 0) FROM project_locations l JOIN projects p ON p.id = l.project_id WHERE p.user_id = ?1)`,
		userID).Scan(&n)
	if err != nil {
		return mapErr(err)
	}
	if n > store.MaxProjectBytesPerUser {
		return store.ErrProjectBytes
	}
	return nil
}

// insertLocations writes locs at positions 0.. after checking that each
// device is a live device of owner.
func (o *ops) insertLocations(ctx context.Context, projectID string, locs []store.ProjectLocation, owner string) error {
	for i, l := range locs {
		n, err := o.count(ctx, `SELECT COUNT(*) FROM devices WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, l.DeviceID, owner)
		if err != nil {
			return err
		}
		if n == 0 {
			return &store.LocationDeviceError{Index: i}
		}
		if _, err := o.exec(ctx, `INSERT INTO project_locations (project_id, position, device_id, path) VALUES (?,?,?,?)`,
			projectID, i, l.DeviceID, l.Path); err != nil {
			return err
		}
	}
	return nil
}

func (o *ops) CreateProject(ctx context.Context, p store.Project, owner string) (store.Project, error) {
	if err := store.CheckProject(p); err != nil {
		return store.Project{}, err
	}
	if len(p.Locations) > 0 {
		if err := store.CheckID("device owner id", owner); err != nil {
			return store.Project{}, err
		}
	}
	if !validIndex(p.LastUsedLocation, len(p.Locations)) {
		return store.Project{}, store.ErrLocationIndex
	}
	var out store.Project
	err := o.unit(ctx, func(tx *ops) error {
		n, err := tx.count(ctx, `SELECT COUNT(*) FROM projects WHERE user_id = ?`, p.UserID)
		if err != nil {
			return err
		}
		if n >= store.MaxProjectsPerUser {
			return store.ErrLimit
		}
		key := store.ProjectNameKey(p.Name)
		if err := tx.nameTaken(ctx, p.UserID, key, p.ID); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `INSERT INTO projects (id, user_id, name, name_key, repo_url, last_used_location, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?)`, p.ID, p.UserID, p.Name, key, nstr(p.RepoURL), p.LastUsedLocation, p.CreatedAt, p.UpdatedAt); err != nil {
			return err
		}
		if err := tx.insertLocations(ctx, p.ID, p.Locations, owner); err != nil {
			return err
		}
		if err := tx.checkProjectBytes(ctx, p.UserID); err != nil {
			return err
		}
		out, err = tx.GetProject(ctx, p.UserID, p.ID)
		return err
	})
	if err != nil {
		return store.Project{}, err
	}
	return out, nil
}

func (o *ops) UpdateProject(ctx context.Context, userID, projectID string, patch store.ProjectPatch, owner string) (store.Project, error) {
	if err := store.CheckID("user id", userID); err != nil {
		return store.Project{}, err
	}
	if err := store.CheckID("project id", projectID); err != nil {
		return store.Project{}, err
	}
	if patch.Name != nil {
		if err := store.CheckProjectName(*patch.Name); err != nil {
			return store.Project{}, err
		}
	}
	if patch.Locations != nil {
		if err := store.CheckProjectLocations(patch.Locations); err != nil {
			return store.Project{}, err
		}
	}
	if len(patch.Locations) > 0 {
		if err := store.CheckID("device owner id", owner); err != nil {
			return store.Project{}, err
		}
	}
	if patch.LastUsedLocation != nil && *patch.LastUsedLocation < 0 {
		return store.Project{}, store.ErrLocationIndex
	}
	if patch.RepoURL != nil {
		if err := store.CheckRepoURLSize(*patch.RepoURL); err != nil {
			return store.Project{}, err
		}
	}
	if patch.UpdatedAt <= 0 {
		return store.Project{}, invalid("project update needs a time")
	}
	var out store.Project
	err := o.unit(ctx, func(tx *ops) error {
		cur, err := tx.GetProject(ctx, userID, projectID)
		if err != nil {
			return err
		}
		edits := patch.Name != nil || patch.RepoURL != nil || patch.Locations != nil
		if edits && patch.ExpectedUpdatedAt != nil && *patch.ExpectedUpdatedAt != cur.UpdatedAt {
			return store.ErrProjectStale
		}
		if !edits && patch.LastUsedLocation == nil {
			out = cur
			return nil
		}
		changed := (patch.Name != nil && *patch.Name != cur.Name) ||
			(patch.RepoURL != nil && *patch.RepoURL != cur.RepoURL) ||
			(patch.Locations != nil && !sameLocations(cur.Locations, patch.Locations))
		if patch.Name != nil {
			key := store.ProjectNameKey(*patch.Name)
			if err := tx.nameTaken(ctx, userID, key, projectID); err != nil {
				return err
			}
			if _, err := tx.exec(ctx, `UPDATE projects SET name = ?, name_key = ? WHERE id = ?`, *patch.Name, key, projectID); err != nil {
				return err
			}
		}
		if patch.RepoURL != nil {
			if _, err := tx.exec(ctx, `UPDATE projects SET repo_url = ? WHERE id = ?`, nstr(*patch.RepoURL), projectID); err != nil {
				return err
			}
		}
		last, count := cur.LastUsedLocation, len(cur.Locations)
		if patch.Locations != nil {
			last = followLocation(cur.Locations, last, patch.Locations)
			count = len(patch.Locations)
		}
		if patch.LastUsedLocation != nil {
			if !validIndex(*patch.LastUsedLocation, count) {
				return store.ErrLocationIndex
			}
			last = *patch.LastUsedLocation
		}
		if patch.Locations != nil {
			if _, err := tx.exec(ctx, `DELETE FROM project_locations WHERE project_id = ?`, projectID); err != nil {
				return err
			}
			if err := tx.insertLocations(ctx, projectID, patch.Locations, owner); err != nil {
				return err
			}
		}
		updatedAt := cur.UpdatedAt
		if changed {
			updatedAt = max(patch.UpdatedAt, cur.UpdatedAt+1)
		}
		if _, err := tx.exec(ctx, `UPDATE projects SET last_used_location = ?, updated_at = ? WHERE id = ?`,
			last, updatedAt, projectID); err != nil {
			return err
		}
		if edits {
			if err := tx.checkProjectBytes(ctx, userID); err != nil {
				return err
			}
		}
		out, err = tx.GetProject(ctx, userID, projectID)
		return err
	})
	if err != nil {
		return store.Project{}, err
	}
	return out, nil
}

// sameLocations reports whether two location lists bind the same devices
// and paths in the same order.
func sameLocations(a, b []store.ProjectLocation) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].DeviceID != b[i].DeviceID || a[i].Path != b[i].Path {
			return false
		}
	}
	return true
}

// validIndex reports whether i is a last used location for n locations:
// an index into them, or 0 when there are none.
func validIndex(i, n int) bool { return i >= 0 && (i < n || i == 0) }

// followLocation returns the index in next of old[last] (same device and
// path), or last clamped into next's range when next does not have it (0
// when next is empty).
func followLocation(old []store.ProjectLocation, last int, next []store.ProjectLocation) int {
	if last >= 0 && last < len(old) {
		for i, l := range next {
			if l.DeviceID == old[last].DeviceID && l.Path == old[last].Path {
				return i
			}
		}
	}
	return max(0, min(last, len(next)-1))
}

func (o *ops) DeleteProject(ctx context.Context, userID, projectID string, now int64) error {
	if err := store.CheckID("user id", userID); err != nil {
		return err
	}
	if err := store.CheckID("project id", projectID); err != nil {
		return err
	}
	if now == 0 {
		return invalid("DeleteProject needs a time")
	}
	return o.unit(ctx, func(tx *ops) error {
		if _, err := tx.GetProject(ctx, userID, projectID); err != nil {
			return err
		}
		var hasSessions bool
		if err := tx.q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE project_id = ?)`, projectID).Scan(&hasSessions); err != nil {
			return mapErr(err)
		}
		if hasSessions {
			return store.ErrHasSessions
		}
		n, err := tx.exec(ctx, `DELETE FROM projects WHERE id = ? AND user_id = ?`, projectID, userID)
		if err == nil && n == 0 {
			err = store.ErrNotFound
		}
		return err
	})
}
