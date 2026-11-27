package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"

	"github.com/EricWvi/circus/internal/store"
)

// Providers.

// providerQuery reads providers with the user's default and their live
// lease count at ?1.
const providerQuery = `SELECT p.id, p.user_id, p.name, p.kind, p.base_url, p.scheme, p.fable_model, p.opus_model,
	p.sonnet_model, p.haiku_model, p.max_sessions, p.secrets, p.key_last4, p.env_names, p.env_last4, p.created_at, p.updated_at,
	COALESCE(u.default_provider_id = p.id, 0),
	(SELECT COUNT(*) FROM session_leases l WHERE l.provider_id = p.id AND l.holder_worker_id IS NOT NULL AND l.expires_at > ?1)
	FROM providers p JOIN users u ON u.id = p.user_id WHERE p.user_id = ?2`

func scanProvider(s scanner) (store.Provider, error) {
	var p store.Provider
	var kind, scheme, names, last4 string
	err := s.Scan(&p.ID, &p.UserID, &p.Name, &kind, &p.BaseURL, &scheme, &p.FableModel, &p.OpusModel, &p.SonnetModel,
		&p.HaikuModel, &p.MaxSessions, &p.Secrets, &p.KeyLast4, &names, &last4, &p.CreatedAt, &p.UpdatedAt, &p.IsDefault, &p.LeasedSessions)
	if err != nil {
		return p, mapErr(err)
	}
	p.Kind, p.Scheme = store.ProviderKind(kind), store.ProviderScheme(scheme)
	if err := json.Unmarshal([]byte(names), &p.EnvNames); err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(last4), &p.EnvLast4); err != nil {
		return p, err
	}
	if len(p.EnvNames) == 0 {
		p.EnvNames, p.EnvLast4 = nil, nil
	}
	return p, nil
}

// queryProviders reads the user's providers matching where (appended to
// providerQuery) with their live workers, in one transaction.
func (o *ops) queryProviders(ctx context.Context, now int64, userID, where string, args ...any) ([]store.Provider, error) {
	var out []store.Provider
	err := o.atomic(ctx, func(tx *ops) error {
		rows, err := tx.q.QueryContext(ctx, providerQuery+where+` ORDER BY p.name_key, p.id`, append([]any{now, userID}, args...)...)
		if err != nil {
			return mapErr(err)
		}
		if out, err = collect(rows, scanProvider); err != nil {
			return err
		}
		if len(out) == 0 {
			return nil
		}
		ids := make([]string, len(out))
		index := make(map[string]int, len(out))
		for i, p := range out {
			ids[i], index[p.ID] = p.ID, i
		}
		rows, err = tx.q.QueryContext(ctx, `SELECT pw.provider_id, pw.worker_id FROM provider_workers pw
			JOIN workers w ON w.id = pw.worker_id AND w.deleted_at IS NULL
			WHERE pw.provider_id IN (`+placeholders(len(ids))+`) ORDER BY pw.provider_id, pw.position`, anys(ids)...)
		if err != nil {
			return mapErr(err)
		}
		pairs, err := collect(rows, func(s scanner) ([2]string, error) {
			var p [2]string
			return p, mapErr(s.Scan(&p[0], &p[1]))
		})
		if err != nil {
			return err
		}
		for _, p := range pairs {
			i := index[p[0]]
			out[i].WorkerIDs = append(out[i].WorkerIDs, p[1])
		}
		return nil
	})
	return out, err
}

func (o *ops) GetProvider(ctx context.Context, userID, providerID string, now int64) (store.Provider, error) {
	if err := store.CheckID("user id", userID); err != nil {
		return store.Provider{}, err
	}
	if err := store.CheckID("provider id", providerID); err != nil {
		return store.Provider{}, err
	}
	if now == 0 {
		return store.Provider{}, invalid("GetProvider needs a time")
	}
	ps, err := o.queryProviders(ctx, now, userID, ` AND p.id = ?3`, providerID)
	if err != nil {
		return store.Provider{}, err
	}
	if len(ps) == 0 {
		return store.Provider{}, store.ErrNotFound
	}
	return ps[0], nil
}

func (o *ops) ListProviders(ctx context.Context, userID string, now int64) ([]store.Provider, error) {
	if err := store.CheckID("user id", userID); err != nil {
		return nil, err
	}
	if now == 0 {
		return nil, invalid("ListProviders needs a time")
	}
	return o.queryProviders(ctx, now, userID, ``)
}

