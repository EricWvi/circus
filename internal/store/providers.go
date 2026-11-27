package store

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"
)

// Provider bounds. The store checks every one of them in the transaction
// that writes the provider; the service validates the request first and
// reports each problem against its field.
const (
	// MaxProvidersPerUser bounds the providers of one user (ErrLimit).
	MaxProvidersPerUser = 32
	// MaxProviderWorkers bounds the workers a provider is assigned to.
	MaxProviderWorkers = 256
	// MaxProviderNameRunes bounds a provider name in Unicode code points.
	MaxProviderNameRunes = 64
	// MaxProviderBaseURLBytes bounds a provider's API base URL.
	MaxProviderBaseURLBytes = 2048
	// MaxProviderModelBytes bounds each alias model id.
	MaxProviderModelBytes = 256
	// MaxProviderKeyBytes bounds the API key.
	MaxProviderKeyBytes = 4096
	// MaxProviderEnv bounds a provider's custom environment variables.
	MaxProviderEnv = 64
	// MaxProviderEnvNameBytes bounds one custom variable name.
	MaxProviderEnvNameBytes = 128
	// MaxProviderEnvBytes bounds the names and values of a provider's
	// custom variables together.
	MaxProviderEnvBytes = 64 << 10
	// MaxProviderMaxSessions is the largest Provider.MaxSessions.
	MaxProviderMaxSessions = 1024
	// MaxProviderSecretsBytes bounds the sealed secrets: the key, every
	// variable value and their names, encoded, plus the nonce and the
	// authentication tag. JSON encoding may escape a byte as six.
	MaxProviderSecretsBytes = 6*(MaxProviderKeyBytes+MaxProviderEnvBytes) + 4096
	// MaxLast4Bytes bounds a stored last-four-characters string.
	MaxLast4Bytes = 16
)

// ProviderKind is what runs with a provider's credentials.
type ProviderKind string

// ProviderKindClaudeCode is Claude Code through the Agent SDK, the only kind.
const ProviderKindClaudeCode ProviderKind = "claude_code"

// ProviderScheme is how the API key is sent.
type ProviderScheme string

const (
	// ProviderSchemeXAPIKey sends the key as x-api-key (ANTHROPIC_API_KEY).
	ProviderSchemeXAPIKey ProviderScheme = "x_api_key"
	// ProviderSchemeBearer sends it as a bearer token (ANTHROPIC_AUTH_TOKEN).
	ProviderSchemeBearer ProviderScheme = "bearer"
)

// ErrProviderInUse reports a DeleteProvider while a session using the
// provider holds a live lease. Nothing was written.
var ErrProviderInUse = errors.New("store: provider in use by a leased session")

// ErrProviderStale reports an UpdateProvider whose ExpectedUpdatedAt differs
// from the provider's UpdatedAt. Nothing was written.
var ErrProviderStale = errors.New("store: provider changed since it was read")

// ErrProviderUnavailable refuses an AcquireLease for a new holder of a
// session whose provider was deleted or is not assigned to the worker.
// Nothing was written.
var ErrProviderUnavailable = errors.New("store: session provider unavailable to this worker")

// ErrProviderFull refuses an AcquireLease for a new holder of a session
// whose provider's MaxSessions sessions already hold live leases. Nothing
// was written.
var ErrProviderFull = errors.New("store: session provider at its session limit")

// ErrUnknownProvider reports a provider id that does not name a provider
// of the user (deleted, another user's, or never created) where a write
// needs one: a project default, a new session, a session's provider
// change. It matches ErrNotFound. Nothing was written.
var ErrUnknownProvider = fmt.Errorf("%w: unknown provider", ErrNotFound)

// ErrTurnRunning refuses SetSessionProvider while a turn runs, and a
// merge or a discard of a session's changeset while a turn runs or is
// about to start. Nothing was written.
var ErrTurnRunning = errors.New("store: a turn is running")

