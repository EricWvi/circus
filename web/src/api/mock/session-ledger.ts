// The mock's session ledger snapshots: per session, the open changeset's
// changed files next to their base versions, its last collection's
// problems, and the changesets merged so far. In this tab's memory only,
// like the sessions. A merge adds a commit to the project's main line
// (`ledger.ts`), so the project's Ledger page shows the new decisions.

import { ApiError } from "@/api/errors"
import type {
  LedgerChangeset,
  LedgerProblem,
  MergedSessionChangeset,
  SessionLedger,
  SessionLedgerDiff,
  SessionLedgerEntry,
  SessionLedgerStatus,
} from "@/api/types"
import { fakeCommitId } from "@/mock/ledger"

import { decisionPathOf, ledgerOf, type MockDecisionState, type MockLedgerCommit } from "./ledger"

/** A decision as a changed file holds it; `number` is "D12" or a temporary "D?a". */
export interface MockChangedDecision {
  number: string
  title: string
  dir: string
  body: string
}

/** One changed file of an open changeset. */
export interface MockLedgerChange {
  entry: SessionLedgerEntry
  /** The file in the base and in the changeset; null where it doesn't exist. */
  base: string | null
  content: string | null
  /** The decision the changeset holds, for a decision file that isn't deleted. */
  decision?: MockChangedDecision
}

export interface MockSessionLedger {
  status: Omit<SessionLedgerStatus, "changeCount" | "problemCount">
  /** The open changeset, with its changes; null when none. */
  open: {
    changeset: LedgerChangeset
    changes: MockLedgerChange[]
    /** Problems of the whole snapshot (no path). */
    snapshotProblems: LedgerProblem[]
    warnings: LedgerProblem[]
    fastForward: boolean
  } | null
  merged: MergedSessionChangeset[]
}

/** The type of a refused merge or delete, as the server names it. */
const refuse = (type: string, message: string) =>
  new ApiError("failed_precondition", message, undefined, { preconditionType: type })

/** A decision file's content: front matter with number and title, then the body. */
export function decisionFile(d: { number: string; title: string; body: string }): string {
  return `---\nnumber: ${d.number.slice(1)}\ntitle: ${JSON.stringify(d.title)}\n---\n\n${d.body}`
}

const pathOf = (d: MockChangedDecision) => {
  const numeric = /^D[0-9]+$/.test(d.number)
  // A temporary number keeps its "?" in the file name, as on a device.
  const file = numeric
    ? decisionPathOf({ number: Number(d.number.slice(1)), title: d.title, dir: d.dir }).split("/").at(-1)!
    : decisionPathOf({ number: 0, title: d.title, dir: "" }).replace(/^D0/, d.number)
  return d.dir === "" ? file : `${d.dir}/${file}`
}

const stateOf = (d: MockDecisionState): MockChangedDecision => ({
  number: `D${d.number}`,
  title: d.title,
  dir: d.dir,
  body: d.body,
})

/** What changed between a base decision (or none) and the changeset's. */
function decisionChange(base: MockChangedDecision | null, next: MockChangedDecision, problems: string[] = []): MockLedgerChange {
  const path = pathOf(next)
  const basePath = base ? pathOf(base) : ""
  return {
    entry: {
      path,
      basePath: base && basePath !== path ? basePath : "",
      number: next.number,
      title: next.title,
      baseTitle: base && base.title !== next.title ? base.title : "",
      added: base === null,
      contentChanged: base !== null && base.body !== next.body,
      moved: base !== null && base.dir !== next.dir,
      retitled: base !== null && base.title !== next.title,
      deleted: false,
      problems,
    },
    base: base && decisionFile(base),
    content: decisionFile(next),
    decision: next,
  }
}

/** What a collection says of a file that isn't a decision: a ledger holds only decision files. */
export const NOT_A_DECISION = "only decision files (D<number>-<name>.md) belong in the ledger"

/** A file that isn't a decision, added or changed: always a problem, which blocks the merge. */
function fileChange(path: string, base: string | null, content: string): MockLedgerChange {
  return {
    entry: {
      path,
      basePath: "",
      number: "",
      title: "",
      baseTitle: "",
      added: base === null,
      contentChanged: base !== null,
      moved: false,
      retitled: false,
      deleted: false,
      problems: [NOT_A_DECISION],
    },
    base,
    content,
  }
}

/** Strictly increasing status times, as the server keeps them. */
function touch(ledger: MockSessionLedger, now: number) {
  ledger.status.updatedAtUnixMs = Math.max(now, ledger.status.updatedAtUnixMs + 1)
}

