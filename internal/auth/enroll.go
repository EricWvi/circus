package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/EricWvi/circus/internal/store"
)

// Enrollment errors. The messages are safe to show to the enrolling client.
var (
	// ErrEnrollRejected is every token failure: unknown, already used,
	// expired, or not issued for the requested role. The cases are
	// deliberately indistinguishable (F20).
	ErrEnrollRejected = errors.New("enrollment token rejected: it is unknown, already used, expired, or not issued for this role")
	// ErrNameTaken reports that the token's user already has a live identity
	// of the requested role with that name. The token is not consumed.
	ErrNameTaken = errors.New("name already taken")
	// ErrInvalidRequest reports a malformed enrollment or token request.
	ErrInvalidRequest = errors.New("invalid request")
)

const (
	// DefaultUserName is the single user M2 creates (outline 4.1); every
	// identity and token belongs to it until logins arrive (D24, M4).
	DefaultUserName = "admin"
	// DefaultEnrollTTL is the default lifetime of a one-time token.
	DefaultEnrollTTL = time.Hour
	// MaxEnrollTTL bounds a one-time token's lifetime.
	MaxEnrollTTL = 7 * 24 * time.Hour
	// MaxNameLen bounds an identity name.
	MaxNameLen = 63
	// maxFieldLen bounds the free-form identity fields a client reports.
	maxFieldLen = 255
)

// EnsureUser returns the user named name, creating it at time now if absent.
// Concurrent callers (the server and an enroll-token command on the same
// database) get the same user.
func EnsureUser(ctx context.Context, st store.Store, name string, now time.Time) (store.User, error) {
	u, err := st.GetUserByName(ctx, name)
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		return u, err
	}
	id, err := newID()
	if err != nil {
		return store.User{}, err
	}
	u = store.User{ID: id, Name: name, CreatedAt: now.UnixMilli()}
	switch err := st.CreateUser(ctx, u); {
	case err == nil:
		return u, nil
	case errors.Is(err, store.ErrExists):
		return st.GetUserByName(ctx, name)
	default:
		return store.User{}, err
	}
}

// CreateEnrollToken stores a one-time token for one role of the user and
// returns its text form, which the caller shows once and then forgets. ttl
// zero means DefaultEnrollTTL; it must not exceed MaxEnrollTTL.
func CreateEnrollToken(ctx context.Context, st store.Store, userID string, kind store.PrincipalKind, ttl time.Duration, now time.Time) (string, store.EnrollToken, error) {
	if ttl == 0 {
		ttl = DefaultEnrollTTL
	}
	if ttl < time.Second || ttl > MaxEnrollTTL {
		return "", store.EnrollToken{}, fmt.Errorf("%w: ttl must be between 1s and %v", ErrInvalidRequest, MaxEnrollTTL)
	}
	if !kind.Valid() {
		return "", store.EnrollToken{}, fmt.Errorf("%w: unknown role %q", ErrInvalidRequest, kind)
	}
	token, err := NewToken()
	if err != nil {
		return "", store.EnrollToken{}, err
	}
	id, err := newID()
	if err != nil {
		return "", store.EnrollToken{}, err
	}
	t := store.EnrollToken{
		ID:        id,
		UserID:    userID,
		TokenHash: HashToken(token),
		Kinds:     []store.PrincipalKind{kind},
		CreatedAt: now.UnixMilli(),
		ExpiresAt: now.Add(ttl).UnixMilli(),
	}
	if err := st.CreateEnrollToken(ctx, t); err != nil {
		return "", store.EnrollToken{}, fmt.Errorf("auth: store enroll token: %w", err)
	}
	return token, t, nil
}

// EnrollRequest is what a joining machine presents for one role.
type EnrollRequest struct {
	Token         string // the one-time token; secret
	Kind          store.PrincipalKind
	Name          string // identity name; defaults to the first label of Hostname
	Hostname      string
	MachineID     string
	OS            string
	Arch          string
	ClientVersion string
}

// Enrollment is the outcome of a successful Enroll.
type Enrollment struct {
	Identity     store.Identity
	CredentialID string
	// Token is the long-term bearer token. Secret: hand it to the client and
	// drop it.
	Token string
}