// ProviderWorkerError reports a provider's worker that is not a live worker
// of the provider's user. Index is the worker's position in the written
// list. It matches ErrNotFound.
type ProviderWorkerError struct{ Index int }

func (e *ProviderWorkerError) Error() string {
	return fmt.Sprintf("store: worker %d is not a live worker of the provider's user", e.Index)
}

// Is makes errors.Is(err, ErrNotFound) match a *ProviderWorkerError.
func (e *ProviderWorkerError) Is(target error) bool { return target == ErrNotFound }

// Provider is a Web user's model provider: an API endpoint, alias models,
// custom environment variables, the workers it may run on and how many of
// its sessions may hold leases at once.
//
// The store keeps the key and the variable values only as Secrets, an
// opaque sealed blob it never opens; it keeps the variable names, their
// order and the last four characters of each secret in the clear.
type Provider struct {
	ID     string
	UserID string
	// Name is unique per user ignoring case (ProjectNameKey).
	Name    string
	Kind    ProviderKind
	BaseURL string
	Scheme  ProviderScheme
	// The alias models; empty means not set.
	FableModel, OpusModel, SonnetModel, HaikuModel string
	// KeyLast4 is the key's last four characters.
	KeyLast4 string
	// EnvNames are the custom variables' names in order; EnvLast4 holds
	// each value's last four characters, index for index.
	EnvNames []string
	EnvLast4 []string
	// Secrets is the sealed key and variable values, at most
	// MaxProviderSecretsBytes.
	Secrets []byte
	// WorkerIDs are the workers the provider is assigned to, in the order
	// written; reads leave out workers deleted since.
	WorkerIDs []string
	// MaxSessions bounds the sessions with this provider that hold live
	// leases at once; 0 means unlimited.
	MaxSessions int
	// IsDefault: the user's default provider. On writes, true makes it the
	// default and false clears the default if it was this provider.
	IsDefault bool
	// LeasedSessions, filled by reads, counts the sessions holding a live
	// lease under this provider at the read's time.
	LeasedSessions int
	CreatedAt      int64
	// UpdatedAt is the version of every stored field; on writes, the time
	// of the change (see UpdateProvider).
	UpdatedAt int64
}

// ProviderOps stores providers, their worker assignments and the user's
// default. A provider is deleted outright: sessions, projects and the user
// default that name it keep the dangling id, which reads as deleted.
type ProviderOps interface {
	// CreateProvider inserts p and returns it as stored (CheckProvider).
	// Each worker must be a live worker of p.UserID, else
	// *ProviderWorkerError. ErrExists if the id is taken or another
	// provider of the user has a name with the same ProjectNameKey;
	// ErrLimit past MaxProvidersPerUser; ErrNotFound if the user does not
	// exist. p.IsDefault makes it the user's default.
	CreateProvider(ctx context.Context, p Provider) (Provider, error)
	// GetProvider returns the user's provider, with LeasedSessions counted
	// at now, or ErrNotFound.
	GetProvider(ctx context.Context, userID, providerID string, now int64) (Provider, error)
	// ListProviders returns the user's providers ordered by ProjectNameKey
	// of the name, then id, with LeasedSessions counted at now.
	ListProviders(ctx context.Context, userID string, now int64) ([]Provider, error)
	// UpdateProvider replaces every stored field of the user's provider
	// p.ID with p's (the name, base URL, scheme, models, secrets and their
	// last four characters, variable names, workers, MaxSessions and
	// IsDefault) in one transaction, and returns the result. CreatedAt is
	// kept. With expectedUpdatedAt set and different from the stored
	// UpdatedAt it returns ErrProviderStale and writes nothing. UpdatedAt
	// becomes max(p.UpdatedAt, previous + 1) when something changed and
	// stays otherwise; a byte-identical Secrets is no change. Errors as
	// CreateProvider, and ErrNotFound for an unknown provider. Leases are
	// never touched.
	UpdateProvider(ctx context.Context, userID string, p Provider, expectedUpdatedAt *int64) (Provider, error)
	// DeleteProvider deletes the user's provider and its worker
	// assignments. ErrProviderInUse, writing nothing, while a session that
	// names the provider, or a lease acquired under it, has a live lease at
	// now. ErrNotFound if absent.
	DeleteProvider(ctx context.Context, userID, providerID string, now int64) error
	// CountProviderLeases counts the live leases at now acquired under the
	// provider.
	CountProviderLeases(ctx context.Context, providerID string, now int64) (int, error)
}

