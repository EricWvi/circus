import { describe, expect, it } from "vitest"

import { decisionPath, ledgerPath, parseDecisionSegment } from "./tree"

describe("ledger paths", () => {
  it("builds the ledger's and a decision's paths", () => {
    expect(ledgerPath("p 1")).toBe("/p/p%201/ledger")
    expect(decisionPath("p1", 12)).toBe("/p/p1/ledger/D12")
    expect(decisionPath("p1", 0, { history: true })).toBe("/p/p1/ledger/D0?view=history")
    expect(decisionPath("p1", 3, { at: "abc" })).toBe("/p/p1/ledger/D3?at=abc")
  })

  it("parses a decision segment", () => {
    expect(parseDecisionSegment("D12")).toBe(12)
    expect(parseDecisionSegment("D0")).toBe(0)
    expect(parseDecisionSegment("D012")).toBe(12)
    expect(parseDecisionSegment("d12")).toBe(12)
    expect(parseDecisionSegment(" D4 ")).toBe(4)
    expect(parseDecisionSegment("D4294967295")).toBe(4294967295)
    for (const bad of [undefined, "", "D", "12", "D 12", "D-1", "D1.5", "D4294967296", "D1234567890123", "D12x"]) {
      expect({ bad, n: parseDecisionSegment(bad) }).toEqual({ bad, n: null })
    }
  })
})