/** The status as the session and GetSessionLedger show it. */
export function ledgerStatusOf(ledger: MockSessionLedger): SessionLedgerStatus {
  const open = ledger.open
  const problemCount = open
    ? open.snapshotProblems.length + open.changes.reduce((n, c) => n + c.entry.problems.length, 0)
    : 0
  return {
    ...ledger.status,
    changesetId: open?.changeset.changesetId ?? "",
    changeCount: open?.changes.length ?? 0,
    problemCount,
  }
}

/** A snapshot ready on the device, without changes. */
export function readyLedger(sessionId: string, now: number): MockSessionLedger {
  return {
    status: {
      state: "ready",
      path: `/home/eric/.local/state/circus/ledger/${sessionId}`,
      error: "unspecified",
      errorDetail: "",
      collectedAtUnixMs: 0,
      collectionDue: false,
      changesetId: "",
      updatedAtUnixMs: now,
      applying: "none",
    },
    open: null,
    merged: [],
  }
}

function openChangeset(sessionId: string, baseCommit: string, now: number): LedgerChangeset {
  return {
    changesetId: `cs-${sessionId}`,
    state: "open",
    baseCommit,
    createdAtUnixMs: now,
    updatedAtUnixMs: now,
    mergedCommit: "",
    discardReason: "",
  }
}

const MINUTE = 60_000

/**
 * The demo snapshots of the seeded sessions, from their projects' main
 * lines: one with changes of every kind that merges, one whose changes
 * have problems, one that waits for an integrator, one without changes and
 * one whose snapshot failed. Other sessions have none.
 */
export function seedSessionLedger(
  sessionId: string,
  mainLine: readonly MockLedgerCommit[],
  now: number,
): MockSessionLedger | undefined {
  const head = ledgerOf(mainLine)
  const current = (n: number): MockChangedDecision | null => {
    const entry = head.entries.find((e) => e.number === n)
    if (!entry) return null
    let state: MockDecisionState | undefined
    for (const c of mainLine) for (const d of c.changes) if (d.number === n) state = d
    return state ? stateOf(state) : null
  }
  const ledger = readyLedger(sessionId, now)
  const open = (changes: (MockLedgerChange | null)[], extra: Partial<NonNullable<MockSessionLedger["open"]>> = {}) => {
    ledger.status.collectedAtUnixMs = now - 4 * MINUTE
    ledger.open = {
      changeset: openChangeset(sessionId, head.headCommit, now - 30 * MINUTE),
      changes: changes.filter((c) => c !== null).sort((a, b) => (a.entry.path < b.entry.path ? -1 : 1)),
      snapshotProblems: [],
      warnings: [],
      fastForward: true,
      ...extra,
    }
  }
  const revise = (n: number, change: (d: MockChangedDecision) => Partial<MockChangedDecision>) => {
    const base = current(n)
    return base && decisionChange(base, { ...base, ...change(base) })
  }

  switch (sessionId) {
    case "web-vite":
      open(
        [
          decisionChange(null, {
            number: "D?a",
            title: "Show a session's Ledger changes next to its conversation",
            dir: "web",
            body: "A session's Ledger changes open in a drawer from the session's header.\n\nThey are merged from there once they fast-forward (see D10).\n",
          }),
          revise(10, (d) => ({ body: `${d.body}\nDecision diffs are shown as plain text too.\n` })),
          revise(3, () => ({ dir: "storage" })),
          revise(4, () => ({ title: "The Ledger is stored as a Git repository" })),
          revise(5, (d) => ({
            dir: "web",
            title: "Connect over JSON in the browser",
            body: d.body.replace("not gRPC-Web", "never gRPC-Web"),
          })),
        ],
        {
          warnings: [
            { path: "web/D?a-Show-a-session's-Ledger-changes-next-to-its-conversation.md", message: "body has no link to the code it decides on" },
          ],
        },
      )
      return ledger
    case "palace-new":
      open([
        decisionChange(
          null,
          {
            number: "D?a",
            title: "Health checks over HTTP",
            dir: "ops",
            body: "Every service answers `GET /healthz` with 200 once it can serve.\n\n<script>alert('not run')</script>\n",
          },
          ["front matter must hold number and title"],
        ),
        revise(1, (d) => ({ body: `${d.body}\nHealth check results are not stored.\n` })),
        fileChange("ops/notes.txt", null, "Ask ops which port the load balancer probes.\n"),
      ], {
        snapshotProblems: [{ path: "", message: "the snapshot holds more than 8192 entries; nothing was collected past them" }],
      })
      return ledger
    case "quic-spike":
      open(
        [
          decisionChange(null, {
            number: "D?a",
            title: "Keep WebSocket for the worker link",
            dir: "architecture",
            body: "QUIC saves a round trip on connect but needs UDP through every firewall; the worker link stays on WebSocket (D2).\n",
          }),
          revise(2, (d) => ({ body: `${d.body}\nQUIC was measured and not adopted.\n` })),
        ],
        { fastForward: false },
      )
      return ledger
    case "libvips":
      ledger.status.collectedAtUnixMs = now - 20 * MINUTE
      return ledger
    case "fix-auth-refresh":
      ledger.status = {
        ...ledger.status,
        state: "failed",
        path: "",
        error: "permission_denied",
        errorDetail: "mkdir /home/eric/.local/state/circus/ledger: permission denied",
      }
      return ledger
  }
  return undefined
}

