// Package auth implements machine credentials and human Web authentication:
// one-time enrollment tokens, the long-term bearer tokens they are exchanged
// for, authentication of link upgrade requests and revocation. Per-machine key
// pairs and challenge signatures are planned to replace this layer only;
// identities and their rows stay.
//
// # Tokens
//
// Every token, one-time or long-term, is TokenBytes random bytes encoded as
// unpadded base64url (TokenLen characters). The store keeps only HashToken,
// the SHA-256 of the token text; lookups go by hash and the hash read back is
// compared again in constant time. Machine tokens never appear in logs or
// error strings. The process-local human setup token is logged once at startup.
//
// # Identities and enrollment
//
// Identities are per machine × role: a daemon enrolls as a
// device identity (store.KindDevice), a worker as a worker identity
// (store.KindWorker), each with its own credential, linked to the machine by
// MachineID. Enroll runs one store transaction:
//
//  1. UseEnrollToken marks the one-time token used (exactly one of any number
//     of concurrent uses succeeds);
//  2. the token must list the requested kind;
//  3. a token issued to rejoin (CreateRejoinToken) replaces its identity:
//     if it is still live, the user's and offline, it is revoked as
//     RevokeByID revokes and the new identity takes its name;
//  4. CreateIdentity inserts the device or worker of the token's user
//     (names are unique per user and kind, ignoring ASCII case);
//  5. CreateCredential stores the hash of a fresh long-term token.
//
// Any failure rolls the whole transaction back, so a rejected role, a taken
// name or a refused rejoin does not burn the token and leaves the replaced
// identity in place: the caller retries, or picks another --name. Every
// token failure (unknown, used, expired, or issued for another role) is the
// single error ErrEnrollRejected, so a caller cannot tell them apart.
// The name check runs after the token check, so only a holder of a valid
// token learns whether a name is taken.
//
// The machine id, hostname, OS and the other identity fields are reported by
// the client itself and never verified: they serve audit and the device ↔
// worker link only, and must never be an input to an authorization decision.
// Which identity a rejoin replaces is decided when the token is issued, by
// the signed-in user or an administrator, never by what the machine reports.

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
// owner of a live connection re-check its credential after revocation.
// LastUsedAt is written at most once per TouchInterval per credential and
// process, not on every request.
//
// # Revocation
//
// RevokeByID (the Web UI's delete) and RevokeByName (the CLI's revoke)
// revoke an identity's credentials and soft-deletes the identity in one
// transaction, so the name can be enrolled again; a rejoin revokes the
// identity it replaces the same way.
//
// # Human authentication
//
// WebService adds password and optional OIDC login, a process-local setup
// token and durable cookie sessions. The single super user is enforced in
// the store transaction. OIDC discovery is lazy; state, nonce and PKCE bind
// the flow to a signed, expiring browser cookie. Verified UserInfo email may
// bind an identity to an existing human user only before that user has an
// identity at the issuer; issuer/subject bindings take precedence later. Session hashes share neither the machine
// credential table nor its authentication middleware.
//
// Future browser Connect services install WebService.Interceptor and use
// WebUser to require a signed-in user. RequireWeb supplies the equivalent
// check for plain HTTP handlers. Both reject cross-origin mutations.
package auth
