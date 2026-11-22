// The mock's ledgers: each project's main line as a list of commits, kept
// in this tab's memory only. A decision's state at any commit is found by
// replaying the commits up to it, which is all a read-only ledger needs.

import { ApiError } from "@/api/errors"
import type { Decision, DecisionVersion, Ledger, LedgerEntry } from "@/api/types"

/** A decision as one commit leaves it: the whole decision, not a diff. */
export interface MockDecisionState {
  number: number
  title: string
  dir: string
  body: string
}

/** One main-line commit. */
export interface MockLedgerCommit {
  /** Full hex id. */
  commit: string
  atUnixMs: number
  author: string
  summary: string
  /** Empty when no code commit was merged with it. */
  codeCommit: string
  /** The decisions it adds or changes. */
  changes: MockDecisionState[]
}

/** Every project's main line, oldest commit first, by project id. */
export type MockLedgers = Map<string, MockLedgerCommit[]>

const NAME_MAX = 60
/** Ledger bounds, as the server applies them. */
export const MAX_DECISIONS = 4096
export const MAX_PAGE_SIZE = 100
const DEFAULT_PAGE_SIZE = 50

/**
 * The file name a decision gets from its number and title: whitespace,
 * `/ \ : * ? " < > |` and control characters become "-", runs of "-"
 * collapse, "-" and "." are trimmed from both ends, and the name is cut to
 * 60 characters.
 */
export function decisionFileName(number: number, title: string): string {
  // oxlint-disable-next-line no-control-regex -- control characters are what it replaces
  const replaced = title.replace(/[\s/\\:*?"<>|\u0000-\u001f\u007f]/gu, "-").replace(/-+/g, "-")
  const trimmed = replaced.replace(/^[-.]+|[-.]+$/g, "")
  const name = [...trimmed].slice(0, NAME_MAX).join("")
  return `D${number}-${name}.md`
}

export const decisionPathOf = (d: Pick<MockDecisionState, "number" | "title" | "dir">) =>
  d.dir === "" ? decisionFileName(d.number, d.title) : `${d.dir}/${decisionFileName(d.number, d.title)}`

/** Byte order, as Go compares strings. */
function compareBytes(a: string, b: string): number {
  const x = new TextEncoder().encode(a)
  const y = new TextEncoder().encode(b)
  for (let i = 0; i < Math.min(x.length, y.length); i++) if (x[i] !== y[i]) return x[i] - y[i]
  return x.length - y.length
}

interface Replayed {
  state: MockDecisionState
  /** The last commit that changed it. */
  updatedAtUnixMs: number
}

/** The decisions after the commits up to and including `last` (all when -1). */
function replay(commits: readonly MockLedgerCommit[], last = commits.length - 1): Map<number, Replayed> {
  const decisions = new Map<number, Replayed>()
  for (const c of commits.slice(0, last + 1)) {
    for (const change of c.changes) decisions.set(change.number, { state: change, updatedAtUnixMs: c.atUnixMs })
  }
  return decisions
}

/** The ledger at the head of the main line. */
export function ledgerOf(commits: readonly MockLedgerCommit[]): Ledger {
  const decisions = [...replay(commits).values()]
  const entries: LedgerEntry[] = decisions
    .map(({ state, updatedAtUnixMs }) => ({
      number: state.number,
      title: state.title,
      dir: state.dir,
      path: decisionPathOf(state),
      updatedAtUnixMs,
    }))
    .sort((a, b) => compareBytes(a.dir, b.dir) || compareBytes(a.path, b.path))
  const highest = decisions.reduce((max, d) => Math.max(max, d.state.number), -1)
  return { headCommit: commits.at(-1)?.commit ?? "", entries, nextNumber: highest + 1 }
}

const notFound = () => new ApiError("not_found", "Decision not found.")
const invalid = (field: string) => new ApiError("invalid_argument", `Invalid ${field}.`, field)

/** One decision at the head, or at `commit`. */
export function decisionOf(commits: readonly MockLedgerCommit[], number: number, commit = ""): Decision {
  checkNumber(number)
  if (commit !== "" && !/^[0-9a-f]{40}$/.test(commit)) throw invalid("commit")
  const at = commit === "" ? commits.length - 1 : commits.findIndex((c) => c.commit === commit)
  if (at < 0) throw notFound()
  const found = replay(commits, at).get(number)
  if (!found) throw notFound()
  const { state } = found
  return {
    number: state.number,
    title: state.title,
    dir: state.dir,
    path: decisionPathOf(state),
    body: state.body,
    commit: commits[at].commit,
    committedAtUnixMs: commits[at].atUnixMs,
  }
}

function checkNumber(number: number) {
  if (!Number.isInteger(number) || number < 0 || number > 0xffffffff) throw invalid("number")
}

/** The commits that changed a decision, newest first, a page at a time. */
export function historyOf(
  commits: readonly MockLedgerCommit[],
  number: number,
  pageSize = 0,
  pageToken = "",
): { versions: DecisionVersion[]; nextPageToken: string } {
  checkNumber(number)
  if (!Number.isInteger(pageSize) || pageSize < 0 || pageSize > MAX_PAGE_SIZE) throw invalid("page_size")
  const size = pageSize === 0 ? DEFAULT_PAGE_SIZE : pageSize
  const offset = pageToken === "" ? 0 : Number(/^o([0-9]+)$/.exec(pageToken)?.[1] ?? NaN)
  if (!Number.isSafeInteger(offset)) throw invalid("page_token")

  const versions: DecisionVersion[] = []
  let previous: MockDecisionState | undefined
  for (const c of commits) {
    const state = c.changes.find((d) => d.number === number)
    if (!state) continue
    versions.push({
      commit: c.commit,
      committedAtUnixMs: c.atUnixMs,
      author: c.author,
      summary: c.summary,
      path: decisionPathOf(state),
      title: state.title,
      added: previous === undefined,
      contentChanged: previous !== undefined && previous.body !== state.body,
      moved: previous !== undefined && previous.dir !== state.dir,
      retitled: previous !== undefined && previous.title !== state.title,
      codeCommit: c.codeCommit,
    })
    previous = state
  }
  if (versions.length === 0) throw notFound()
  versions.reverse()
  const page = versions.slice(offset, offset + size)
  const next = offset + size < versions.length ? `o${offset + size}` : ""
  return { versions: page, nextPageToken: next }
}
