package store

import (
	"context"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// UserRole is the Web user's authority.
type UserRole string

const (
	RoleUser                UserRole = "user"
	RoleSuper               UserRole = "super"
	WebSessionIdleTimeout            = 7 * 24 * time.Hour
	WebSessionMaxLifetime            = 30 * 24 * time.Hour
	WebSessionTouchInterval          = 5 * time.Minute
)

// NormalizeEmail returns the unique human identity in its storage form.
func NormalizeEmail(email string) string { return strings.ToLower(strings.TrimSpace(email)) }

// CheckEmail performs the deliberately minimal email validation shared by all callers.
func CheckEmail(email string) error {
	if len(email) > 254 || !utf8.ValidString(email) || strings.Count(email, "@") != 1 {
		return invalid("email must contain one @ and be at most 254 bytes")
	}
	local, domain, _ := strings.Cut(email, "@")
	if local == "" || domain == "" || strings.ContainsFunc(email, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return invalid("email needs a local part and domain without whitespace")
	}
	return nil
}

// UserIdentity binds an OIDC issuer and subject to one user.
type UserIdentity struct {
	Issuer, Subject, UserID string
	CreatedAt               int64
}

// WebSession stores only the hash of the browser's random bearer cookie.
// ExpiresAt is the sliding idle deadline capped at CreatedAt + max lifetime.
type WebSession struct {
	TokenHash                        []byte
	UserID                           string
	CreatedAt, LastUsedAt, ExpiresAt int64
}

// WebAuthOps stores Web users, login identities and sessions.
// All times are caller-supplied Unix milliseconds.
type WebAuthOps interface {
	// CreateSuperUser inserts the only super user and optional OIDC identity
	// atomically. ErrExists if any super user or unique identity already exists.
	CreateSuperUser(context.Context, User, *UserIdentity) error
	GetSuperUser(context.Context) (User, error)
	GetUserByEmail(context.Context, string) (User, error)
	// CreateUserIdentity binds an identity; ErrExists if already bound.
	CreateUserIdentity(context.Context, UserIdentity) error
	GetUserIdentity(context.Context, string, string) (UserIdentity, error)
	// HasUserIdentityAtIssuer reports whether a user has any subject at this
	// issuer. Check and bind in one Tx to prevent concurrent email rebinding.
	HasUserIdentityAtIssuer(context.Context, string, string) (bool, error)
	// DeleteUser deletes a user and cascades web sessions/identities and empty
	// projects. Any session under its projects prevents deletion with
	// ErrHasSessions, deleting nothing; callers must remove those sessions
	// through the chunked deletion flow first. Existing
	// machine resources still prevent deletion (ErrNotFound for referenced rows).
	// Missing users are ignored. Like DeleteProject, it first deletes the
	// ledger rows of the user's projects in bounded transactions.
	DeleteUser(context.Context, string) error
	CreateWebSession(context.Context, WebSession) error
	// UseWebSession rejects expired sessions with ErrNotFound, and advances
	// LastUsedAt/ExpiresAt at most once per five minutes, in one transaction.
	UseWebSession(context.Context, []byte, int64) (WebSession, error)
	DeleteWebSession(context.Context, []byte) error
	// PurgeWebSessions deletes sessions whose ExpiresAt is at or before now.
	PurgeWebSessions(context.Context, int64) (int, error)
}
