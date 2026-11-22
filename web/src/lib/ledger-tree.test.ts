import { describe, expect, it } from "vitest"

import type { LedgerEntry } from "@/api/types"

import {
  ancestorDirs,
  buildLedgerTree,
  dirOfPath,
  matchesFilter,
  rowKey,
  shortCommit,
  visibleRows,
  type LedgerDir,
} from "./ledger-tree"

const entry = (number: number, dir: string, title: string): LedgerEntry => ({
  number,
  title,
  dir,
  path: `${dir ? `${dir}/` : ""}D${number}-${title.replace(/\s+/g, "-")}.md`,
  updatedAtUnixMs: 0,
})

// In the server's order: by directory, then file name.
const entries = [
  entry(0, "", "Conventions"),
  entry(1, "arch", "Go control plane"),
  entry(10, "arch", "Bounds"),
  entry(3, "arch/storage", "SQLite"),
  entry(4, "arch/storage/deep", "Git 仓库"),
  entry(6, "session", "Worker 取舍"),
]

/** The tree as nested names, for comparing shapes. */
const shape = (dir: LedgerDir): unknown => ({
  [`${dir.name || "/"} (${dir.count})`]: [...dir.dirs.map(shape), ...dir.decisions.map((d) => `D${d.number}`)],
})

describe("buildLedgerTree", () => {
  it("nests directories", () => {
    expect(shape(buildLedgerTree(entries))).toEqual({
      "/ (6)": [
        { "arch (4)": [{ "storage (2)": [{ "deep (1)": ["D4"] }, "D3"] }, "D1", "D10"] },
        { "session (1)": ["D6"] },
        "D0",
      ],
    })
  })

  it("sorts by itself: subdirectories by name, then decisions by number", () => {
    const shuffled = [entries[5], entries[2], entries[4], entries[0], entries[3], entries[1], entry(30, "arch", "Thirty"), entry(2, "Arch", "Upper")]
    expect(shape(buildLedgerTree(shuffled))).toEqual({
      "/ (8)": [
        { "Arch (1)": ["D2"] },
        { "arch (5)": [{ "storage (2)": [{ "deep (1)": ["D4"] }, "D3"] }, "D1", "D10", "D30"] },
        { "session (1)": ["D6"] },
        "D0",
      ],
    })
  })

  it("creates directories that only hold directories", () => {
    const root = buildLedgerTree([entry(5, "a/b/c", "Deep")])
    expect(root.dirs[0].path).toBe("a")
    expect(root.dirs[0].decisions).toEqual([])
    expect(root.dirs[0].dirs[0].dirs[0].path).toBe("a/b/c")
    expect(root.dirs[0].count).toBe(1)
  })

  it("is empty for an empty ledger", () => {
    expect(buildLedgerTree([])).toEqual({ path: "", name: "", dirs: [], decisions: [], count: 0 })
  })
})

describe("matchesFilter", () => {
  const d12 = { number: 12, title: "Render Markdown as untrusted input" }
  it("matches numbers with or without D, by prefix", () => {
    for (const q of ["12", "D12", "d12", "1", "D1", " 12 "]) expect({ q, hit: matchesFilter(d12, q) }).toEqual({ q, hit: true })
    for (const q of ["2", "D2", "123", "D0"]) expect({ q, hit: matchesFilter(d12, q) }).toEqual({ q, hit: false })
  })
  it("matches a part of the title, ignoring case", () => {
    expect(matchesFilter(d12, "markdown")).toBe(true)
    expect(matchesFilter(d12, "UNTRUSTED in")).toBe(true)
    expect(matchesFilter(d12, "html")).toBe(false)
    expect(matchesFilter({ number: 4, title: "Ledger 以 Git 仓库存储" }, "仓库")).toBe(true)
  })
  it("matches everything when empty", () => {
    expect(matchesFilter(d12, "  ")).toBe(true)
  })
})

describe("visibleRows", () => {
  const root = buildLedgerTree(entries)
  const keys = (rows: ReturnType<typeof visibleRows>) => rows.map((r) => `${"  ".repeat(r.level - 1)}${rowKey(r)}`)

  it("lists directories before decisions, with levels and positions", () => {
    const rows = visibleRows(root, { collapsed: new Set(), filter: "" })
    expect(keys(rows)).toEqual([
      "dir:arch",
      "  dir:arch/storage",
      "    dir:arch/storage/deep",
      "      D4",
      "    D3",
      "  D1",
      "  D10",
      "dir:session",
      "  D6",
      "D0",
    ])
    const d10 = rows.find((r) => rowKey(r) === "D10")
    expect(d10).toMatchObject({ level: 2, parent: "dir:arch", setSize: 3, posInSet: 3 })
    expect(rows[0]).toMatchObject({ kind: "dir", expanded: true, parent: null, setSize: 3, posInSet: 1 })
  })

  it("hides what collapsed directories hold", () => {
    const rows = visibleRows(root, { collapsed: new Set(["arch/storage", "session"]), filter: "" })
    expect(keys(rows)).toEqual(["dir:arch", "  dir:arch/storage", "  D1", "  D10", "dir:session", "D0"])
    expect(rows[1]).toMatchObject({ expanded: false })
  })

  it("shows only matches while filtering, inside open directories", () => {
    const rows = visibleRows(root, { collapsed: new Set(["arch", "arch/storage"]), filter: "git" })
    expect(keys(rows)).toEqual(["dir:arch", "  dir:arch/storage", "    dir:arch/storage/deep", "      D4"])
    expect(rows.every((r) => r.kind === "decision" || r.expanded)).toBe(true)
    expect(keys(visibleRows(root, { collapsed: new Set(), filter: "D1" }))).toEqual(["dir:arch", "  D1", "  D10"])
    expect(visibleRows(root, { collapsed: new Set(), filter: "nothing like it" })).toEqual([])
  })
})

describe("path helpers", () => {
  it("lists a directory and its ancestors", () => {
    expect(ancestorDirs("")).toEqual([])
    expect(ancestorDirs("a/b/c")).toEqual(["a", "a/b", "a/b/c"])
  })
  it("takes the directory of a path", () => {
    expect(dirOfPath("D1-x.md")).toBe("")
    expect(dirOfPath("a/b/D1-x.md")).toBe("a/b")
  })
  it("shortens commits", () => {
    expect(shortCommit("0123456789abcdef")).toBe("0123456")
  })
})
