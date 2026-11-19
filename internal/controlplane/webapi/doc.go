// Package webapi serves the user-facing Connect services of the Web UI that
// are not authentication: ProjectService, MachineService and SessionService.
//
// Every call is authenticated by the browser session cookie through the
// interceptor of auth.WebService (the same one AuthService uses), which
// resolves the user and refuses an Origin other than the public URL; a call
// without a session is UNAUTHENTICATED. Machine routes keep their own
// Bearer credentials and never reach this package.
//
// Validation errors carry a google.rpc.BadRequest field violation whose
// field is the request path the Web UI shows the error at ("name",
// "locations[1].path", ...). Messages are short English text; the Web UI
// maps codes and fields to its own text.
//
// Which devices a Web user may see and bind is decided in one place,
// deviceOwner; see its comment for the current rule.
//
// Sessions (sessions.go, watch.go) belong to the Web user who owns their
// project; a session's user_id is the machine owner whose devices serve it
// (deviceOwner). Every session call resolves the session through a project
// of the caller (ownSession) and answers NOT_FOUND otherwise. Writes run
// in one store transaction with that check and any view event the control
// plane records; after commit they announce new durable events on the
// event bus (sessions.Bus) and hint the scheduler (sessions.Waker).
// WatchSession streams read durable events from the store and take live
// ones from the bus; see its comment for the no-gap rule and the bounds.
//
// Name, path and repository URL rules (rules.go) are the authority. The Web
// UI runs a port of them (web/src/api/validation.ts and git-url.ts) before
// sending; TestWebParityFixture (parity_test.go) keeps the two in step.
package webapi