/** The session's snapshot with its open changeset's changes. */
export function sessionLedgerOf(ledger: MockSessionLedger | undefined): SessionLedger {
  if (!ledger) return { entries: [], problems: [], warnings: [], fastForward: false, merged: [] }
  const open = ledger.open
  const problems: LedgerProblem[] = open
    ? [
        ...open.snapshotProblems,
        ...open.changes.flatMap((c) => c.entry.problems.map((message) => ({ path: c.entry.path, message }))),
      ]
    : []
  return {
    status: ledgerStatusOf(ledger),
    ...(open ? { changeset: open.changeset } : {}),
    entries: open?.changes.map((c) => c.entry) ?? [],
    problems,
    warnings: open?.warnings ?? [],
    fastForward: open?.fastForward ?? false,
    merged: ledger.merged,
  }
}

/** One changed file next to its base version. */
export function sessionLedgerDiffOf(ledger: MockSessionLedger | undefined, path: string): SessionLedgerDiff {
  const change = ledger?.open?.changes.find((c) => c.entry.path === path)
  if (!change) throw new ApiError("not_found", "The open changeset doesn't change that path.")
  return {
    baseExists: change.base !== null,
    base: change.base ?? "",
    baseTruncated: false,
    changesetExists: change.content !== null,
    changeset: change.content ?? "",
    changesetTruncated: false,
  }
}

/** Whether deleting the session would lose changes: an open changeset with changes, or a collection due. */
export function holdsLedgerChanges(ledger: MockSessionLedger | undefined): boolean {
  return !!ledger && ((ledger.open?.changes.length ?? 0) > 0 || ledger.status.collectionDue)
}

/** Refuses a delete that would lose changes; discards them when asked to. */
export function deleteSessionLedger(ledger: MockSessionLedger | undefined, discard: boolean, now: number) {
  if (!ledger) return
  if (holdsLedgerChanges(ledger) && !discard) {
    throw refuse("LEDGER_CHANGES", "The session's Ledger changes are not merged.")
  }
  if (ledger.open) {
    ledger.open.changeset = { ...ledger.open.changeset, state: "discarded", discardReason: "session deleted", updatedAtUnixMs: now }
    ledger.open = null
  }
}

/**
 * Merges the open changeset onto the main line, as MergeSessionLedger:
 * new decisions get the next numbers in path order. Returns the new
 * commit and the merged changeset.
 */
