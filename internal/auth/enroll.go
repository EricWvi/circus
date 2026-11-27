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
	// deliberately indistinguishable.
	ErrEnrollRejected = errors.New("enrollment token rejected: it is unknown, already used, expired, or not issued for this role")
	// ErrIdentityLimit reports a full identity quota. The token remains unused.
	ErrIdentityLimit = errors.New("live identity limit reached")
	// ErrNameTaken reports that the token's user already has a live identity
	// of the requested role whose name differs at most in ASCII case. The
	// token is not consumed.
	ErrNameTaken = errors.New("name already taken")
	// ErrReplaceOnline refuses a rejoin while the identity it replaces is
	// connected. The token is not consumed.
	ErrReplaceOnline = errors.New("the identity this link replaces is online")
	// ErrReplaceGone refuses a rejoin whose identity was deleted, or is no
	// longer the token user's, since the token was issued. The token is not
	// consumed, but it can never be used.
	ErrReplaceGone = errors.New("the identity this link replaces no longer exists")
	// ErrInvalidRequest reports a malformed enrollment or token request.
	ErrInvalidRequest = errors.New("invalid request")
)

const (
	// DefaultEnrollTTL is the default lifetime of a one-time token.
	DefaultEnrollTTL = time.Hour
	// MaxEnrollTTL bounds a one-time token's lifetime.
	MaxEnrollTTL = 7 * 24 * time.Hour
	// MaxLiveDevices and MaxLiveWorkers are per-user enrollment limits.
	MaxLiveDevices = 32
	MaxLiveWorkers = 4
	// MaxNameLen bounds an identity name.
	MaxNameLen = 63
	// maxFieldLen bounds the free-form identity fields a client reports.
	maxFieldLen = 255
)

// CheckIdentityLimit reports ErrIdentityLimit when userID has no enrollment
// slot for kind. Use the same transaction for this check and enrollment.
func CheckIdentityLimit(ctx context.Context, st store.Ops, userID string, kind store.PrincipalKind) error {
	n, err := st.CountLiveIdentities(ctx, kind, userID)
	if err != nil {
		return err
	}
	limit := MaxLiveDevices
	if kind == store.KindWorker {
		limit = MaxLiveWorkers
	}
	if n >= limit {
		return ErrIdentityLimit
	}
	return nil
}

// CreateEnrollToken stores a one-time token for one role of the user and
// returns its text form, which the caller shows once and then forgets. ttl
// zero means DefaultEnrollTTL; it must not exceed MaxEnrollTTL.
func CreateEnrollToken(ctx context.Context, st store.Ops, userID string, kind store.PrincipalKind, ttl time.Duration, now time.Time) (string, store.EnrollToken, error) {
	return createEnrollToken(ctx, st, userID, kind, "", ttl, now)
}

// CreateRejoinToken stores a one-time token like CreateEnrollToken whose
// enrollment replaces the user's live identity target of kind: it revokes
// target (as RevokeByID does) and takes its name (see Enroll).
// ErrNoSuchIdentity when target is not a live identity of kind of userID.
// Run it in the transaction that decided on target.
func CreateRejoinToken(ctx context.Context, st store.Ops, userID string, kind store.PrincipalKind, target string, ttl time.Duration, now time.Time) (string, store.EnrollToken, error) {
	if !kind.Valid() {
		return "", store.EnrollToken{}, fmt.Errorf("%w: unknown role %q", ErrInvalidRequest, kind)
	}
	if _, err := liveIdentity(ctx, st, kind, userID, target); err != nil {
		return "", store.EnrollToken{}, err
	}
	return createEnrollToken(ctx, st, userID, kind, target, ttl, now)
}

// liveIdentity returns the live identity id of kind owned by userID, or
// ErrNoSuchIdentity.
func liveIdentity(ctx context.Context, st store.Ops, kind store.PrincipalKind, userID, id string) (store.Identity, error) {
	if id == "" {
		return store.Identity{}, fmt.Errorf("%w: no %s id", ErrNoSuchIdentity, AdminNoun(kind))
	}
	ident, err := st.GetIdentity(ctx, kind, id)
	if errors.Is(err, store.ErrNotFound) || err == nil && (ident.DeletedAt != 0 || ident.UserID != userID) {
		return store.Identity{}, fmt.Errorf("%w: no %s %s", ErrNoSuchIdentity, AdminNoun(kind), id)
	}
	return ident, err
}

func createEnrollToken(ctx context.Context, st store.Ops, userID string, kind store.PrincipalKind, replace string, ttl time.Duration, now time.Time) (string, store.EnrollToken, error) {
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
		ReplaceID: replace,
	}
	if err := st.CreateEnrollToken(ctx, t); err != nil {
		return "", store.EnrollToken{}, fmt.Errorf("auth: store enroll token: %w", err)
	}
	return token, t, nil
}

// EnrollRequest is what a joining machine presents for one role, and how
// the control plane tells whether an identity is connected.
type EnrollRequest struct {
	Token string // the one-time token; secret
	Kind  store.PrincipalKind
	// Name is the identity name; empty for the first label of Hostname,
	// or, with a rejoin token, for the replaced identity's name.
	Name          string
	Hostname      string
	MachineID     string
	OS            string
	Arch          string
	ClientVersion string
	// Online reports whether the identity a rejoin token replaces is
	// connected now; see Enroll. It is the control plane's, not the
	// client's. Nil counts every identity as online, so no rejoin succeeds.
	Online OnlineFunc
}

// OnlineFunc reports whether the live identity id of kind, owned by
// userID, is connected to the control plane now.
type OnlineFunc func(ctx context.Context, kind store.PrincipalKind, userID, id string) (bool, error)

