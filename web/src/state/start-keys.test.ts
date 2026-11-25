import { afterEach, describe, expect, it } from "vitest"

import { forgetAllStartKeys, startKey } from "./start-keys"

afterEach(() => forgetAllStartKeys())

describe("start keys", () => {
  const at = { device: "devbox", path: "/src/circus" }

  it("keeps the key for a start sent again with the same permission mode", () => {
    const first = startKey("p1", 0, at, undefined, "acceptEdits")
    expect(startKey("p1", 0, at, undefined, "acceptEdits")).toBe(first)
  })

  it("takes a new key when the permission mode changed, which the server would refuse under the old one", () => {
    const edits = startKey("p1", 0, at, undefined, "acceptEdits")
    const auto = startKey("p1", 0, at, undefined, "auto")
    expect(auto).not.toBe(edits)
    // And again for the other provider choices, as before.
    expect(startKey("p1", 0, at, "", "auto")).not.toBe(auto)
  })
})
