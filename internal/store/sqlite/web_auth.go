package sqlite

import (
	"context"
	"crypto/sha256"
	"errors"

	"github.com/EricWvi/circus/internal/store"
)

func normalizeUser(u store.User) (store.User, error) {
	if err := store.CheckID("user id", u.ID); err != nil {
		return u, err
	}
	u.Email = store.NormalizeEmail(u.Email)
	if u.Email != "" {
		if err := store.CheckEmail(u.Email); err != nil {
			return u, err
		}
		if u.Role != store.RoleUser && u.Role != store.RoleSuper {
			return u, invalid("human user needs a role")
		}
		if u.CreatedAt <= 0 {
			return u, invalid("user needs a creation time")
		}
		if u.Name == "" {
			u.Name = u.ID
		}
	} else if u.Role != "" || u.PasswordHash != "" {
		return u, invalid("human user needs an email")
	}
	if u.Name == "" {
		return u, invalid("user needs a name")
	}
	return u, nil
}

func (o *ops) insertUser(ctx context.Context, u store.User) error {
	u, err := normalizeUser(u)
	if err != nil {
		return err
	}
	_, err = o.exec(ctx, `INSERT INTO users (id, name, created_at, email, role, password_hash) VALUES (?,?,?,?,?,?)`,
		u.ID, u.Name, u.CreatedAt, nstr(u.Email), nstr(string(u.Role)), nstr(u.PasswordHash))
	return err
}

func checkUserIdentity(id store.UserIdentity) error {
	if id.Issuer == "" || id.Subject == "" || id.UserID == "" || id.CreatedAt <= 0 {
		return invalid("identity needs issuer, subject, user id and creation time")
	}
	return nil
}

func (o *ops) CreateSuperUser(ctx context.Context, u store.User, id *store.UserIdentity) error {
	if u.Email == "" || u.Role != store.RoleSuper {
		return invalid("super user needs an email and super role")
	}
	var err error
	u, err = normalizeUser(u)
	if err != nil {
		return err
	}
	if id != nil {
		if err := checkUserIdentity(*id); err != nil {
			return err
		}
		if id.UserID != u.ID {
			return invalid("identity must belong to the new user")
		}
	}
	return o.unit(ctx, func(tx *ops) error {
		if _, err := tx.GetSuperUser(ctx); err == nil {
			return store.ErrExists
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := tx.insertUser(ctx, u); err != nil {
			return err
		}
		if id != nil {
			return tx.CreateUserIdentity(ctx, *id)
		}
		return nil
	})
}
func (o *ops) GetSuperUser(ctx context.Context) (store.User, error) {
	return o.getUser(ctx, "role", "super")
}
func (o *ops) GetUserByEmail(ctx context.Context, email string) (store.User, error) {
	return o.getUser(ctx, "email", store.NormalizeEmail(email))
}
func (o *ops) CreateUserIdentity(ctx context.Context, id store.UserIdentity) error {
	if err := checkUserIdentity(id); err != nil {
		return err
	}
	_, err := o.exec(ctx, `INSERT INTO user_identities (issuer, subject, user_id, created_at) VALUES (?,?,?,?)`, id.Issuer, id.Subject, id.UserID, id.CreatedAt)
	return err
}
func (o *ops) GetUserIdentity(ctx context.Context, issuer, subject string) (store.UserIdentity, error) {
	if issuer == "" || subject == "" {
		return store.UserIdentity{}, invalid("identity needs issuer and subject")
	}
	var id store.UserIdentity
	err := o.q.QueryRow(ctx, `SELECT issuer, subject, user_id, created_at FROM user_identities WHERE issuer = ? AND subject = ?`, issuer, subject).
		Scan(&id.Issuer, &id.Subject, &id.UserID, &id.CreatedAt)
	return id, mapErr(err)
}
func (o *ops) DeleteUser(ctx context.Context, id string) error {
	if err := store.CheckID("user id", id); err != nil {
		return err
	}
	_, err := o.exec(ctx, `DELETE FROM users WHERE id = ?`, id)
	return err
}
func (o *ops) CreateWebSession(ctx context.Context, s store.WebSession) error {
	if len(s.TokenHash) != sha256.Size || s.UserID == "" || s.CreatedAt <= 0 || s.LastUsedAt < s.CreatedAt ||
		s.ExpiresAt <= s.LastUsedAt || s.ExpiresAt > s.CreatedAt+store.WebSessionMaxLifetime.Milliseconds() ||
		s.ExpiresAt > s.LastUsedAt+store.WebSessionIdleTimeout.Milliseconds() {
		return invalid("invalid web session")
	}
	_, err := o.exec(ctx, `INSERT INTO web_sessions (token_hash, user_id, created_at, last_used_at, expires_at) VALUES (?,?,?,?,?)`,
		s.TokenHash, s.UserID, s.CreatedAt, s.LastUsedAt, s.ExpiresAt)
	return err
}
func (o *ops) UseWebSession(ctx context.Context, hash []byte, now int64) (store.WebSession, error) {
	if len(hash) != sha256.Size || now <= 0 {
		return store.WebSession{}, invalid("session needs SHA-256 hash and time")
	}
	var s store.WebSession
	err := o.unit(ctx, func(tx *ops) error {
		if err := tx.q.QueryRow(ctx, `SELECT token_hash, user_id, created_at, last_used_at, expires_at FROM web_sessions WHERE token_hash = ?`, hash).
			Scan(&s.TokenHash, &s.UserID, &s.CreatedAt, &s.LastUsedAt, &s.ExpiresAt); err != nil {
			return mapErr(err)
		}
		if now >= s.ExpiresAt || now >= s.CreatedAt+store.WebSessionMaxLifetime.Milliseconds() {
			return store.ErrNotFound
		}
		if now-s.LastUsedAt >= store.WebSessionTouchInterval.Milliseconds() {
			s.LastUsedAt = now
			s.ExpiresAt = min(now+store.WebSessionIdleTimeout.Milliseconds(), s.CreatedAt+store.WebSessionMaxLifetime.Milliseconds())
			_, err := tx.exec(ctx, `UPDATE web_sessions SET last_used_at = ?, expires_at = ? WHERE token_hash = ?`, s.LastUsedAt, s.ExpiresAt, hash)
			return err
		}
		return nil
	})
	if err != nil {
		return store.WebSession{}, err
	}
	return s, nil
}
func (o *ops) DeleteWebSession(ctx context.Context, hash []byte) error {
	if len(hash) != sha256.Size {
		return invalid("session needs SHA-256 hash")
	}
	_, err := o.exec(ctx, `DELETE FROM web_sessions WHERE token_hash = ?`, hash)
	return err
}

func (o *ops) HasUserIdentityAtIssuer(ctx context.Context, userID, issuer string) (bool, error) {
	if err := store.CheckID("user id", userID); err != nil {
		return false, err
	}
	if issuer == "" {
		return false, invalid("identity needs issuer")
	}
	var found bool
	err := o.q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM user_identities WHERE user_id = ? AND issuer = ?)`, userID, issuer).Scan(&found)
	return found, mapErr(err)
}
func (o *ops) PurgeWebSessions(ctx context.Context, now int64) (int, error) {
	if now <= 0 {
		return 0, invalid("session purge needs a time")
	}
	return o.exec(ctx, `DELETE FROM web_sessions WHERE expires_at <= ?`, now)
}
