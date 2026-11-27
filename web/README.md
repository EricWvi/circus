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

| Value | Serves |
| --- | --- |
| `mock` (default) | Everything from an in-browser mock |
| `server` | Everything from the control plane, over Connect |

Both implement the same client interface (`src/api/types.ts`), and the
mock's sessions emit the same raw session events as the server, so the
transcript reducer (`src/lib/session-events/`) and the pages run one code
path either way.

### Mock switches

In mock mode, `?mock=fresh` starts an uninitialized server (the setup link is
printed in the browser console), `?mock=reset` restores the demo data and
`?mock=nooidc` hides the OIDC button until the next load; they combine with
commas. `?mock=queue` adds sessions that wait, one for each queue reason, and
`?mock=agents` adds workers with Claude Code in every state an agent can be
in (for the agents on `/devices`), and `?mock=noagents` takes Claude Code off
every worker; these last until the next load. The demo accounts are `eric@example.com` (super user) and
`dev@example.com`, both with password `circus`.

The folder picker of a location's path browses a fake folder tree, the same
on every mock device (`src/api/mock/directories.ts`): a home folder with
projects, git repositories, hidden, linked and unreadable folders, an empty
one and `many-folders` (more than a listing keeps). Folders under `/mnt`
misbehave: `slow` answers after two seconds, `stale-nfs` times out,
`busy`, `unreachable` and `hung` fail the request as a busy, unreachable
(or too old) and silent device. Offline devices fail as offline.
Its git repositories have origin URLs for reading a project's repository
URL from a location's folder: `~/src/palace` has no origin and
`~/src/toolbox` answers after 1.5 seconds.

### Mock workers and agents

Workers live in the tab's memory. The demo workers all have Claude Code
installed (the offline `eric-win` as it last reported). Installs and
uninstalls asked for on `/devices` play out as a worker would do them
(`src/api/mock/agents.ts`): an online worker downloads with progress reports
once a second and then has it installed, or uninstalls it in a couple of
seconds; an offline one keeps the wish. The mock refuses what the server
refuses: an install on a worker that never reported its agents, or that
reported without Claude Code (`fresh-worker` and `legacy-runner` in
`?mock=agents`, whose rows offer no Install), and an uninstall while the
worker holds a session's lease (`devbox` in `?mock=queue`). A session waits
with the reason `agent_not_installed` while no online worker that may run
it has Claude Code installed; one in a location on a device whose machine
has a worker runs only on that worker.

### Mock sessions

Projects, devices and accounts persist in `localStorage`; sessions live in
the tab's memory only (`src/api/mock/sessions.ts`), and each page load
starts with the demo sessions of the demo projects that still exist. One of
them waits for an approval (`Fix flaky auth token refresh tests`), one was
interrupted (`Rewrite the dotfiles installer in Go`), one is broken
(`Migrate thumbnailer to libvips`), one is stopped and in one the user
stopped a running Bash call (`Add a health check endpoint`). Every message gets a
short scripted reply: streamed text, a Bash call and more text. A message
containing "approve" asks for approval to run its Bash call instead, and
runs it once approved. A message containing "401" fails its turn the way
Claude Code does when the provider rejects the key: its own "Failed to
authenticate" text and an error result. A new session keeps the permission
mode it was created with (`auto` when none); the mock accepts any mode, as
the control plane does. The session header shows it as the start mode:
Claude can leave it during the session (after a plan is approved, say),
and no event tells the view. Stopping the turn, stopping the session and
continuing a broken session work as on the server. For anything more
(reconnects, a lost worker, broken transcripts on demand), run against the
control plane with the scripted worker below.

A session started in a project's default location (an empty path) waits
for that folder on its device (`src/api/mock/workspaces.ts`, in this tab's
memory like the sessions): it is pending, preparing a second later, and
ready (cloned, or created without a repository URL) a couple of seconds
after that; then the session's messages get their replies. The project's
repository URL picks other outcomes: one containing `offline` first waits
for the device for a few seconds, `outdated` waits for circus to be updated
for good, `slow` clones for half a minute (in the background after a few
seconds), and `not-empty`, `mismatch`, `clone-fails` and `no-space` fail
the first attempt that way (Retry then succeeds). With `remove-fails`,
deleting the folder along with the project fails. An offline device
(`eric-win`) waits until it is online; a Windows device that is online
fails as unsupported.

### Mock ledgers

Ledgers live in the tab's memory too (`src/api/mock/ledger.ts`, demo data in
`src/mock/ledger.ts`), rebuilt on every page load. `circus` has sixteen
decisions in a few directories (one nested), with history: one decision
edited many times (its history pages), one moved, one retitled, some with a
code commit. Its bodies use lists, tables, code, links (absolute, relative
and `mailto:`), raw HTML, an image, and references to existing and missing
decisions and to `palace#D1`. `palace` has two decisions and `toolbox` none.
`?mock=stress` adds two decisions to `palace` for that page load: `Layout
torture` (long words, inline code, headings, links, deep lists and quotes,
footnotes) and `Large body` (about 250 KiB with some 25,000 references).

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
and daemon lines, already as `task cli --` commands; run them from the
repository root. A name typed in the dialog goes to `join --name`. Its link
points at `public_url`, the Vite origin, so the machine reaches the server
through the Vite proxy.

Create a project with a location on `web-dev-device` and enter an existing
absolute directory on that device. Send a message from the project page:
it creates a session and opens it. The worker streams a Markdown reply
quoting your input. The following inputs exercise specific session
behaviors.

