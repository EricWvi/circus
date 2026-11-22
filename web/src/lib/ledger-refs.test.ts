import { describe, expect, it } from "vitest"

import { tokenizeDecisionRefs, type RefToken } from "./ledger-refs"

/** The references found, as "D12" or "project#D12". */
const refs = (text: string, names: string[] = []) =>
  tokenizeDecisionRefs(text, names).flatMap((t) =>
    t.kind === "ref" ? [t.project === null ? `D${t.number}` : `${t.project}#D${t.number}`] : [],
  )

describe("tokenizeDecisionRefs", () => {
  it("splits text around references and keeps every character", () => {
    const tokens = tokenizeDecisionRefs("See D12 and D3.")
    expect(tokens).toEqual<RefToken[]>([
      { kind: "text", text: "See " },
      { kind: "ref", text: "D12", number: 12, project: null },
      { kind: "text", text: " and " },
      { kind: "ref", text: "D3", number: 3, project: null },
      { kind: "text", text: "." },
    ])
    expect(tokens.map((t) => t.text).join("")).toBe("See D12 and D3.")
  })

  it("finds references at word boundaries only", () => {
    expect(refs("D1 D0 (D7) [D8], D9; D10: D11! D12? 'D13' \"D14\" -D15- /D16/")).toEqual(
      ["D1", "D0", "D7", "D8", "D9", "D10", "D11", "D12", "D13", "D14", "D15", "D16"],
    )
    expect(refs("AD12 D12a D12_ _D12 3D12 éD12 D12é xD1y")).toEqual([])
    expect(refs("d12 D 12 D-12 D")).toEqual([])
  })

  it("treats Chinese, Japanese and Korean characters as boundaries", () => {
    expect(refs("见D12的说明")).toEqual(["D12"])
    expect(refs("参见 D12，以及D13。")).toEqual(["D12", "D13"])
    expect(refs("「D4」と한D5다")).toEqual(["D4", "D5"])
  })

  it("accepts only canonical numbers up to the highest decision number", () => {
    expect(refs("D0 D012 D00 D4096 D4097 D99999")).toEqual(["D0", "D4096", "D4097", "D99999"])
    expect(refs("D4294967295 D4294967296 D12345678901")).toEqual(["D4294967295"])
  })

  it("finds the project of a reference by its exact name", () => {
    const names = ["palace", "my project", "circus"]
    expect(refs("palace#D3 and circus#D0", names)).toEqual(["palace#D3", "circus#D0"])
    expect(refs("see my project#D2.", names)).toEqual(["my project#D2"])
    expect(refs("（palace#D3）、palace#D4", names)).toEqual(["palace#D3", "palace#D4"])
    const [, ref] = tokenizeDecisionRefs("x palace#D3 y", names)
    expect(ref).toEqual({ kind: "ref", text: "palace#D3", number: 3, project: "palace" })
  })

  it("leaves references to unknown projects as text", () => {
    const names = ["circus"]
    expect(refs("other#D3 Circus#D3 my-circus#D3 circus2#D3", names)).toEqual([])
    expect(refs("palace#D3")).toEqual([])
    // A known project must be the whole name before "#".
    expect(refs("xcircus#D3", names)).toEqual([])
  })

  it("reads a bare #D12 as this project's", () => {
    expect(refs("#D12 and (#D13)")).toEqual(["D12", "D13"])
    expect(tokenizeDecisionRefs("#D12")[0]).toEqual({ kind: "text", text: "#" })
  })

  it("lets a project whose name ends in D<n> be referenced", () => {
    expect(refs("see a D1#D2 here", ["a D1"])).toEqual(["a D1#D2"])
    expect(refs("D1#D2", ["D1"])).toEqual(["D1#D2"])
    // Without "#D<n>" after it, D1 is this project's.
    expect(refs("a D1 and a D1#x", ["a D1"])).toEqual(["D1", "D1"])
    for (const [text, names] of [["x a D1#D2 y", ["a D1"]], ["D1#D2", ["D1"]], ["a D1#D2#D3", ["a D1", "D2"]]] as const) {
      expect(tokenizeDecisionRefs(text, names).map((t) => t.text).join("")).toBe(text)
    }
  })

  it("lets a project name with D<n> anywhere inside it win", () => {
    expect(refs("see Plan D2 web#D5.", ["Plan D2 web"])).toEqual(["Plan D2 web#D5"])
    expect(refs("Plan D2 web D3#D4", ["Plan D2 web D3"])).toEqual(["Plan D2 web D3#D4"])
    // Not followed by "#D<n>", or not that name: the inner D2 is this project's.
    expect(refs("Plan D2 web#x and Plan D2 webs#D5", ["Plan D2 web"])).toEqual(["D2", "D2"])
  })

  it("never starts a name inside a reference already taken", () => {
    // "b#D1 c" would start inside "a b#D1".
    const names = ["a b", "b#D1 c"]
    expect(refs("a b#D1 c#D2", names)).toEqual(["a b#D1"])
    expect(tokenizeDecisionRefs("a b#D1 c#D2", names).map((t) => t.text).join("")).toBe("a b#D1 c#D2")
    // Nor lets such a name swallow a plain reference after it.
    expect(refs("a b#D1 c D2#D3", ["a b", "b#D1 c D2"])).toEqual(["a b#D1", "D2"])
  })

  it("never lets a name overlap a reference already taken", () => {
    // "D2#D3": D2 is taken by "a D1#D2", so "#D3" can't claim the name "D2".
    const tokens = tokenizeDecisionRefs("a D1#D2#D3", ["a D1", "D2"])
    expect(tokens.filter((t) => t.kind === "ref").map((t) => t.text)).toEqual(["a D1#D2"])
    expect(tokens.map((t) => t.text).join("")).toBe("a D1#D2#D3")
  })

  it("prefers the longest matching project name", () => {
    expect(refs("big circus#D1", ["circus", "big circus"])).toEqual(["big circus#D1"])
  })

  it("finds references next to emoji and other astral characters", () => {
    expect(refs("😀D12😀 𝐀D3")).toEqual(["D12"])
  })
})
