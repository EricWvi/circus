// Package auth implements the M2 token layer of control-plane identity
// (D22, D43 items 2, 7, 8): one-time enrollment tokens, the long-term bearer
// tokens they are exchanged for, authentication of link upgrade requests and
// revocation. D14's key pairs and challenge signatures later replace this
// layer only; identities and their rows stay.
//
// # Tokens
//
// Every token, one-time or long-term, is TokenBytes random bytes encoded as
// unpadded base64url (TokenLen characters). The store keeps only HashToken,
// the SHA-256 of the token text; lookups go by hash and the hash read back is
// compared again in constant time. Tokens never appear in logs or error
// strings.
//
// # Identities and enrollment
//
// Identities are per machine × role (D43 item 8): a daemon enrolls as a
// device identity (store.KindDevice), a worker as a worker identity
// (store.KindWorker), each with its own credential, linked to the machine by
// MachineID. Enroll runs one store transaction:
//
//  1. UseEnrollToken marks the one-time token used (exactly one of any number
//     of concurrent uses succeeds);
//  2. the token must list the requested kind;
//  3. CreateIdentity inserts the device or worker of the token's user (names
//     are unique per user and kind);
//  4. CreateCredential stores the hash of a fresh long-term token.
//
// Any failure rolls the whole transaction back, so a rejected role or a taken
// name does not burn the token: the caller retries with another --name. Every
// token failure (unknown, used, expired, or issued for another role) is the
// single error ErrEnrollRejected, so a caller cannot tell them apart (F20).
// The name check runs after the token check, so only a holder of a valid
// token learns whether a name is taken.
//
// The machine id, hostname, OS and the other identity fields are reported by
// the client itself and never verified: they serve audit and the device ↔
// worker link only, and must never be an input to an authorization decision.
//
// A token whose Kinds list both roles (the store allows it) still yields one
// identity per Enroll: the first Enroll consumes it. Tokens made by
// CreateEnrollToken always carry exactly one kind.
//
// # Authentication
//
// Authenticator.Authenticate checks "Authorization: Bearer <token>" on a link
// upgrade request and returns the Principal, ErrUnauthenticated (missing,
// malformed or unknown token: 401) or ErrRevoked (revoked credential or
// deleted identity: 403). Require wraps a handler with that check, so an
// unauthenticated request never reaches the WebSocket upgrade. Valid lets the
// owner of a live connection re-check its credential after revocation (F17).
// LastUsedAt is written at most once per TouchInterval per credential and
// process, not on every request.
//
// # Revocation
//
// Revoke and RevokeByName revoke an identity's credentials and soft-delete
// the identity in one transaction, so the name can be enrolled again (a
// re-join after revocation, or after a join whose credential file was never
// written).
package auth
