// Package providers seals and opens the secrets of model providers and
// builds the environment a worker gives the Claude Code CLI.
//
// A provider's key and custom variable values are one JSON document sealed
// with AES-256-GCM: a random 12-byte nonce per write, stored in front of
// the ciphertext, and the provider id as additional data, so a sealed blob
// moved to another provider does not open. The 32-byte key lives in its
// own file (LoadOrCreateKey), never in the database.
//
// Nothing here logs. Secrets and Var values must never reach a log or an
// error message: Secrets formats as "[redacted]" for fmt and slog, and
// errors name no value.
package providers

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/EricWvi/circus/internal/store"
)

// KeyBytes is the size of the sealing key.
const KeyBytes = 32

const nonceBytes = 12

// ErrUnreadable reports sealed secrets that do not open with this key: the
// key file was lost or replaced, or the blob belongs to another provider.
var ErrUnreadable = errors.New("providers: secrets cannot be decrypted")

// Box seals and opens provider secrets with one key. It is safe for
// concurrent use.
type Box struct{ aead cipher.AEAD }

// NewBox returns a Box for a KeyBytes key.
func NewBox(key []byte) (*Box, error) {
	if len(key) != KeyBytes {
		return nil, fmt.Errorf("providers: key must be %d bytes", KeyBytes)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// EnvValue is one custom variable with its secret value.
type EnvValue struct {
	Name  string `json:"n"`
	Value string `json:"v"`
}

// Secrets is what a provider keeps sealed: the API key and the value of
// every custom variable, in the provider's order.
type Secrets struct {
	Key string     `json:"k"`
	Env []EnvValue `json:"e"`
}

// String hides the secrets from fmt.
func (Secrets) String() string { return "[redacted]" }

// GoString hides the secrets from %#v.
func (Secrets) GoString() string { return "[redacted]" }

// LogValue hides the secrets from slog.
func (Secrets) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// Equal reports whether two secrets hold the same key and variables in the
// same order.
func (s Secrets) Equal(o Secrets) bool {
	if s.Key != o.Key || len(s.Env) != len(o.Env) {
		return false
	}
	for i := range s.Env {
		if s.Env[i] != o.Env[i] {
			return false
		}
	}
	return true
}

// Value returns the value of variable name and whether it is present.
func (s Secrets) Value(name string) (string, bool) {
	for _, e := range s.Env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

// Seal encrypts s for provider providerID with a fresh random nonce.
func (b *Box) Seal(providerID string, s Secrets) ([]byte, error) {
	plain, err := json.Marshal(s)
	if err != nil {
		return nil, errors.New("providers: encode secrets")
	}
	defer clear(plain)
	out := make([]byte, nonceBytes, nonceBytes+len(plain)+b.aead.Overhead())
	if _, err := rand.Read(out); err != nil {
		return nil, err
	}
	out = b.aead.Seal(out, out[:nonceBytes], plain, []byte(providerID))
	if len(out) > store.MaxProviderSecretsBytes {
		return nil, errors.New("providers: secrets too large")
	}
	return out, nil
}

// Open decrypts the secrets sealed for providerID; ErrUnreadable when they
// do not open with this key or under this id.
func (b *Box) Open(providerID string, sealed []byte) (Secrets, error) {
	if b == nil || len(sealed) < nonceBytes+b.aead.Overhead() {
		return Secrets{}, ErrUnreadable
	}
	plain, err := b.aead.Open(nil, sealed[:nonceBytes], sealed[nonceBytes:], []byte(providerID))
	if err != nil {
		return Secrets{}, ErrUnreadable
	}
	defer clear(plain)
	var s Secrets
	if err := json.Unmarshal(plain, &s); err != nil {
		return Secrets{}, ErrUnreadable
	}
	return s, nil
}

// OpenProvider opens p's secrets and checks that they hold a value for
// each of p's variables, in p's order; ErrUnreadable otherwise.
func (b *Box) OpenProvider(p store.Provider) (Secrets, error) {
	s, err := b.Open(p.ID, p.Secrets)
	if err != nil {
		return Secrets{}, err
	}
	if len(s.Env) != len(p.EnvNames) {
		return Secrets{}, ErrUnreadable
	}
	for i, n := range p.EnvNames {
		if s.Env[i].Name != n {
			return Secrets{}, ErrUnreadable
		}
	}
	return s, nil
}

// Readable reports whether p's secrets open with this key. A nil Box
// opens nothing.
func (b *Box) Readable(p store.Provider) bool {
	if b == nil {
		return false
	}
	_, err := b.OpenProvider(p)
	return err == nil
}

// Var is one variable of a launch environment. Its value may be a secret.
type Var struct{ Name, Value string }

// The variables the API endpoint sets.
const (
	EnvBaseURL     = "ANTHROPIC_BASE_URL"
	EnvAPIKey      = "ANTHROPIC_API_KEY"
	EnvAuthToken   = "ANTHROPIC_AUTH_TOKEN"
	EnvOpusModel   = "ANTHROPIC_DEFAULT_OPUS_MODEL"
	EnvSonnetModel = "ANTHROPIC_DEFAULT_SONNET_MODEL"
	EnvHaikuModel  = "ANTHROPIC_DEFAULT_HAIKU_MODEL"
)

// LaunchEnv returns the environment of p with its opened secrets s: the
// endpoint's variables (base URL when set, the key under the scheme's
// name, then the alias models that are set), then the custom variables in
// p's order.
func LaunchEnv(p store.Provider, s Secrets) []Var {
	out := make([]Var, 0, 5+len(s.Env))
	if p.BaseURL != "" {
		out = append(out, Var{EnvBaseURL, p.BaseURL})
	}
	if p.Scheme == store.ProviderSchemeBearer {
		out = append(out, Var{EnvAuthToken, s.Key})
	} else {
		out = append(out, Var{EnvAPIKey, s.Key})
	}
	for _, m := range []Var{{EnvOpusModel, p.OpusModel}, {EnvSonnetModel, p.SonnetModel}, {EnvHaikuModel, p.HaikuModel}} {
		if m.Value != "" {
			out = append(out, m)
		}
	}
	for _, e := range s.Env {
		out = append(out, Var(e))
	}
	return out
}

// Last4 returns the last four characters of s (fewer when shorter).
func Last4(s string) string {
	r := []rune(s)
	return string(r[max(0, len(r)-4):])
}

// reservedEnv are the names a custom variable may not take: the worker's
// own and the endpoint's.
var reservedEnv = map[string]bool{
	"CLAUDE_CONFIG_DIR": true, "HOME": true, "PATH": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
	// The worker sets it itself when it starts the CLI with a provider.
	"CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST": true,
	EnvBaseURL:                             true, EnvAPIKey: true, EnvAuthToken: true, EnvOpusModel: true, EnvSonnetModel: true, EnvHaikuModel: true,
}

// CheckEnvName returns why name may not be a custom variable's name, or ""
// when it may: [A-Z_][A-Z0-9_]*, at most store.MaxProviderEnvNameBytes,
// neither CIRCUS_* nor a reserved name.
func CheckEnvName(name string) string {
	if name == "" {
		return "enter a variable name"
	}
	if len(name) > store.MaxProviderEnvNameBytes {
		return fmt.Sprintf("variable name is longer than %d bytes", store.MaxProviderEnvNameBytes)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c == '_' || 'A' <= c && c <= 'Z' || i > 0 && '0' <= c && c <= '9') {
			return "variable name must match [A-Z_][A-Z0-9_]*"
		}
	}
	if strings.HasPrefix(name, "CIRCUS_") || reservedEnv[name] {
		return "this variable is reserved"
	}
	return ""
}
