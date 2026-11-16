import { describe, expect, it } from "vitest"

import { backendKind } from "./backend"

describe("backendKind", () => {
  it("defaults to the mock", () => {
    expect(backendKind(undefined)).toBe("mock")
    expect(backendKind("")).toBe("mock")
    expect(backendKind("mock")).toBe("mock")
    expect(backendKind("server")).toBe("server")
  })

  it("rejects anything else", () => {
    expect(() => backendKind("Server")).toThrow(/VITE_CIRCUS_BACKEND/)
  })
})