func (o *ops) CountProviderLeases(ctx context.Context, providerID string, now int64) (int, error) {
	if err := store.CheckID("provider id", providerID); err != nil {
		return 0, err
	}
	if now == 0 {
		return 0, invalid("CountProviderLeases needs a time")
	}
	return o.count(ctx, `SELECT COUNT(*) FROM session_leases WHERE provider_id = ? AND holder_worker_id IS NOT NULL AND expires_at > ?`,
		providerID, now)
}

// providerNameTaken reports ErrExists when another provider of the user
// (not exceptID) has a name with this key.
func (o *ops) providerNameTaken(ctx context.Context, userID, key, exceptID string) error {
	n, err := o.count(ctx, `SELECT COUNT(*) FROM providers WHERE user_id = ? AND name_key = ? AND id <> ?`, userID, key, exceptID)
	if err != nil {
		return err
	}
	if n > 0 {
		return store.ErrExists
	}
	return nil
}

// insertProviderWorkers writes workers at positions 0.. after checking that
// each is a live worker of userID.
func (o *ops) insertProviderWorkers(ctx context.Context, providerID, userID string, workers []string) error {
	for i, w := range workers {
		n, err := o.count(ctx, `SELECT COUNT(*) FROM workers WHERE id = ? AND user_id = ? AND deleted_at IS NULL`, w, userID)
		if err != nil {
			return err
		}
		if n == 0 {
			return &store.ProviderWorkerError{Index: i}
		}
		if _, err := o.exec(ctx, `INSERT INTO provider_workers (provider_id, position, worker_id) VALUES (?,?,?)`,
			providerID, i, w); err != nil {
			return err
		}
	}
	return nil
}

// setDefaultProvider makes providerID the user's default, or, with on
// false, clears the default when it is providerID.
func (o *ops) setDefaultProvider(ctx context.Context, userID, providerID string, on bool) error {
	var err error
	if on {
		_, err = o.exec(ctx, `UPDATE users SET default_provider_id = ? WHERE id = ?`, providerID, userID)
	} else {
		_, err = o.exec(ctx, `UPDATE users SET default_provider_id = NULL WHERE id = ? AND default_provider_id = ?`, userID, providerID)
	}
	return err
}

func jsonList(v []string) string {
	if len(v) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(v) // strings always encode
	return string(b)
}