export function mergeSessionLedger(
  ledger: MockSessionLedger | undefined,
  req: { sessionId: string; expectedChangesetId: string; turnRunning: boolean; deviceOnline: boolean },
  mainLine: readonly MockLedgerCommit[],
  now: number,
): { commit: MockLedgerCommit; merged: MergedSessionChangeset } {
  const open = ledger?.open
  if (!ledger || !open || open.changes.length === 0) throw refuse("NO_CHANGES", "The session has no Ledger changes.")
  if (open.changeset.changesetId !== req.expectedChangesetId) {
    throw refuse("CHANGESET_CHANGED", "The open changeset is another one.")
  }
  if (req.turnRunning) throw refuse("TURN_RUNNING", "A turn is running.")
  if (!req.deviceOnline && ledger.status.collectionDue) {
    throw refuse("DEVICE_OFFLINE", "The device is offline and holds changes not collected yet.")
  }
  if (open.snapshotProblems.length > 0 || open.changes.some((c) => c.entry.problems.length > 0)) {
    throw refuse("PROBLEMS", "The changeset fails validation.")
  }
  if (!open.fastForward) throw refuse("NOT_FAST_FORWARD", "The changeset doesn't fast-forward.")

  let next = ledgerOf(mainLine).nextNumber
  const numbers: Record<string, number> = {}
  for (const c of open.changes) {
    if (c.decision && !/^D[0-9]+$/.test(c.decision.number)) numbers[c.decision.number] = next++
  }
  const commit: MockLedgerCommit = {
    commit: fakeCommitId(`${open.changeset.changesetId}:${now}`),
    atUnixMs: now,
    author: `session-${req.sessionId}`,
    summary: "Merge a session's Ledger changes",
    codeCommit: "",
    changes: open.changes.flatMap((c) => {
      const d = c.decision
      if (!d) return []
      const number = numbers[d.number] ?? Number(d.number.slice(1))
      // Temporary references in the body take the assigned numbers.
      const body = d.body.replace(/D\?[a-z]+\b/g, (ref) => (ref in numbers ? `D${numbers[ref]}` : ref))
      return [{ number, title: d.title, dir: d.dir, body }]
    }),
  }
  const merged: MergedSessionChangeset = {
    changeset: { ...open.changeset, state: "merged", mergedCommit: commit.commit, updatedAtUnixMs: now },
    numbers,
  }
  ledger.merged = [merged, ...ledger.merged]
  ledger.open = null
  touch(ledger, now)
  return { commit, merged }
}

/** The main line moved on: an open changeset based on an older head no longer fast-forwards. */
export function mainLineMoved(ledger: MockSessionLedger | undefined, head: string, now: number) {
  if (!ledger?.open || ledger.open.changeset.baseCommit === head || !ledger.open.fastForward) return false
  ledger.open.fastForward = false
  touch(ledger, now)
  return true
}

/**
 * A turn ended: the snapshot is collected. With `newDecision`, the agent
 * wrote a new decision in it during the turn, which the collection finds
 * (opening a changeset on `head` when none is open).
 */
export function collectSessionLedger(
  ledger: MockSessionLedger | undefined,
  req: { sessionId: string; head: string; newDecision?: { title: string; body: string } },
  now: number,
) {
  if (!ledger || ledger.status.state !== "ready") return false
  ledger.status.collectedAtUnixMs = now
  ledger.status.collectionDue = false
  if (req.newDecision) {
    ledger.open ??= {
      changeset: { ...openChangeset(req.sessionId, req.head, now), changesetId: `cs-${req.sessionId}-${now}` },
      changes: [],
      snapshotProblems: [],
      warnings: [],
      fastForward: true,
    }
    const used = new Set(ledger.open.changes.map((c) => c.entry.number))
    let letter = 0
    while (used.has(tempNumber(letter))) letter++
    ledger.open.changes.push(
      decisionChange(null, { number: tempNumber(letter), title: req.newDecision.title, dir: "", body: req.newDecision.body }),
    )
    ledger.open.changes.sort((a, b) => (a.entry.path < b.entry.path ? -1 : 1))
  }
  touch(ledger, now)
  return true
}

/** "D?a", "D?b", …, "D?z", "D?ba", … */
function tempNumber(index: number): string {
  let out = ""
  let i = index
  do {
    out = String.fromCharCode(97 + (i % 26)) + out
    i = Math.floor(i / 26)
  } while (i > 0)
  return `D?${out}`
}

/**
 * The open changeset is discarded, as an integrator may do: the snapshot
 * starts over from the main line's head, without changes. Returns the
 * discarded changeset's id, or null when none was open.
 */
export function discardSessionLedger(ledger: MockSessionLedger | undefined, now: number): string | null {
  const open = ledger?.open
  if (!ledger || !open) return null
  ledger.open = null
  ledger.status.collectionDue = false
  touch(ledger, now)
  return open.changeset.changesetId
}

/** Adds a new decision whose file is at the size limit (256 KiB), every line its own. */
export function addLargeDecision(ledger: MockSessionLedger) {
  const open = ledger.open
  if (!open) return
  const lines: string[] = []
  for (let i = 0, size = 0; size < 250 * 1024; i++) {
    const line = `- Measurement ${i}: ${(i * 7919) % 1000} ms`
    lines.push(line)
    size += line.length + 1
  }
  open.changes.push(
    decisionChange(null, { number: "D?z", title: "Benchmark log", dir: "stress", body: `${lines.join("\n")}\n` }),
  )
  open.changes.sort((a, b) => (a.entry.path < b.entry.path ? -1 : 1))
}
