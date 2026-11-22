package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/EricWvi/circus/internal/store"
)

// Projects.

// projectQuery reads projects with their locations, one row per location
// (one row with NULL location columns for a project without locations), in
// one statement (one snapshot).
const projectQuery = `SELECT p.id, p.user_id, p.name, p.repo_url, p.last_used_location, p.created_at, p.updated_at, p.default_provider_id,
	l.device_id, d.name, l.path, d.deleted_at IS NOT NULL, ` + workspaceCols + `
	FROM projects p LEFT JOIN project_locations l ON l.project_id = p.id LEFT JOIN devices d ON d.id = l.device_id
	LEFT JOIN workspaces w ON l.path = '' AND w.project_id = p.id AND w.device_id = l.device_id
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
		var workspace workspaceScan
		dest := []any{&p.ID, &p.UserID, &p.Name, nullStr{&p.RepoURL}, &p.LastUsedLocation, &p.CreatedAt, &p.UpdatedAt, nullStr{&p.DefaultProviderID},
			&hasLocation, nullStr{&l.DeviceName}, nullStr{&l.Path}, &l.DeviceRevoked}
		if err := rows.Scan(append(dest, workspace.dest()...)...); err != nil {
			return nil, mapErr(err)
		}
		if n := len(out); n == 0 || out[n-1].ID != p.ID {
			out = append(out, p)
		}
		if hasLocation.Valid {
			l.DeviceID = hasLocation.String
			if workspace.ID != "" {
				l.Workspace = new(workspace.result())
			}
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

// checkProviderRef returns ErrUnknownProvider unless providerID is empty or
// names a provider of the user.
func (o *ops) checkProviderRef(ctx context.Context, userID, providerID string) error {
	if providerID == "" {
		return nil
	}
	n, err := o.count(ctx, `SELECT COUNT(*) FROM providers WHERE id = ? AND user_id = ?`, providerID, userID)
	if err != nil {
		return err
	}
	if n == 0 {
		return store.ErrUnknownProvider
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

func (o *ops) CreateProject(ctx context.Context, p store.Project) (store.Project, error) {
	if err := store.CheckProject(p); err != nil {
		return store.Project{}, err
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
		if err := tx.checkProviderRef(ctx, p.UserID, p.DefaultProviderID); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `INSERT INTO projects (id, user_id, name, name_key, repo_url, last_used_location, created_at, updated_at,
			default_provider_id) VALUES (?,?,?,?,?,?,?,?,?)`, p.ID, p.UserID, p.Name, key, nstr(p.RepoURL), p.LastUsedLocation,
			p.CreatedAt, p.UpdatedAt, nstr(p.DefaultProviderID)); err != nil {
			return err
		}
		if err := tx.insertLocations(ctx, p.ID, p.Locations, p.UserID); err != nil {
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

func (o *ops) UpdateProject(ctx context.Context, userID, projectID string, patch store.ProjectPatch) (store.Project, error) {
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
	if patch.LastUsedLocation != nil && *patch.LastUsedLocation < 0 {
		return store.Project{}, store.ErrLocationIndex
	}
	if patch.RepoURL != nil {
		if err := store.CheckRepoURLSize(*patch.RepoURL); err != nil {
			return store.Project{}, err
		}
	}
	if patch.DefaultProviderID != nil {
		if err := store.CheckOptionalID("default provider id", *patch.DefaultProviderID); err != nil {
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
		edits := patch.Name != nil || patch.RepoURL != nil || patch.Locations != nil || patch.DefaultProviderID != nil
		if edits && patch.ExpectedUpdatedAt != nil && *patch.ExpectedUpdatedAt != cur.UpdatedAt {
			return store.ErrProjectStale
		}
		if !edits && patch.LastUsedLocation == nil {
			out = cur
			return nil
		}
		changed := (patch.Name != nil && *patch.Name != cur.Name) ||
			(patch.RepoURL != nil && *patch.RepoURL != cur.RepoURL) ||
			(patch.Locations != nil && !sameLocations(cur.Locations, patch.Locations)) ||
			(patch.DefaultProviderID != nil && *patch.DefaultProviderID != cur.DefaultProviderID)
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
		if patch.DefaultProviderID != nil {
			if err := tx.checkProviderRef(ctx, userID, *patch.DefaultProviderID); err != nil {
				return err
			}
			if _, err := tx.exec(ctx, `UPDATE projects SET default_provider_id = ? WHERE id = ?`, nstr(*patch.DefaultProviderID), projectID); err != nil {
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
			if err := tx.insertLocations(ctx, projectID, patch.Locations, userID); err != nil {
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
	deletable := func(ctx context.Context, tx *ops) ([]string, error) {
		if _, err := tx.GetProject(ctx, userID, projectID); err != nil {
			return nil, err
		}
		var hasSessions bool
		if err := tx.q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM sessions WHERE project_id = ?)`, projectID).Scan(&hasSessions); err != nil {
			return nil, mapErr(err)
		}
		if hasSessions {
			return nil, store.ErrHasSessions
		}
		return []string{projectID}, nil
	}
	return o.deleteWithLedgers(ctx, deletable, func(ctx context.Context, tx *ops) error {
		n, err := tx.exec(ctx, `DELETE FROM projects WHERE id = ? AND user_id = ?`, projectID, userID)
		if err == nil && n == 0 {
			err = store.ErrNotFound
		}
		return err
	})
}

