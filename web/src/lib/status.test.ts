import { describe, expect, it } from "vitest"

import { isVisibleInTree, rollupState } from "./status"
import type { SessionState } from "./types"

const of = (...states: SessionState[]) => states.map((state) => ({ state }))

describe("rollupState", () => {
  it("prefers approval over every other state", () => {
    expect(rollupState(of("idle", "broken", "running", "awaiting_approval"))).toBe(
      "awaiting_approval",
    )
  })

  it("orders broken > running > queued > failed > interrupted > idle", () => {
    expect(rollupState(of("running", "broken", "queued"))).toBe("broken")
    expect(rollupState(of("queued", "idle", "running"))).toBe("running")
    expect(rollupState(of("idle", "queued", "interrupted"))).toBe("queued")
    expect(rollupState(of("idle", "interrupted"))).toBe("interrupted")
    expect(rollupState(of("interrupted", "failed", "idle"))).toBe("failed")
    expect(rollupState(of("failed", "queued"))).toBe("queued")
  })

  it("ignores stopped sessions", () => {
    expect(rollupState(of("stopped", "idle"))).toBe("idle")
    expect(rollupState(of("stopped"))).toBeUndefined()
    expect(rollupState([])).toBeUndefined()
  })
})

describe("isVisibleInTree", () => {
  it("hides only stopped sessions", () => {
    expect(isVisibleInTree({ state: "stopped" })).toBe(false)
    expect(isVisibleInTree({ state: "broken" })).toBe(true)
  })
})
