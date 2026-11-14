package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/EricWvi/circus/internal/clock"
	"github.com/EricWvi/circus/internal/store"
)

// LinkAuthenticator is what a link handler needs from the host's single
// *Authenticator: upgrade authentication and live revalidation of the
// accepted credential. Tests may inject failures.
type LinkAuthenticator interface {
	Authenticate(context.Context, *http.Request) (Principal, error)
	Valid(ctx context.Context, credentialID string) (bool, error)
}

// Admit authenticates r with a and requires one of kinds. On failure it
// writes the one HTTP mapping every endpoint uses and returns false: 401
// (with WWW-Authenticate: Bearer) for ErrUnauthenticated, 403 for ErrRevoked
// or a principal of another kind, and 503 for any other error (a store
// failure), so an unauthenticated request never reaches a WebSocket upgrade.
func Admit(w http.ResponseWriter, r *http.Request, a LinkAuthenticator, logger *slog.Logger, kinds ...store.PrincipalKind) (Principal, bool) {
	p, err := a.Authenticate(r.Context(), r)
	log := logger.With("path", r.URL.Path, "remote", r.RemoteAddr)
	switch {
	case errors.Is(err, ErrUnauthenticated):
		log.InfoContext(r.Context(), "request rejected", "status", http.StatusUnauthorized, "reason", "unknown or malformed token")
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return Principal{}, false
	case errors.Is(err, ErrRevoked):
		log.InfoContext(r.Context(), "request rejected", "status", http.StatusForbidden, "reason", "credential revoked")
		http.Error(w, "credential revoked", http.StatusForbidden)
		return Principal{}, false
	case err != nil:
		log.ErrorContext(r.Context(), "authentication failed", "error", err)
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return Principal{}, false
	}
	if !slices.Contains(kinds, p.Kind) {
		log.InfoContext(r.Context(), "request rejected", "status", http.StatusForbidden, "reason", "wrong role",
			"kind", p.Kind, "principal_id", p.ID)
		roles := make([]string, len(kinds))
		for i, k := range kinds {
			roles[i] = RoleName(k)
		}
		http.Error(w, fmt.Sprintf("wrong role: a %s credential was supplied; use a %s credential", RoleName(p.Kind), strings.Join(roles, " or ")), http.StatusForbidden)
		return Principal{}, false
	}
	return p, true
}

// RecheckPolicy bounds how long a live link may outlast a revocation of its
// credential. Zero fields take the defaults: a check every second, each
// bounded by five seconds, and at most 30 seconds without a valid check.
type RecheckPolicy struct {
	Interval, Timeout, MaxStale time.Duration
}

// Normalize fills zero fields with the defaults and refuses negative ones.
func (p RecheckPolicy) Normalize() (RecheckPolicy, error) {
	if p.Interval == 0 {
		p.Interval = time.Second
	}
	if p.Timeout == 0 {
		p.Timeout = 5 * time.Second
	}
	if p.MaxStale == 0 {
		p.MaxStale = 30 * time.Second
	}
	if p.Interval < 0 || p.Timeout < 0 || p.MaxStale < 0 {
		return p, errors.New("auth: invalid recheck policy")
	}
	return p, nil
}

// Watch revalidates a live link's credential on clk until ctx ends. The
// first check runs after a random phase in [0, Interval), which desynchronizes
// links, and then every Interval; each check is bounded by Timeout. valid
// reports whether the credential is still usable.
//
// A definitive revoke (valid returns false or ErrRevoked within Timeout)
// calls onClose at once, so it is observed within one Interval plus one
// Timeout. A failed or timed-out check keeps the link until MaxStale has
// passed since the last valid check (or since Watch started); onClose is
// then called. onClose runs at most once and Watch returns after it. p must
// be normalized.
func (p RecheckPolicy) Watch(ctx context.Context, clk clock.Clock, valid func(context.Context) (bool, error), onClose func(definitive bool, lastValid time.Time, err error)) {
	lastValid := clk.Now()
	delay := time.Duration(rand.Float64() * float64(p.Interval))
	for {
		alarm := clk.After(delay)
		select {
		case <-ctx.Done():
			return
		case <-alarm:
		}
		check, finish := clock.WithTimeout(ctx, clk, p.Timeout)
		ok, err := valid(check)
		timedOut := check.Err() != nil
		finish()
		if ctx.Err() != nil {
			return
		}
		if ok && err == nil && !timedOut {
			lastValid = clk.Now()
		} else {
			definitive := !timedOut && (errors.Is(err, ErrRevoked) || err == nil && !ok)
			if definitive || clk.Now().Sub(lastValid) >= p.MaxStale {
				onClose(definitive, lastValid, err)
				return
			}
		}
		delay = p.Interval
	}
}
