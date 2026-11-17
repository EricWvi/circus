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

### Simulated session states

Development builds show a **Simulate** menu in the session header while
sessions are simulated: drop the connection for 10 seconds (reconnects with
1, 2, 4… s backoff; "Retry now" tries at once without moving that schedule),
kill the worker mid-turn, break the transcript, and take the session's device
offline. While the connection is down, the conversation and the session lists
stay as they were when it dropped (see `state/conversations-provider.tsx`). The connection outage and the device switch
live in memory and reset on reload; the browser's own offline mode shows the
offline banner. The seeded `Rewrite the dotfiles installer in Go` session is
interrupted (on an offline device) and `Migrate thumbnailer to libvips` is
broken.

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

## Translations

All UI text comes from the catalogs in `src/i18n/locales/`: English (`en.ts`,
the source and fallback) and Simplified Chinese (`zh-CN.ts`). Components use
`useTranslation()`; code outside React calls `i18n.t` from `src/i18n/i18n.ts`.

- Add a key to `en.ts` first. `t()` keys and their `{{placeholders}}` are typed
  from it, and `zh-CN.ts` must have exactly the same keys (`satisfies Catalog`
  fails otherwise). `src/i18n/catalog.test.ts` also compares placeholders.
- Plurals use `_one`/`_other` with `count`; Chinese reads only `_other`, but
  keeps `_one` too so the key sets stay equal. Links and emphasis inside a
  sentence go through `<Trans>` with named tags.
- Dates and relative times are formatted with `Intl` in the active language
  (`src/lib/time.ts`).
- The language follows the browser until someone picks one in the user menu
  (stored in `localStorage` as `circus-language`).
- `npm -w @circus/web run lint` rejects literal text in JSX and in these
  props: `placeholder`, `title`, `alt`, `label`, `aria-label`,
  `aria-description`, `aria-placeholder`, `aria-roledescription`,
  `aria-valuetext`, and the app's own `description`, `heading`, `reason` and
  `tooltip` (`eslint-plugin-i18next`, run by oxlint; see `.oxlintrc.json`).
  On native DOM tags the plugin checks only `placeholder`, `title`, `alt` and
  `aria-label`, and nothing on SVG tags; the other props in the list
  (`aria-description`, `aria-placeholder`, `aria-roledescription`,
  `aria-valuetext`, `label`, …) are checked on components only, so keep them
  translated on DOM tags by hand.
  Tests (`*.test.tsx`, `src/test/`) are exempt. The vendored `components/ui`
  and `components/ai-elements` are exempt too; their
  visible strings come from the catalog through `useTranslation` or `i18n.t`,
  never from English defaults.

## Generated protocol code

The UI imports the generated messages and services as
`@circus/proto/circus/v1/<file>_pb`. Vite (`vite.config.ts`) and TypeScript
(`tsconfig.app.json`) resolve those imports to the sources in `gen/ts/src`, so
`gen/ts` needs no build step before the UI builds, tests or runs.
