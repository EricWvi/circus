# Circus Web UI

React and Vite. Run commands from the repository root after `npm ci`.

```sh
task web:dev     # dev server on http://localhost:5173
task web:check   # typecheck, lint and test
task web:build   # production build into web/dist
```

## Serving the built UI from the control plane

`task build:webui` builds `bin/circus` with the Web UI embedded in server mode.
The release container image includes the same UI. Set `public_url` to the
server's browser-facing origin for Origin checks and setup links.

The API prefixes `/circus.v1.`, `/auth/`, and `/v1/` are reserved for API
routes. Missing files under `/assets/` and missing paths with a file extension
return 404. Other missing paths serve `index.html` for client-side routing.

## Backends

`VITE_CIRCUS_BACKEND` chooses what the UI talks to. The choice is made in
`src/api/backend.ts`; pages only see the client interface.

| Value | Auth, projects, devices | Sessions |
| --- | --- | --- |
| `mock` (default) | In-browser mock | In-browser mock |
| `server` | The control plane, over Connect | In-browser mock, as the owner of the demo data |

The control plane doesn't serve sessions yet, so in server mode the mock
still simulates them and trusts the real sign-in. Its sessions belong to the
server's projects: it looks projects up, and records the last used location,
through the same projects client as the pages.

### Mock switches

In mock mode, `?mock=fresh` starts an uninitialized server (the setup link is
printed in the browser console), `?mock=reset` restores the demo data and
`?mock=nooidc` hides the OIDC button until the next load; they combine with
commas. The demo accounts are `eric@example.com` (super user) and
`dev@example.com`, both with password `circus`.

In server mode only `?mock=reset` does something (it resets the simulated
sessions); auth, projects and devices come from the server.

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

Run the control plane and the Web UI in separate terminals from the
repository root.

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
user. The token is valid only for that server run. Restart the server for a
new one. Then, in a third terminal, enroll the scripted worker for the Web
account you will use to create sessions.

```sh
task web:dev:worker USER_EMAIL=dev@example.com
```

Replace `dev@example.com` with the email you used during setup. The worker
reuses its enrollment in `.tmp/dev/config/worker.json`. To enroll it for a
different Web account, stop the worker and remove that file before rerunning
the task with the new email. To start with a fresh server as well, stop the
dev processes and remove the database `.tmp/dev/circus.db` and the
development CLI files under `.tmp/dev/config`, `.tmp/dev/state` and
`.tmp/dev/cache`.

Sessions need a project location on an enrolled device. `task cli -- <args>`
runs the CLI from source with `CIRCUS_HOME=.tmp/dev`, so its credentials,
daemon state and command output stay under `.tmp/dev` and never touch an
installed circus (see "Development CLI" in the repository README). Enroll a
local device for the same Web user and run its daemon in another terminal:

```sh
go run ./cmd/circus server enroll-token create --config web/dev.server.yaml \
  --user dev@example.com --role daemon --url http://127.0.0.1:8443 |
  task cli -- join --role daemon --name web-dev-device
task cli -- daemon
```

Alternatively, sign in as that user and use **Add device** on `/devices`.
The dev server is a development build, so the dialog prints only the join
and daemon lines; run them with `task cli --` in place of `circus`. Its link
points at `public_url`, the Vite origin, so the machine reaches the server
through the Vite proxy.

Create a project with a location on `web-dev-device` and enter an existing
absolute directory on that device. Create a session in the project and send
a message. The worker streams a Markdown reply quoting your input. The
following inputs exercise specific session behaviors.

| Input | Behavior |
| --- | --- |
| `/fake bash <command>` | Run Bash in the session's working directory. |
| `/fake read <path>` | Read a file on the device. Relative paths use the session's working directory. |
| `/fake approve <command>` | Request approval after denying Bash. An approval retries Bash in the active turn or starts a turn if idle. |
| `/fake slow <seconds>` | Stretch the reply to exercise Interrupt and NOW input. |
| `/fake think` | Emit a thinking block before the reply. |
| `/fake crash` | Drop all local sessions mid-turn. After two seconds, start a new boot. The server records the interrupted turn. |
| `/fake blip` | Disconnect for two seconds, resume the same lease and finish the turn. |
| `/fake break` | Report failed transcript mirroring. Use RecoverSession to accept the gap. |
| `/fake job <command>` | Start a Bash job on the device and deliver its completion notice. |
| `/fake error` | End the turn with an error. |

Bash and Read use the real daemon and its filesystem. Stop the worker,
daemon, Vite and server with Ctrl-C in their terminals when finished.
The worker can take up to five seconds to exit when the control plane is
unreachable, while it tries to flush events and release session leases.

How the pieces fit:

- Vite proxies `/circus.v1.*` (Connect RPCs), `/auth/*` (OIDC redirects) and
  `/v1/*` (machine links, including WebSockets) to `VITE_CIRCUS_SERVER`
  (default `http://127.0.0.1:8443`). The proxy keeps
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

## Validation rules

`src/api/validation.ts` and `src/api/git-url.ts` port the control plane's
rules for project names, paths and repository URLs
(`internal/controlplane/webapi/rules.go`), so forms can refuse what the
server would. Two fixtures are generated from the Go side by
`TestWebParityFixture` (`internal/controlplane/webapi/parity_test.go`):

- `src/api/validation-parity.json`: what the Go rules return for a list of
  inputs (those of `rules_test.go` and more); `validation-parity.test.ts`
  runs them against the port.
- `src/api/case-fold.json`: Go's simple case-folding orbits, which project
  name uniqueness uses (`sameProjectName`).

The Go test fails whenever the rules (or Go's Unicode tables) no longer
match the fixtures. Regenerate them, then run the Web tests to see whether
the port still agrees:

```sh
go test ./internal/controlplane/webapi -run TestWebParityFixture -update
npm -w @circus/web run test
```

New inputs go into `webParityInputs` in `parity_test.go`.

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

## Fonts

- UI text is Geist Mono; Chinese falls through to the platform's Simplified
  Chinese UI font (PingFang SC / Hiragino Sans GB, Microsoft YaHei, Noto Sans
  CJK SC / Source Han Sans SC), named in the stacks in `src/index.css`.
- Code (`font-mono`, `pre`, `code`) uses `--code-font-family` and
  `--code-font-size`, which `src/settings/code-font.ts` sets on the root
  element from the per-browser setting (`localStorage` key
  `circus-code-font`) before the first render. The default is each
  platform's own code font. People change both on `/settings`.

## Generated protocol code

The UI imports the generated messages and services as
`@circus/proto/circus/v1/<file>_pb`. Vite (`vite.config.ts`) and TypeScript
(`tsconfig.app.json`) resolve those imports to the sources in `gen/ts/src`, so
`gen/ts` needs no build step before the UI builds, tests or runs.