// CheckProvider validates what every backend requires of a provider before
// writing it: ids, a name of 1 to MaxProviderNameRunes code points, a known
// kind and scheme, the size of every string, a nonempty sealed secret of
// at most MaxProviderSecretsBytes, 0 to MaxProviderEnv distinct variable
// names with as many last-four strings, 0 to MaxProviderWorkers distinct
// worker ids, MaxSessions in 0..MaxProviderMaxSessions, and times.
func CheckProvider(p Provider) error {
	if err := CheckID("provider id", p.ID); err != nil {
		return err
	}
	if err := CheckID("user id", p.UserID); err != nil {
		return err
	}
	if p.Name == "" || !utf8.ValidString(p.Name) || utf8.RuneCountInString(p.Name) > MaxProviderNameRunes {
		return invalid("provider name must be 1 to %d code points of valid UTF-8", MaxProviderNameRunes)
	}
	if p.Kind != ProviderKindClaudeCode {
		return invalid("unknown provider kind %q", p.Kind)
	}
	if p.Scheme != ProviderSchemeXAPIKey && p.Scheme != ProviderSchemeBearer {
		return invalid("unknown provider scheme %q", p.Scheme)
	}
	if err := CheckText("base URL", p.BaseURL, MaxProviderBaseURLBytes, true); err != nil {
		return err
	}
	for _, m := range []string{p.FableModel, p.OpusModel, p.SonnetModel, p.HaikuModel} {
		if err := CheckText("model", m, MaxProviderModelBytes, true); err != nil {
			return err
		}
	}
	if err := CheckText("key last four", p.KeyLast4, MaxLast4Bytes, true); err != nil {
		return err
	}
	if len(p.Secrets) == 0 || len(p.Secrets) > MaxProviderSecretsBytes {
		return invalid("sealed secrets must be 1 to %d bytes", MaxProviderSecretsBytes)
	}
	if len(p.EnvNames) > MaxProviderEnv || len(p.EnvLast4) != len(p.EnvNames) {
		return invalid("at most %d variables, each with its last four characters", MaxProviderEnv)
	}
	names := make(map[string]bool, len(p.EnvNames))
	for i, n := range p.EnvNames {
		if err := CheckText("variable name", n, MaxProviderEnvNameBytes, false); err != nil {
			return err
		}
		if err := CheckText("variable last four", p.EnvLast4[i], MaxLast4Bytes, true); err != nil {
			return err
		}
		if names[n] {
			return invalid("variable %q listed twice", n)
		}
		names[n] = true
	}
	if len(p.WorkerIDs) > MaxProviderWorkers {
		return invalid("at most %d workers", MaxProviderWorkers)
	}
	if err := CheckIDs("worker id", p.WorkerIDs); err != nil {
		return err
	}
	workers := make(map[string]bool, len(p.WorkerIDs))
	for _, w := range p.WorkerIDs {
		if workers[w] {
			return invalid("worker %q listed twice", w)
		}
		workers[w] = true
	}
	if p.MaxSessions < 0 || p.MaxSessions > MaxProviderMaxSessions {
		return invalid("max sessions must be 0..%d", MaxProviderMaxSessions)
	}
	if p.CreatedAt <= 0 || p.UpdatedAt < p.CreatedAt {
		return invalid("provider needs creation and update times")
	}
	return nil
}
