import { describe, expect, it } from "vitest"

import { interruptedTurnInput, latestInterruptId } from "./turns"
import type { SessionEvent } from "./types"

const at = "2026-11-17T12:00:00Z"
const message = (id: string, text: string): SessionEvent => ({
  kind: "user_message",
  id,
  at,
  text,
  clientMsgId: id,
  delivery: "delivered",
})

describe("interrupted turns", () => {
  const events: SessionEvent[] = [
    message("u1", "First"),
    { kind: "assistant_text", id: "a1", at, text: "Done." },
    message("u2", "Fix the build"),
    { kind: "turn_interrupted", id: "i1", at },
  ]

  it("finds the latest marker and the message that started its turn", () => {
    expect(latestInterruptId(events)).toBe("i1")
    expect(interruptedTurnInput(events)).toBe("Fix the build")
  })

  it("offers nothing once another message was sent", () => {
    expect(interruptedTurnInput([...events, message("u3", "Something else")])).toBeNull()
    expect(interruptedTurnInput(events.slice(0, 3))).toBeNull()
  })
})
