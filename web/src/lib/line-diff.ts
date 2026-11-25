// Line diffs of a changed Ledger file against its base, bounded so that a
// file the agent made as large as the server allows (256 KiB) can't freeze
// the page: past a line count no diff is tried, a diff gives up after a
// while, and at most `MAX_DIFF_ROWS` rows are ever shown. Pure and free of
// the DOM and i18n, so it also runs in a worker (`line-diff.worker.ts`).

import { diffLines } from "diff"

/** One row of a diff; numbers are 1-based, in the base and in the changeset. */
export type DiffLine =
  | { kind: "context"; text: string; oldNo: number; newNo: number }
  | { kind: "removed"; text: string; oldNo: number }
  | { kind: "added"; text: string; newNo: number }
  /** Unchanged lines left out between hunks. */
  | { kind: "skipped"; count: number }
  /** Lines left out because the diff shows at most `MAX_DIFF_ROWS` rows. */
  | { kind: "more"; count: number }

/**
 * What a comparison of two versions shows: a line diff, or both versions
 * whole (removed, then added) when they have too many lines to compare
 * (`too_large`) or are too different to compare in time (`too_different`).
 * Either way at most `MAX_DIFF_ROWS` rows.
 */
export type DiffOutcome =
  | { kind: "diff"; lines: DiffLine[] }
  | { kind: "whole"; reason: "too_large" | "too_different"; lines: DiffLine[] }

/** Unchanged lines kept around each change. */
export const DIFF_CONTEXT = 3
/** The most rows a diff shows. */
export const MAX_DIFF_ROWS = 2_000
/** Versions with more lines than this (either one) aren't compared line by line. */
export const MAX_DIFF_LINES = 10_000
/** How long a diff may take before it gives up. */
const DIFF_TIMEOUT_MS = 500

export const splitLines = (value: string): string[] => {
  if (value === "") return []
  const lines = value.split("\n")
  if (lines.at(-1) === "") lines.pop()
  return lines
}

/**
 * A line diff of `base` against `changeset`, with unchanged runs longer
 * than twice the context folded, not capped. Null when the two are too
 * different to compare within `timeoutMs`.
 */
export function lineDiff(
  base: string,
  changeset: string,
  { context = DIFF_CONTEXT, timeoutMs = DIFF_TIMEOUT_MS }: { context?: number; timeoutMs?: number } = {},
): DiffLine[] | null {
  const parts = diffLines(base, changeset, { timeout: timeoutMs })
  if (!parts) return null
  const lines: DiffLine[] = []
  let oldNo = 1
  let newNo = 1
  for (const part of parts) {
    for (const text of splitLines(part.value)) {
      if (part.added) lines.push({ kind: "added", text, newNo: newNo++ })
      else if (part.removed) lines.push({ kind: "removed", text, oldNo: oldNo++ })
      else lines.push({ kind: "context", text, oldNo: oldNo++, newNo: newNo++ })
    }
  }
  return foldContext(lines, context)
}

/** Replaces unchanged lines farther than `context` from any change with a "skipped" mark. */
function foldContext(lines: DiffLine[], context: number): DiffLine[] {
  const near = new Array<boolean>(lines.length).fill(false)
  lines.forEach((line, i) => {
    if (line.kind === "context") return
    for (let j = Math.max(0, i - context); j <= Math.min(lines.length - 1, i + context); j++) near[j] = true
  })
  const out: DiffLine[] = []
  let skipped = 0
  lines.forEach((line, i) => {
    if (near[i]) {
      if (skipped > 0) out.push({ kind: "skipped", count: skipped })
      skipped = 0
      out.push(line)
    } else skipped++
  })
  if (skipped > 0) out.push({ kind: "skipped", count: skipped })
  return out
}

/** The first `max` rows, then a "more" mark counting the lines left out. */
export function capRows(lines: DiffLine[], max = MAX_DIFF_ROWS): DiffLine[] {
  if (lines.length <= max) return lines
  const rest = lines.slice(max)
  const count = rest.reduce((n, l) => n + (l.kind === "skipped" || l.kind === "more" ? l.count : 1), 0)
  return [...lines.slice(0, max), { kind: "more", count }]
}

/** Both versions whole, the base as removed and the changeset as added, each capped to half the rows. */
export function wholeVersions(base: string, changeset: string, max = MAX_DIFF_ROWS): DiffLine[] {
  const half = Math.floor(max / 2)
  const removed = splitLines(base).map((text, i): DiffLine => ({ kind: "removed", text, oldNo: i + 1 }))
  const added = splitLines(changeset).map((text, i): DiffLine => ({ kind: "added", text, newNo: i + 1 }))
  return [...capRows(removed, half), ...capRows(added, half)]
}

/** How long two versions are, in lines, without splitting them. */
const lineCount = (value: string) => {
  let n = value === "" || value.endsWith("\n") ? 0 : 1
  for (let i = value.indexOf("\n"); i >= 0; i = value.indexOf("\n", i + 1)) n++
  return n
}

/**
 * Compares two versions as far as the bounds allow: a capped line diff,
 * or both versions whole and capped. `diff` computes the line diff (here,
 * or in a worker).
 */
export async function compareVersions(
  base: string,
  changeset: string,
  diff: (base: string, changeset: string) => Promise<DiffLine[] | null> = async (b, c) => lineDiff(b, c),
): Promise<DiffOutcome> {
  if (lineCount(base) > MAX_DIFF_LINES || lineCount(changeset) > MAX_DIFF_LINES) {
    return { kind: "whole", reason: "too_large", lines: wholeVersions(base, changeset) }
  }
  let lines: DiffLine[] | null
  try {
    lines = await diff(base, changeset)
  } catch {
    // A diff that fails shows both versions, like one that gives up.
    lines = null
  }
  if (lines === null) return { kind: "whole", reason: "too_different", lines: wholeVersions(base, changeset) }
  return { kind: "diff", lines: capRows(lines) }
}
