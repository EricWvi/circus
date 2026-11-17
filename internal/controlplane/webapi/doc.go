// Package webapi serves the user-facing Connect services of the Web UI that
// are not authentication: ProjectService and MachineService.
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
// Name, path and repository URL rules (rules.go) are the authority. They
// started as a port of the Web UI's checks in web/src/api/validation.ts;
// rules.go lists what the Web UI does not check yet.
package webapi
