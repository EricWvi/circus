package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/EricWvi/circus/internal/store"
)

// Authentication errors. Callers map them to HTTP statuses before any
// upgrade: ErrUnauthenticated to 401, ErrRevoked to 403. Any other error from
// Authenticate is a store failure (503).
var (
	ErrUnauthenticated = errors.New("auth: unauthenticated")
	ErrRevoked         = errors.New("auth: credential revoked")
)

// DefaultTouchInterval is how often, at most, one process records a
// credential's LastUsedAt.
const DefaultTouchInterval = 5 * time.Minute

// Principal is an authenticated machine-role identity.
type Principal struct {
	Kind         store.PrincipalKind
	ID           string // the device or worker id
	Name         string // the user-visible device or worker name
	UserID       string
	MachineID    string
	CredentialID string
}

// Options configures an Authenticator.
type Options struct {
	// Now supplies time for LastUsedAt; nil means time.Now.
	Now func() time.Time
	// TouchInterval bounds LastUsedAt writes per credential; zero means
	// DefaultTouchInterval.
	TouchInterval time.Duration
	// Logger receives authentication failures (never tokens); nil discards.
	Logger *slog.Logger
}

// Authenticator checks long-term bearer tokens against the store. It is safe
// for concurrent use.
type Authenticator struct {
	st     store.Store
	now    func() time.Time
	touch  time.Duration
	logger *slog.Logger

	mu sync.Mutex
	// seen maps the id of every credential this Authenticator accepted to
	// its token hash, so Valid can re-read it by hash (the store looks
	// credentials up by hash only), and to when it last touched it. One entry
	// per credential, bounded by the number of enrollments.
	seen map[string]*seenCred
}

type seenCred struct {
	hash    []byte
	touched time.Time
}

// NewAuthenticator returns an Authenticator on st.
func NewAuthenticator(st store.Store, opts Options) *Authenticator {
	a := &Authenticator{st: st, now: opts.Now, touch: opts.TouchInterval, logger: opts.Logger, seen: map[string]*seenCred{}}
	if a.now == nil {
		a.now = time.Now
	}
	if a.touch <= 0 {
		a.touch = DefaultTouchInterval
	}
	if a.logger == nil {
		a.logger = slog.New(slog.DiscardHandler)
	}
	return a
}

// BearerToken returns the token of an "Authorization: Bearer <token>" header,
// or "" when the header is missing or has another scheme.
func BearerToken(r *http.Request) string {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(token)
}

// Authenticate checks the request's bearer token. It returns
// ErrUnauthenticated for a missing, malformed or unknown token, ErrRevoked
// for a revoked credential or a deleted identity, and a wrapped store error
// otherwise.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (Principal, error) {
	token := BearerToken(r)
	if !WellFormed(token) {
		return Principal{}, ErrUnauthenticated
	}
	hash := HashToken(token)
	cred, err := a.st.LookupCredential(ctx, hash)
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: look up credential: %w", err)
	}
	if !sameHash(cred.TokenHash, hash) {
		return Principal{}, ErrUnauthenticated
	}
	if cred.Revoked() {
		return Principal{}, ErrRevoked
	}
	id, err := a.st.GetIdentity(ctx, cred.Kind, cred.PrincipalID)
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("auth: look up identity: %w", err)
	}
	if id.DeletedAt != 0 {
		return Principal{}, ErrRevoked
	}
	a.touchCredential(ctx, cred.ID, hash)
	return Principal{Kind: cred.Kind, ID: id.ID, Name: id.Name, UserID: id.UserID, MachineID: id.MachineID, CredentialID: cred.ID}, nil
}

// touchCredential records LastUsedAt when this process has not done so for
// the credential within the touch interval. A failed write is logged and
// does not fail the authentication.
func (a *Authenticator) touchCredential(ctx context.Context, credID string, hash []byte) {
	now := a.now()
	a.mu.Lock()
	s := a.seen[credID]
	if s == nil {
		s = &seenCred{hash: hash}
		a.seen[credID] = s
	}
	due := s.touched.IsZero() || now.Sub(s.touched) >= a.touch
	if due {
		s.touched = now
	}
	a.mu.Unlock()
	if !due {
		return
	}
	if err := a.st.TouchCredential(ctx, credID, now.UnixMilli()); err != nil {
		a.logger.WarnContext(ctx, "record credential use failed", "credential_id", credID, "error", err)
	}
}

// Valid reports whether the credential is still usable: not revoked and its
// identity not deleted. A live connection calls it to notice a revocation
// made after its upgrade (F17).
//
// It must be called on the same Authenticator instance that accepted the
// credential: the store looks credentials up by token hash only, so this
// instance keeps the id → hash mapping of every credential it accepted, and
// Valid reports false for any other id. The mapping goes away once the store
// can read a credential by id (GetCredential).
func (a *Authenticator) Valid(ctx context.Context, credentialID string) (bool, error) {
	a.mu.Lock()
	s := a.seen[credentialID]
	a.mu.Unlock()
	if s == nil {
		return false, nil
	}
	cred, err := a.st.LookupCredential(ctx, s.hash)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: look up credential: %w", err)
	}
	if cred.ID != credentialID || cred.Revoked() {
		return false, nil
	}
	id, err := a.st.GetIdentity(ctx, cred.Kind, cred.PrincipalID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("auth: look up identity: %w", err)
	}
	return id.DeletedAt == 0, nil
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal Require stored in ctx.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Require authenticates every request before next sees it, answering
// failures as Admit does without calling next. next finds the principal with
// PrincipalFrom.
//
// kinds must name at least one valid kind: an endpoint that forgets to say
// which role it serves is a programming error, and Require panics rather
// than let every role through.
func (a *Authenticator) Require(next http.Handler, kinds ...store.PrincipalKind) http.Handler {
	if len(kinds) == 0 {
		panic("auth: Require needs at least one principal kind")
	}
	for _, k := range kinds {
		if !k.Valid() {
			panic(fmt.Sprintf("auth: Require with unknown principal kind %q", k))
		}
	}
	kinds = slices.Clone(kinds)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := Admit(w, r, a, a.logger, kinds...)
		if !ok {
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}