// Enroll exchanges a one-time token for a new identity of the requested kind
// and its long-term credential, in one store transaction (see the package
// doc). Token failures return ErrEnrollRejected, a taken name ErrNameTaken,
// malformed fields ErrInvalidRequest; any other error is the store's.
func Enroll(ctx context.Context, st store.Store, req EnrollRequest, now time.Time) (Enrollment, error) {
	if !req.Kind.Valid() {
		return Enrollment{}, fmt.Errorf("%w: unknown role %q", ErrInvalidRequest, req.Kind)
	}
	if req.Name == "" {
		req.Name = DefaultName(req.Hostname)
	}
	if err := ValidateName(req.Name); err != nil {
		return Enrollment{}, err
	}
	if req.MachineID == "" {
		return Enrollment{}, fmt.Errorf("%w: machine id is required", ErrInvalidRequest)
	}
	for field, v := range map[string]string{
		"hostname": req.Hostname, "machine id": req.MachineID, "os": req.OS,
		"arch": req.Arch, "client version": req.ClientVersion,
	} {
		if len(v) > maxFieldLen {
			return Enrollment{}, fmt.Errorf("%w: %s longer than %d bytes", ErrInvalidRequest, field, maxFieldLen)
		}
	}
	if !WellFormed(req.Token) {
		return Enrollment{}, ErrEnrollRejected
	}
	identityID, err := newID()
	if err != nil {
		return Enrollment{}, err
	}
	credID, err := newID()
	if err != nil {
		return Enrollment{}, err
	}
	longTerm, err := NewToken()
	if err != nil {
		return Enrollment{}, err
	}
	hash := HashToken(req.Token)
	at := now.UnixMilli()
	var out Enrollment
	err = st.Tx(ctx, func(ctx context.Context, tx store.Ops) error {
		tok, err := tx.UseEnrollToken(ctx, hash, at, req.MachineID)
		if errors.Is(err, store.ErrTokenUnusable) {
			return ErrEnrollRejected
		}
		if err != nil {
			return err
		}
		if !sameHash(tok.TokenHash, hash) || !slices.Contains(tok.Kinds, req.Kind) {
			return ErrEnrollRejected
		}
		id := store.Identity{
			Kind: req.Kind, ID: identityID, UserID: tok.UserID, Name: req.Name,
			MachineID: req.MachineID, OS: req.OS, Arch: req.Arch, Hostname: req.Hostname,
			ClientVersion: req.ClientVersion, CreatedAt: at,
		}
		if err := tx.CreateIdentity(ctx, id); err != nil {
			if errors.Is(err, store.ErrExists) {
				return fmt.Errorf("%w: a %s named %q already exists", ErrNameTaken, RoleName(req.Kind), req.Name)
			}
			return err
		}
		cred := store.Credential{ID: credID, Kind: req.Kind, PrincipalID: identityID, TokenHash: HashToken(longTerm), CreatedAt: at}
		if err := tx.CreateCredential(ctx, cred); err != nil {
			return err
		}
		out = Enrollment{Identity: id, CredentialID: credID, Token: longTerm}
		return nil
	})
	if err != nil {
		return Enrollment{}, err
	}
	return out, nil
}

// RoleName returns the user-facing role of a kind: "daemon" for a device
// identity, "worker" for a worker.
func RoleName(k store.PrincipalKind) string {
	if k == store.KindDevice {
		return "daemon"
	}
	return string(k)
}

// AdminNoun returns the administrative noun of a kind, as the server CLI
// spells it: "device" or "worker" (`circus server device revoke`).
func AdminNoun(k store.PrincipalKind) string { return string(k) }

// DefaultName derives an identity name from a hostname: its first label
// ("laptop.local" → "laptop").
func DefaultName(hostname string) string {
	for i := range len(hostname) {
		if hostname[i] == '.' {
			return hostname[:i]
		}
	}
	return hostname
}

// ValidateName accepts 1 to MaxNameLen characters from [A-Za-z0-9._-],
// starting with a letter or digit. Names address devices in tool calls (D3),
// so they stay simple to type and quote.
func ValidateName(name string) error {
	if name == "" || len(name) > MaxNameLen {
		return fmt.Errorf("%w: name must be 1 to %d characters; pass --name", ErrInvalidRequest, MaxNameLen)
	}
	for i := range len(name) {
		c := name[i]
		alnum := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
		if !alnum && (i == 0 || c != '.' && c != '_' && c != '-') {
			return fmt.Errorf("%w: name %q must use letters, digits, '.', '_' or '-' and start with a letter or digit; pass --name", ErrInvalidRequest, name)
		}
	}
	return nil
}