// errLedgerRowsBack reports ledger rows written for a project between the
// purge and the final delete transaction of its deletion.
var errLedgerRowsBack = errors.New("ledger rows were written during the deletion")

// Attempts of a deletion whose ledger rows keep coming back, as from a
// commit racing it, before it gives up with ErrBusy.
const ledgerDeleteAttempts = 3

// deleteWithLedgers deletes what final deletes, after the ledger rows of
// the projects that deletable returns, so the final transaction cascades
// over their head rows only. deletable checks the deletion's
// preconditions in every transaction that changes something.
//
// The first transaction hides each index: the head is cleared and readers
// move to a new, empty generation. The rows are then deleted in bounded
// transactions. A failure or crash after the hiding leaves a project that
// still exists with an empty index and invisible rows; the ledger rebuilds
// such an index on the next read, and a retried deletion finishes the
// rows. The final transaction refuses to cascade rows written meanwhile,
// and the deletion starts over instead. These steps continue when ctx
// ends, within the ledger cleanup budget, so a caller that goes away does
// not leave the index hidden.
func (o *ops) deleteWithLedgers(ctx context.Context, deletable func(context.Context, *ops) ([]string, error), final func(context.Context, *ops) error) error {
	ctx, cancel := ledgerCleanupContext(ctx)
	defer cancel()
	for attempt := 1; ; attempt++ {
		var indexed []string
		err := o.unit(ctx, func(tx *ops) error {
			indexed = indexed[:0]
			projects, err := deletable(ctx, tx)
			if err != nil {
				return err
			}
			for _, id := range projects {
				n, err := tx.exec(ctx, `UPDATE ledger_heads SET head = '', next_position = 0, generation = reserved + 1,
					reserved = reserved + 1 WHERE project_id = ?`, id)
				if err != nil {
					return err
				}
				if n > 0 {
					indexed = append(indexed, id)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		for _, id := range indexed {
			if err := o.purgeLedgerIndex(ctx, id); err != nil {
				return err
			}
		}
		err = o.unit(ctx, func(tx *ops) error {
			projects, err := deletable(ctx, tx)
			if err != nil {
				return err
			}
			for _, id := range projects {
				var rows bool
				if err := tx.q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM ledger_versions WHERE project_id = ?1)
					OR EXISTS(SELECT 1 FROM ledger_entries WHERE project_id = ?1)`, id).Scan(&rows); err != nil {
					return mapErr(err)
				}
				if rows {
					return errLedgerRowsBack
				}
			}
			return final(ctx, tx)
		})
		if !errors.Is(err, errLedgerRowsBack) {
			return err
		}
		if attempt == ledgerDeleteAttempts {
			return fmt.Errorf("%w: %w", store.ErrBusy, err)
		}
	}
}
