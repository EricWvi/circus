import { describe, expect, it } from "vitest"

import { capRows, compareVersions, lineDiff, MAX_DIFF_LINES, MAX_DIFF_ROWS, wholeVersions, type DiffLine } from "./line-diff"

describe("line diffs", () => {
  it("numbers the lines of both sides", () => {
    expect(lineDiff("a\nb\nc\n", "a\nB\nc\nd\n")).toEqual([
      { kind: "context", text: "a", oldNo: 1, newNo: 1 },
      { kind: "removed", text: "b", oldNo: 2 },
      { kind: "added", text: "B", newNo: 2 },
      { kind: "context", text: "c", oldNo: 3, newNo: 3 },
      { kind: "added", text: "d", newNo: 4 },
    ])
  })

  it("is all added for a new file and all removed for a deleted one", () => {
    expect(lineDiff("", "x\ny")).toEqual([
      { kind: "added", text: "x", newNo: 1 },
      { kind: "added", text: "y", newNo: 2 },
    ])
    expect(lineDiff("x\n", "")).toEqual([{ kind: "removed", text: "x", oldNo: 1 }])
  })

  it("folds unchanged lines far from any change", () => {
    const base = Array.from({ length: 20 }, (_, i) => `line ${i + 1}`).join("\n")
    const next = base.replace("line 10", "line ten")
    const diff = lineDiff(base, next, { context: 2 })!
    expect(diff[0]).toEqual({ kind: "skipped", count: 7 })
    expect(diff.slice(1, 3).map((l) => l.kind)).toEqual(["context", "context"])
    expect(diff.filter((l) => l.kind === "removed")).toEqual([{ kind: "removed", text: "line 10", oldNo: 10 }])
    expect(diff.at(-1)).toEqual({ kind: "skipped", count: 8 })
  })

  it("is a single fold when nothing changed", () => {
    expect(lineDiff("a\nb\nc\nd\ne\nf\ng\nh", "a\nb\nc\nd\ne\nf\ng\nh")).toEqual([{ kind: "skipped", count: 8 }])
  })

  it("gives up when the time is up", () => {
    const a = Array.from({ length: 400 }, (_, i) => `a${i}`).join("\n")
    const b = Array.from({ length: 400 }, (_, i) => `b${i}`).join("\n")
    expect(lineDiff(a, b, { timeoutMs: -1 })).toBeNull()
  })
})

const lines = (n: number, prefix: string) => Array.from({ length: n }, (_, i) => `${prefix}${i}`).join("\n")

describe("bounded diffs", () => {
  it("caps the rows, counting the lines left out", () => {
    const rows: DiffLine[] = Array.from({ length: 10 }, (_, i) => ({ kind: "added", text: `x${i}`, newNo: i + 1 }))
    rows.splice(5, 0, { kind: "skipped", count: 40 })
    expect(capRows(rows, 4)).toEqual([...rows.slice(0, 4), { kind: "more", count: 46 }])
    expect(capRows(rows, 11)).toBe(rows)
  })

  it("shows both versions whole with each side capped to half the rows", () => {
    const whole = wholeVersions(lines(10, "a"), lines(3, "b"), 8)
    expect(whole.map((l) => l.kind)).toEqual([
      "removed", "removed", "removed", "removed", "more",
      "added", "added", "added",
    ])
    expect(whole[4]).toEqual({ kind: "more", count: 6 })
  })

  it("doesn't diff versions with too many lines, and says so", async () => {
    let diffed = false
    const outcome = await compareVersions(lines(MAX_DIFF_LINES + 1, "a"), "b\n", async () => {
      diffed = true
      return []
    })
    expect(diffed).toBe(false)
    expect(outcome.kind).toBe("whole")
    expect(outcome.kind === "whole" && outcome.reason).toBe("too_large")
    expect(outcome.lines.length).toBeLessThanOrEqual(MAX_DIFF_ROWS + 2)
  })

  it("shows both versions when the diff gives up", async () => {
    const outcome = await compareVersions("a\n", "b\n", async () => null)
    expect(outcome).toEqual({
      kind: "whole",
      reason: "too_different",
      lines: [
        { kind: "removed", text: "a", oldNo: 1 },
        { kind: "added", text: "b", newNo: 1 },
      ],
    })
  })

  it("shows both versions when the diff fails", async () => {
    const outcome = await compareVersions("a\n", "b\n", async () => {
      throw new Error("worker gone")
    })
    expect(outcome).toMatchObject({ kind: "whole", reason: "too_different" })
    expect(outcome.lines).toHaveLength(2)
  })

  it("caps a diff of a large new file", async () => {
    const outcome = await compareVersions("", lines(5_000, "n"))
    expect(outcome.kind).toBe("diff")
    expect(outcome.lines).toHaveLength(MAX_DIFF_ROWS + 1)
    expect(outcome.lines.at(-1)).toEqual({ kind: "more", count: 3_000 })
  })
})