| Input | Behavior |
| --- | --- |
| `/fake bash <command>` | Run Bash in the session's working directory. |
| `/fake read <path>` | Read a file on the device. Relative paths use the session's working directory. |
| `/fake approve <command>` | Request approval after denying Bash. An approval retries Bash in the active turn or starts a turn if idle. |
| `/fake ask` | Ask two questions (one multi-select) and end the turn. Answering them starts the next turn, which quotes the answers. |
| `/fake slow <seconds>` | Stretch the reply to exercise Interrupt and NOW input. |
| `/fake think` | Emit a thinking block before the reply. |
| `/fake crash` | Drop all local sessions mid-turn. After two seconds, start a new boot. The server records the interrupted turn. |
| `/fake blip` | Disconnect for two seconds, resume the same lease and finish the turn. |
| `/fake break` | Report failed transcript mirroring. Use RecoverSession to accept the gap. |
| `/fake job <command>` | Start a Bash job on the device and deliver its completion notice. |
| `/fake large <bytes>` | Answer a Bash use with that much output without running it (over 256 KiB, the view stores the result truncated). |
| `/fake parallel <command>` | Run Bash twice, as two tool uses of one model response. |
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

## Following a session

- The open session, and only it, is watched with `WatchSession` from the
  last durable event applied (`src/state/session-watch.ts`). A stream that
  ends or fails (including the server's hourly end of every stream) is
  reopened: at once when it had served for a while, otherwise on the
  backoff schedule of `src/state/connection.ts`. The reconnect banner, and
  disabled sending and approvals, wait until a loss lasts two seconds. Text that was
  streaming when the stream broke is dropped; the durable events replace
  it. A deleted session stops the watch, and a request rejected as
  unauthenticated, from any call, signs the user out (`src/api/unauthenticated.ts`).
- `src/lib/session-events/transcript.ts` turns the raw events into the view
  model of `src/lib/types.ts`. Its tests, and some of the session view's,
  replay what the scripted worker's scenarios stream from a real control
  plane (`fake-worker.fixture.json`, read through
  `src/test/fake-worker-events.ts`). `TestWebEventFixture`
  (`internal/testutil/fakeworker/webfixture_test.go`) records it in process,
  with a real daemon running the tools, and fails when the fake worker or
  the control plane no longer stream what the fixture holds. Regenerate it,
  then run the Web tests against it:

  ```sh
  go test ./internal/testutil/fakeworker -run TestWebEventFixture -update
  npm -w @circus/web run test
  ```

  New scenarios go into `webScenarios` in that file.
- Sent messages live in an outbox above the pages (`src/state/outbox.ts`)
  until the stream shows them: sending, then sent once the server accepted
  them, and no mark once the worker handed them to the agent. A failed
  message stays with its reason and a retry, which reuses its client
  message id and text, so the server never takes it twice.
- A turn whose RESULT is an error (anything but a success without
  `is_error`, and not aborted: a user stop's RESULT is an error too, and
  its TURN_STOPPED says what happened) shows as failed: the CLI's own error
  text, when it is the turn's last text, in error styling, then a failure
  line with the RESULT's errors (never the CLI's `[…_diagnostic]` lines),
  unless an ERROR of the turn already said the same. A stop or
  interruption of the same turn recorded after it takes it back. The header
  says "Turn failed" while the session is idle and its last turn outcome is
  an error (`viewState`; queued, running and an approval win). When the
  stream shows nothing of that failed turn (its RESULT too large to keep, a
  worker-side abort), the view adds the failure line itself
  (`showsTurnFailure`), so the header and the conversation agree.
- Session lists refetch every five seconds while the tab is visible; the
  open session's state is read again at turn and approval boundaries. The
  stream decides whether a turn runs, except that a stopped or broken
  session runs nothing and a turn the server already counts as over is over
  even when its RESULT never came (`turnRunning` in `src/lib/session-state.ts`).

## Session Ledger drawer

The control plane runs one read of a session's Ledger (the snapshot or a
file's diff) per user at a time and refuses another meanwhile as busy
(`resource_exhausted` with the ErrorInfo reason `LEDGER_BUSY`). The drawer
sends them one after another (`oneSessionLedgerRead` in
`src/api/hooks.ts`), gives up on one that takes over 35 seconds (aborting
it, so the next goes), and retries only the busy reply (`isLedgerBusy`,
which reads the reason, never the message), up to six times with backoff;
other refusals are errors at once. The snapshot is read again on a
KIND_LEDGER event and when
the session shows a newer version; a merge doesn't read it at once, since
the server is still applying it; its numbers stay in the footer until the
snapshot holds a new open changeset. A diff belongs to one version of the
snapshot, so it is read again only after the snapshot's new version is in.

## Ledger page

`/p/:projectId/ledger` shows a project's Ledger read-only:
`/p/:projectId/ledger/D12` one decision, `?view=history` its versions and
`?at=<commit>` the version at that commit. Bodies are written by agents and
rendered as untrusted Markdown (`src/components/ledger/ledger-markdown.tsx`,
rules in `src/lib/ledger-markdown.ts`): raw HTML shows as text, only
`http:`, `https:` and `mailto:` links are links (in a new tab), relative
links are plain text and images are linked, never loaded. `D12` and
`<project name>#D12` outside code become links to those decisions, with a
preview on hover and keyboard focus (`src/lib/ledger-refs.ts`). A body may
hold tens of thousands of references, so each is a plain anchor resolved in
constant time against sets of decision numbers worked out once per ledger
(`use-ref-resolver.ts`), and one layer around the body handles clicks and
previews for all of them (`decision-ref.tsx`). The tree sorts by itself,
whatever order the server lists: subdirectories by name, then decisions by
number.

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
