import type { LedgerEntry } from "@/api/types"

/** A directory of a ledger, with what it holds. */
export interface LedgerDir {
  /** Relative to the ledger root; empty for the root itself. */
  path: string
  /** The last segment; empty for the root. */
  name: string
  /** Subdirectories, by name. */
  dirs: LedgerDir[]
  /** Its own decisions, by number. */
  decisions: LedgerEntry[]
  /** Decisions in it and below. */
  count: number
}

/** Compares strings by code point, the same on every machine and language. */
const byCodePoint = (a: string, b: string) => (a < b ? -1 : a > b ? 1 : 0)

/**
 * The directory tree of a ledger's entries, whatever order they came in:
 * in each directory, subdirectories by name, then decisions by number.
 */
export function buildLedgerTree(entries: readonly LedgerEntry[]): LedgerDir {
  const root: LedgerDir = { path: "", name: "", dirs: [], decisions: [], count: 0 }
  const byPath = new Map<string, LedgerDir>([["", root]])
  const dirOf = (path: string): LedgerDir => {
    const known = byPath.get(path)
    if (known) return known
    const cut = path.lastIndexOf("/")
    const parent = dirOf(cut < 0 ? "" : path.slice(0, cut))
    const dir: LedgerDir = { path, name: path.slice(cut + 1), dirs: [], decisions: [], count: 0 }
    parent.dirs.push(dir)
    byPath.set(path, dir)
    return dir
  }
  for (const entry of entries) {
    dirOf(entry.dir).decisions.push(entry)
    for (let path = entry.dir; ; path = path.slice(0, Math.max(0, path.lastIndexOf("/")))) {
      dirOf(path).count++
      if (path === "") break
    }
  }
  for (const dir of byPath.values()) {
    dir.dirs.sort((a, b) => byCodePoint(a.name, b.name))
    dir.decisions.sort((a, b) => a.number - b.number)
  }
  return root
}

/**
 * Whether a decision matches what was typed in the filter: its number
 * ("12", "D12"; a prefix matches too) or a part of its title, ignoring case.
 * An empty filter matches everything.
 */
export function matchesFilter(entry: Pick<LedgerEntry, "number" | "title">, filter: string): boolean {
  const q = filter.trim().toLowerCase()
  if (q === "") return true
  if (/^d?[0-9]+$/.test(q) && String(entry.number).startsWith(q.replace(/^d/, ""))) return true
  return entry.title.toLowerCase().includes(q)
}

/** A row of the tree as shown: a directory or a decision. */
export type LedgerTreeRow =
  | { kind: "dir"; dir: LedgerDir; level: number; expanded: boolean; parent: string | null; setSize: number; posInSet: number }
  | { kind: "decision"; entry: LedgerEntry; level: number; parent: string | null; setSize: number; posInSet: number }

/** The key of a row: "dir:<path>" or "D<number>". */
export const rowKey = (row: LedgerTreeRow) => (row.kind === "dir" ? `dir:${row.dir.path}` : `D${row.entry.number}`)

/**
 * The rows the tree shows, top to bottom: in each directory its
 * subdirectories, then its decisions. Collapsed directories hide what they
 * hold. While a filter is set, only matching decisions show, with the
 * directories above them, all expanded. Levels start at 1 (the root's own
 * contents); `parent` is the key of the directory row above, null at the
 * top.
 */
export function visibleRows(
  root: LedgerDir,
  { collapsed, filter }: { collapsed: ReadonlySet<string>; filter: string },
): LedgerTreeRow[] {
  const filtering = filter.trim() !== ""
  const rows: LedgerTreeRow[] = []
  const hasMatch = (dir: LedgerDir): boolean =>
    dir.decisions.some((d) => matchesFilter(d, filter)) || dir.dirs.some(hasMatch)

  const walk = (dir: LedgerDir, level: number, parent: string | null) => {
    const dirs = filtering ? dir.dirs.filter(hasMatch) : dir.dirs
    const decisions = filtering ? dir.decisions.filter((d) => matchesFilter(d, filter)) : dir.decisions
    const setSize = dirs.length + decisions.length
    let pos = 0
    for (const sub of dirs) {
      const expanded = filtering || !collapsed.has(sub.path)
      rows.push({ kind: "dir", dir: sub, level, expanded, parent, setSize, posInSet: ++pos })
      if (expanded) walk(sub, level + 1, `dir:${sub.path}`)
    }
    for (const entry of decisions) rows.push({ kind: "decision", entry, level, parent, setSize, posInSet: ++pos })
  }
  walk(root, 1, null)
  return rows
}

/** The directory and every directory above it, without the root: "a/b" gives "a" and "a/b". */
export function ancestorDirs(dir: string): string[] {
  if (dir === "") return []
  const parts = dir.split("/")
  return parts.map((_, i) => parts.slice(0, i + 1).join("/"))
}

/** The directory part of a decision's path; empty at the root. */
export const dirOfPath = (path: string) => (path.includes("/") ? path.slice(0, path.lastIndexOf("/")) : "")

/** The first seven digits of a commit, as Git shows it. */
export const shortCommit = (commit: string) => commit.slice(0, 7)
