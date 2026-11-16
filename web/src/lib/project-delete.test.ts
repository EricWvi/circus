import { describe, expect, it } from "vitest"

import { deleteBlockedReason } from "./project-delete"

describe("deleteBlockedReason", () => {
  it("allows deleting when every session is stopped", () => {
    expect(deleteBlockedReason([])).toBeNull()
    expect(deleteBlockedReason([{ state: "stopped" }, { state: "stopped" }])).toBeNull()
  })

  it("counts every session that is not stopped", () => {
    expect(deleteBlockedReason([{ state: "idle" }, { state: "stopped" }])).toBe(
      "Stop its active session first",
    )
    expect(deleteBlockedReason([{ state: "broken" }, { state: "interrupted" }, { state: "queued" }])).toBe(
      "Stop its 3 active sessions first",
    )
  })
})
