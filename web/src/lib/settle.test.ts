import { describe, expect, it } from "vitest"

import { INTERRUPTED_TOOL_ERROR, settleEvents } from "./settle"
import type { SessionEvent } from "./types"

describe("settleEvents", () => {
  it("stops streaming replies and fails running tools", () => {
    const events: SessionEvent[] = [
      { kind: "assistant_text", id: "a", at: "", text: "par", streaming: true },
      { kind: "tool_call", id: "t", at: "", tool: "Bash", input: {}, status: "running" },
      { kind: "tool_call", id: "u", at: "", tool: "Read", input: {}, status: "completed" },
    ]
    const [text, tool, done] = settleEvents(events)
    expect(text).toMatchObject({ streaming: false, text: "par" })
    expect(tool).toMatchObject({ status: "error", error: INTERRUPTED_TOOL_ERROR })
    expect(done).toBe(events[2])
  })

  it("returns the same array when nothing is in flight", () => {
    const events: SessionEvent[] = [{ kind: "assistant_text", id: "a", at: "", text: "x" }]
    expect(settleEvents(events)).toBe(events)
  })
})
