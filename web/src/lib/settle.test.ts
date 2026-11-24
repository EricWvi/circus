import { describe, expect, it } from "vitest"

import { NO_RESULT_TOOL_ERROR, settleEvents } from "./settle"
import type { SessionEvent } from "./types"

describe("settleEvents", () => {
  it("stops streaming replies and marks running tools as cut off", () => {
    const events: SessionEvent[] = [
      { kind: "assistant_text", id: "a", at: "", text: "par", streaming: true },
      { kind: "tool_call", id: "t", at: "", tool: "Bash", input: {}, status: "running" },
      { kind: "tool_call", id: "u", at: "", tool: "Read", input: {}, status: "completed" },
    ]
    const [text, tool, done] = settleEvents(events)
    expect(text).toMatchObject({ streaming: false, text: "par" })
    expect(tool).toMatchObject({ status: "interrupted", interruption: "cut_off" })
    expect(tool).not.toHaveProperty("error", expect.anything())
    expect(done).toBe(events[2])
  })

  it("fails running tools when the turn ended without their result", () => {
    const events: SessionEvent[] = [
      { kind: "tool_call", id: "t", at: "", tool: "Bash", input: {}, status: "running" },
    ]
    expect(settleEvents(events, "no_result")[0]).toMatchObject({ status: "error", error: NO_RESULT_TOOL_ERROR })
  })

  it("returns the same array when nothing is in flight", () => {
    const events: SessionEvent[] = [{ kind: "assistant_text", id: "a", at: "", text: "x" }]
    expect(settleEvents(events)).toBe(events)
  })
})
