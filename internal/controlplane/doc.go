// Package controlplane hosts circus server: enrollment, authentication, HTTP,
// TLS, health checks, durable timers and the M2 call path.
//
// New builds enrollment and administrative HTTP endpoints, seeds a durable
// enroll_token_expiry timer, and composes the registry, call router, daemon
// gateway and worker call handler, using the SAME Authenticator instance for
// upgrades and live credential rechecks. Start launches HTTP and the timer
// runner; Run waits for termination and shuts down; Shutdown without Start
// releases what New built. The caller closes the store afterward.
//
// New mounts /v1/link/daemon and /v1/link/worker. The call plane's ONE
// shutdown step closes worker links, drains the daemon gateway, closes the router, then
// stops and joins timers with fixed 20%/60%/remaining sub-deadlines. Earlier
// steps donate unused time. Gateway drain must precede router closure to persist
// disconnects and accept final results; its join guarantee can exceed its ctx.
// Closing worker links only detaches their subscriptions; calls keep executing.
// Readiness requires a store read, the router's constant-time Healthy accessor
// and a running, healthy timer runner (including its first poll). Timers poll
// every second by default (Config.TimerPollInterval), retain at most 64 claims
// with 5s leases, and dispatch device_grace/call_watchdog to the router.
// Enrollment cleanup completes its claim, purges expired unused tokens, and
// rearms a ten-minute deadline in one transaction. Unknown
// kinds are logged and completed rather than reclaimed forever. Existing
// deadlines survive restarts; authentication enforces expiry before cleanup.
//
// Exactly one transport mode is required: TLSCert/TLSKey, BehindProxy, or
// loopback-only DevInsecure. BehindProxy on a non-loopback listener warns about
// plaintext credentials. /healthz reports HTTP liveness; /readyz returns 503
// during shutdown or when any readiness check fails. /v1/whoami authenticates
// either role for circus doctor. EnrollService exchanges one-time tokens;
// /join serves instructions (the URL fragment never reaches the server).
//
// Logs are JSON with a per-process UUIDv7 instance_id. Tokens, request bodies
// and Authorization headers are never logged. Host configuration uses flags,
// with package defaults for bounded internal capacities; see workerlink's
// package documentation and calls/DESIGN.md for ownership and memory limits.
package controlplane
