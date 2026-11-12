// Package controlplane assembles the circus control plane process (`circus
// server`): the HTTP mux, TLS, the per-process instance id, structured logs,
// health endpoints, enrollment and graceful shutdown. The link gateway and
// call router (packages gateway, calls, timers, bus) plug into it.
//
// # Lifecycle
//
//	srv, err := controlplane.New(ctx, st, cfg) // validates cfg, builds the mux
//	srv.Mount("/v1/link/daemon", srv.Authenticator().Require(gw, store.KindDevice))
//	srv.AddReadyCheck("gateway", gw.Ready)
//	srv.AddShutdownHook("gateway", gw.Shutdown)
//	err = srv.Run(ctx)                         // Start, wait for ctx, Shutdown
//
// Mount, AddReadyCheck and AddShutdownHook must be called before Start; they
// panic afterwards. The Server never closes the store: its owner does, after
// Run or Shutdown returns.
//
// # Transport security (outline 3.8, D43 item 7)
//
// Exactly one mode is required: TLSCert and TLSKey (the server terminates
// TLS), BehindProxy (a reverse proxy terminates TLS and forwards plaintext),
// or DevInsecure (plaintext for development, refused unless the listener is
// bound to a loopback address). BehindProxy on a non-loopback address logs a
// warning at Start: clients that bypass the proxy would send tokens in
// plaintext.
//
// # Endpoints mounted by New
//
//   - GET /healthz: 200 while the process serves requests.
//   - GET /readyz: 200 when the store answers a read and every ready check
//     passes; 503 otherwise and during shutdown. The body names the failed
//     checks; their errors go to the log only.
//   - EnrollService (Connect, /circus.v1.EnrollService/): exchanges a
//     one-time token for a long-term credential (package auth). Its
//     control_plane_url is Config.PublicURL, empty when unset.
//   - GET /v1/whoami: authenticated with the bearer token; returns the
//     principal's role and id as JSON. `circus doctor` uses it to check a
//     credential without opening a link.
//   - GET /join: a plain-text hint for a join link opened in a browser (the
//     token in the fragment never reaches the server).
//
// Integration mounts the link endpoints, /v1/link/daemon and
// /v1/link/worker, behind Authenticator().Require.
//
// # Logging
//
// Logs are slog JSON lines; every line carries instance_id (D11 item 8, D31
// item 3), a UUIDv7 chosen per process. Each request is logged once with
// method, path, status and duration. Tokens, request bodies and the
// Authorization header are never logged; client-controlled strings (path,
// machine id, name) are truncated.
//
// # Enrollment token purge
//
// Unused enrollment tokens past their expiry are deleted by a ticker every
// PurgeInterval (store.PurgeEnrollTokens). The purge is idempotent, so
// several instances may run it at once. When the claimed timer loop
// (package timers) lands, the purge moves onto it as an enroll_token_expiry
// timer, see purge.go.
package controlplane
