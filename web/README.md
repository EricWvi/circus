# Circus Web UI

React and Vite. Run commands from the repository root after `npm ci`.

```sh
task web:dev     # dev server on http://localhost:5173
task web:check   # typecheck, lint and test
task web:build   # production build into web/dist
```

## Backends

`VITE_CIRCUS_BACKEND` chooses what the UI talks to. The choice is made in
`src/api/backend.ts`; pages only see the client interface.

| Value | Auth | Projects, devices, sessions |
| --- | --- | --- |
| `mock` (default) | In-browser mock | In-browser mock |
| `server` | The control plane, over Connect | In-browser mock, as the owner of the demo data |

The control plane serves only auth so far, so in server mode the mock still
stands in for everything else and trusts the real sign-in.

### Mock switches

In mock mode, `?mock=fresh` starts an uninitialized server (the setup link is
printed in the browser console), `?mock=reset` restores the demo data and
`?mock=nooidc` hides the OIDC button until the next load; they combine with
commas. The demo accounts are `eric@example.com` (super user) and
`dev@example.com`, both with password `circus`.

In server mode only `?mock=reset` does something; auth comes from the server.

## Running against the control plane

Two terminals, both in the repository root:

```sh
# Terminal 1: the control plane, with web/dev.server.yaml
# (127.0.0.1:8443, plaintext, database in .tmp/dev/circus.db).
task web:dev:server

# Terminal 2: the Web UI in server mode.
task web:dev BACKEND=server
```

On a database without a super user, the server logs a setup link (one JSON
line; the `url` field):

```
{"level":"INFO","msg":"*** Initial setup required: open the link to create the super user. ...","url":"http://localhost:5173/setup?token=...", ...}
```

The link already points at the Vite origin, because `public_url` in the dev
config is `http://localhost:5173`. Open it in the browser and create the super
user. The token is valid only for that server run; restart the server for a
new one. To start over, stop the server and delete `.tmp/dev/circus.db`.

How the pieces fit:

- Vite proxies `/circus.v1.*` (Connect RPCs) and `/auth/*` (OIDC redirects)
  to `VITE_CIRCUS_SERVER` (default `http://127.0.0.1:8443`). The proxy keeps
  the browser's `Host` and `Origin`, so the server's Origin check compares
  against `public_url`, the Vite origin.
- The session cookie has no `Domain` and the server redirects to relative
  paths, so both stay on the Vite origin.
- Open the UI as `http://localhost:5173`, not `127.0.0.1:5173`: another
  origin fails the server's Origin check.
- OIDC needs a provider that redirects to
  `http://localhost:5173/auth/oidc/callback`; see the commented `oidc` block in
  `web/dev.server.yaml`.

To point Vite at another control plane, set `VITE_CIRCUS_SERVER`, and make
sure that server's `public_url` is the Vite origin.

## Generated protocol code

The UI imports the generated messages and services as
`@circus/proto/circus/v1/<file>_pb`. Vite (`vite.config.ts`) and TypeScript
(`tsconfig.app.json`) resolve those imports to the sources in `gen/ts/src`, so
`gen/ts` needs no build step before the UI builds, tests or runs.