// Enrollment is the outcome of a successful Enroll.
type Enrollment struct {
	Identity     store.Identity
	CredentialID string
	// Token is the long-term bearer token. Secret: hand it to the client and
	// drop it.
	Token string
	// Replaced is the identity a rejoin token replaced, as it was before
	// its revocation; its ID is empty for a plain enrollment.
	// ReplacedCredentials counts the credentials the revocation revoked.
	Replaced            store.Identity
	ReplacedCredentials int
}

// Enroll exchanges a one-time token for a new identity of the requested kind
// and its long-term credential, in one store transaction (see the package
// doc). Token failures return ErrEnrollRejected, a full identity quota
// ErrIdentityLimit, a taken name ErrNameTaken, malformed fields
// ErrInvalidRequest, and a rejoin that cannot replace its identity
// ErrReplaceOnline or ErrReplaceGone. Other errors come from the store.
// Names are compared ignoring ASCII case (store.IdentityNameKey).
//
// A token issued by CreateRejoinToken replaces its identity: in the same
// transaction Enroll checks that the identity is still live and the token
// user's (ErrReplaceGone), asks req.Online whether it is connected
// (ErrReplaceOnline), revokes it as RevokeByID does and creates the new
// identity under its name. A request naming another name is refused
// (ErrInvalidRequest); the hostname is not used. Nothing the client
// reports, its machine id included, takes part in that decision.
//
// Online is asked inside the transaction, after the token is used, and its
// answer is final for this enrollment. An identity whose link is closing
// counts as online until the control plane has finished closing it (it is
// still listed as connected until then), so a rejoin that races a stopping
// daemon or worker is refused and succeeds when retried a moment later. A
// link that connects with the old credential after the check is cut as an
// administrator's revocation cuts it: new connections are refused at once
// and live ones end at their next credential check.
func Enroll(ctx context.Context, st store.Store, req EnrollRequest, now time.Time) (Enrollment, error) {
	if !req.Kind.Valid() {
		return Enrollment{}, fmt.Errorf("%w: unknown role %q", ErrInvalidRequest, req.Kind)
	}
	if req.Name != "" {
		if err := ValidateName(req.Name); err != nil {
			return Enrollment{}, err
		}
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
		name := req.Name
		var replaced store.Identity
		var n int
		if tok.ReplaceID != "" {
			if replaced, n, err = replace(ctx, tx, req, tok, now); err != nil {
				return err
			}
			name = replaced.Name
		} else if name == "" {
			name = DefaultName(req.Hostname)
			if err := ValidateName(name); err != nil {
				return err
			}
		}
		if err := CheckIdentityLimit(ctx, tx, tok.UserID, req.Kind); err != nil {
			return err
		}
		id := store.Identity{
			Kind: req.Kind, ID: identityID, UserID: tok.UserID, Name: name,
			MachineID: req.MachineID, OS: req.OS, Arch: req.Arch, Hostname: req.Hostname,
			ClientVersion: req.ClientVersion, CreatedAt: at,
		}
		if err := tx.CreateIdentity(ctx, id); err != nil {
			if errors.Is(err, store.ErrExists) {
				held := name
				if h, err := tx.GetIdentityByName(ctx, req.Kind, tok.UserID, name); err == nil {
					held = h.Name
				}
				return fmt.Errorf("%w: a %s named %q already exists", ErrNameTaken, RoleName(req.Kind), held)
			}
			return err
		}
		cred := store.Credential{ID: credID, Kind: req.Kind, PrincipalID: identityID, TokenHash: HashToken(longTerm), CreatedAt: at}
		if err := tx.CreateCredential(ctx, cred); err != nil {
			return err
		}
		out = Enrollment{Identity: id, CredentialID: credID, Token: longTerm, Replaced: replaced, ReplacedCredentials: n}
		return nil
	})
	if err != nil {
		return Enrollment{}, err
	}
	return out, nil
}

// replace revokes the identity a rejoin token tok replaces, after checking
// that it is still live and tok's user's, that req names no other name and
// that it is offline; it returns the identity as it was and how many
// credentials the revocation revoked.
func replace(ctx context.Context, tx store.Ops, req EnrollRequest, tok store.EnrollToken, now time.Time) (store.Identity, int, error) {
	target, err := liveIdentity(ctx, tx, req.Kind, tok.UserID, tok.ReplaceID)
	if errors.Is(err, ErrNoSuchIdentity) {
		return store.Identity{}, 0, fmt.Errorf("%w: it was deleted since the link was issued; ask for a new link", ErrReplaceGone)
	}
	if err != nil {
		return store.Identity{}, 0, err
	}
	if req.Name != "" && store.IdentityNameKey(req.Name) != store.IdentityNameKey(target.Name) {
		return store.Identity{}, 0, fmt.Errorf("%w: this link rejoins as %s %q, which keeps its name; join without --name, or with --name %s",
			ErrInvalidRequest, RoleName(req.Kind), target.Name, target.Name)
	}
	online := true
	if req.Online != nil {
		if online, err = req.Online(ctx, req.Kind, tok.UserID, target.ID); err != nil {
			return store.Identity{}, 0, fmt.Errorf("auth: check whether %s %s is online: %w", RoleName(req.Kind), target.ID, err)
		}
	}
	if online {
		return store.Identity{}, 0, fmt.Errorf("%w: %s %q (%s) is connected now", ErrReplaceOnline, RoleName(req.Kind), target.Name, target.ID)
	}
	n, err := revokeIn(ctx, tx, req.Kind, target.ID, now)
	if err != nil {
		return store.Identity{}, 0, err
	}
	return target, n, nil
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
// starting with a letter or digit. Names address devices in tool calls, so
// they stay simple to type and quote.
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
