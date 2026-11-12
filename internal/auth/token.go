package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"

	"github.com/google/uuid"
)

const (
	// TokenBytes is the entropy of every token.
	TokenBytes = 32
	// TokenLen is the length of a token's text form (unpadded base64url).
	TokenLen = 43
)

// NewToken returns a fresh random token in its text form.
func NewToken() (string, error) {
	var b [TokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("auth: generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// WellFormed reports whether s has the shape of a token: TokenLen
// characters of unpadded base64url decoding to TokenBytes bytes. It lets a
// client catch a truncated copy and a server skip the store for garbage.
func WellFormed(s string) bool {
	if len(s) != TokenLen {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	return err == nil && len(b) == TokenBytes
}

// HashToken returns the SHA-256 of the token text, the only form the store
// keeps.
func HashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// sameHash compares two hashes in constant time.
func sameHash(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }

// newID returns a UUIDv7, the id of every row this package creates (D11
// item 5).
func newID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("auth: generate id: %w", err)
	}
	return id.String(), nil
}