func (o *ops) CreateProvider(ctx context.Context, p store.Provider) (store.Provider, error) {
	if err := store.CheckProvider(p); err != nil {
		return store.Provider{}, err
	}
	var out store.Provider
	err := o.unit(ctx, func(tx *ops) error {
		var exists int
		if err := tx.q.QueryRow(ctx, `SELECT 1 FROM users WHERE id = ?`, p.UserID).Scan(&exists); err != nil {
			return mapErr(err)
		}
		n, err := tx.count(ctx, `SELECT COUNT(*) FROM providers WHERE user_id = ?`, p.UserID)
		if err != nil {
			return err
		}
		if n >= store.MaxProvidersPerUser {
			return store.ErrLimit
		}
		key := store.ProjectNameKey(p.Name)
		if err := tx.providerNameTaken(ctx, p.UserID, key, p.ID); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `INSERT INTO providers (id, user_id, name, name_key, kind, base_url, scheme, fable_model,
			opus_model, sonnet_model, haiku_model, max_sessions, secrets, key_last4, env_names, env_last4, created_at,
			updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.ID, p.UserID, p.Name, key, string(p.Kind), p.BaseURL, string(p.Scheme), p.FableModel, p.OpusModel,
			p.SonnetModel, p.HaikuModel, p.MaxSessions, p.Secrets, p.KeyLast4, jsonList(p.EnvNames), jsonList(p.EnvLast4), p.CreatedAt, p.UpdatedAt); err != nil {
			return err
		}
		if err := tx.insertProviderWorkers(ctx, p.ID, p.UserID, p.WorkerIDs); err != nil {
			return err
		}
		if p.IsDefault {
			if err := tx.setDefaultProvider(ctx, p.UserID, p.ID, true); err != nil {
				return err
			}
		}
		out, err = tx.GetProvider(ctx, p.UserID, p.ID, p.CreatedAt)
		return err
	})
	if err != nil {
		return store.Provider{}, err
	}
	return out, nil
}

// sameProvider reports whether a and b store the same fields (UpdatedAt,
// CreatedAt and the read-only counts aside).
func sameProvider(a, b store.Provider) bool {
	return a.Name == b.Name && a.BaseURL == b.BaseURL && a.Scheme == b.Scheme && a.Kind == b.Kind &&
		a.FableModel == b.FableModel && a.OpusModel == b.OpusModel && a.SonnetModel == b.SonnetModel && a.HaikuModel == b.HaikuModel &&
		a.MaxSessions == b.MaxSessions && bytes.Equal(a.Secrets, b.Secrets) && a.KeyLast4 == b.KeyLast4 &&
		slices.Equal(a.EnvNames, b.EnvNames) && slices.Equal(a.EnvLast4, b.EnvLast4) &&
		slices.Equal(a.WorkerIDs, b.WorkerIDs) && a.IsDefault == b.IsDefault
}

func (o *ops) UpdateProvider(ctx context.Context, userID string, p store.Provider, expectedUpdatedAt *int64) (store.Provider, error) {
	if err := store.CheckID("user id", userID); err != nil {
		return store.Provider{}, err
	}
	if p.UserID == "" {
		p.UserID = userID
	}
	if p.UserID != userID {
		return store.Provider{}, invalid("provider of another user")
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = 1 // checked against the stored row below
	}
	if err := store.CheckProvider(p); err != nil {
		return store.Provider{}, err
	}
	var out store.Provider
	err := o.unit(ctx, func(tx *ops) error {
		cur, err := tx.GetProvider(ctx, userID, p.ID, p.UpdatedAt)
		if err != nil {
			return err
		}
		if expectedUpdatedAt != nil && *expectedUpdatedAt != cur.UpdatedAt {
			return store.ErrProviderStale
		}
		key := store.ProjectNameKey(p.Name)
		if err := tx.providerNameTaken(ctx, userID, key, p.ID); err != nil {
			return err
		}
		updatedAt := cur.UpdatedAt
		if !sameProvider(cur, p) {
			updatedAt = max(p.UpdatedAt, cur.UpdatedAt+1)
		}
		if _, err := tx.exec(ctx, `UPDATE providers SET name = ?, name_key = ?, base_url = ?, scheme = ?, fable_model = ?,
			opus_model = ?, sonnet_model = ?, haiku_model = ?, max_sessions = ?, secrets = ?, key_last4 = ?, env_names = ?, env_last4 = ?,
			updated_at = ? WHERE id = ?`,
			p.Name, key, p.BaseURL, string(p.Scheme), p.FableModel, p.OpusModel, p.SonnetModel, p.HaikuModel, p.MaxSessions,
			p.Secrets,
			p.KeyLast4, jsonList(p.EnvNames), jsonList(p.EnvLast4), updatedAt, p.ID); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, `DELETE FROM provider_workers WHERE provider_id = ?`, p.ID); err != nil {
			return err
		}
		if err := tx.insertProviderWorkers(ctx, p.ID, userID, p.WorkerIDs); err != nil {
			return err
		}
		if err := tx.setDefaultProvider(ctx, userID, p.ID, p.IsDefault); err != nil {
			return err
		}
		out, err = tx.GetProvider(ctx, userID, p.ID, p.UpdatedAt)
		return err
	})
	if err != nil {
		return store.Provider{}, err
	}
	return out, nil
}

func (o *ops) DeleteProvider(ctx context.Context, userID, providerID string, now int64) error {
	if err := store.CheckID("user id", userID); err != nil {
		return err
	}
	if err := store.CheckID("provider id", providerID); err != nil {
		return err
	}
	if now == 0 {
		return invalid("DeleteProvider needs a time")
	}
	return o.unit(ctx, func(tx *ops) error {
		n, err := tx.count(ctx, `SELECT COUNT(*) FROM providers WHERE id = ? AND user_id = ?`, providerID, userID)
		if err != nil {
			return err
		}
		if n == 0 {
			return store.ErrNotFound
		}
		var inUse bool
		if err := tx.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM session_leases l
			WHERE l.holder_worker_id IS NOT NULL AND l.expires_at > ?2
			AND (l.provider_id = ?1 OR l.session_id IN (SELECT session_id FROM sessions WHERE provider_id = ?1)))`,
			providerID, now).Scan(&inUse); err != nil {
			return mapErr(err)
		}
		if inUse {
			return store.ErrProviderInUse
		}
		_, err = tx.exec(ctx, `DELETE FROM providers WHERE id = ?`, providerID)
		return err
	})
}
