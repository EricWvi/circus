import { describe, expect, it } from "vitest"

import { INTERRUPTED_TOOL_ERROR } from "./settle"
import {
  breakTranscript,
  continueWithoutHistory,
  interruptedTurnInput,
  interruptTurn,
  latestInterruptId,
  startTurn,
  stopSession,
  stopTurn,
  withoutStreamingText,
} from "./turns"
import type { SessionEvent } from "./types"

const at = "2026-11-17T12:00:00Z"
const m = (id: string) => ({ id, at })
const image = { id: "i1", filename: "ci.png", mediaType: "image/png", url: "data:image/png;base64,AA" }

const inFlight: SessionEvent[] = [
  { kind: "user_message", id: "u1", at, text: "Fix the build", attachments: [image] },
  {
    kind: "tool_call",
    id: "t1",
    at,
    tool: "Bash",
    input: { command: "go test ./..." },
    status: "running",
  },
  { kind: "assistant_text", id: "a1", at, text: "The failing test is", streaming: true },
]

describe("startTurn", () => {
  it("appends the message, runs the session and names it after the first message", () => {
    const first = startTurn([], { text: "Fix the build", attachments: [] }, m("u1"))
    expect(first.events).toEqual([{ kind: "user_message", id: "u1", at, text: "Fix the build", attachments: [] }])
    expect(first.activity).toEqual({ state: "running", title: "Fix the build" })
    expect(startTurn(first.events, { text: "More", attachments: [] }, m("u2")).activity).toEqual({
      state: "running",
    })
  })
})

describe("stopTurn", () => {
  it("makes the session idle, keeps the shown text, settles tools and adds a quiet marker", () => {
    const { events, activity } = stopTurn(inFlight, m("s1"))
    expect(activity).toEqual({ state: "idle" })
    expect(events[2]).toEqual({ kind: "assistant_text", id: "a1", at, text: "The failing test is", streaming: false })
    expect(events[1]).toMatchObject({ status: "error", error: INTERRUPTED_TOOL_ERROR })
    expect(events.at(-1)).toEqual({ kind: "turn_stopped", id: "s1", at })
  })
})

describe("interruptTurn (worker lost)", () => {
  it("interrupts the session, keeps the streamed text as unsaved and adds the marker", () => {
    const { events, activity } = interruptTurn(inFlight, m("i1"))
    expect(activity).toEqual({ state: "interrupted", pendingApprovalCount: 0 })
    expect(events[2]).toMatchObject({ text: "The failing test is", streaming: false, unsaved: true })
    expect(events[1]).toMatchObject({ status: "error", error: INTERRUPTED_TOOL_ERROR })
    expect(events.at(-1)).toEqual({ kind: "turn_interrupted", id: "i1", at })
    expect(latestInterruptId(events)).toBe("i1")
  })

  it("leaves finished text alone", () => {
    const done: SessionEvent[] = [{ kind: "assistant_text", id: "a0", at, text: "Done.", streaming: false }]
    expect(interruptTurn(done, m("i1")).events[0]).toEqual(done[0])
  })

  it("lets Retry resend the turn's message with its images, which runs the session again", () => {
    const interrupted = interruptTurn(inFlight, m("i1")).events
    const input = interruptedTurnInput(interrupted)
    expect(input).toEqual({ text: "Fix the build", attachments: [image] })
    const retried = startTurn(interrupted, input!, m("u2"))
    expect(retried.activity.state).toBe("running")
    expect(retried.events.at(-1)).toMatchObject({ kind: "user_message", text: "Fix the build", attachments: [image] })
  })

  it("lets Continue send the continue prompt, which runs the session again", () => {
    const interrupted = interruptTurn(inFlight, m("i1")).events
    const continued = startTurn(interrupted, { text: "Continue", attachments: [] }, m("u2"))
    expect(continued.activity.state).toBe("running")
    expect(continued.events.at(-1)).toMatchObject({ kind: "user_message", text: "Continue" })
    // A later interruption retries the latest turn, not the first one.
    const again = interruptTurn(continued.events, m("i2")).events
    expect(interruptedTurnInput(again)).toEqual({ text: "Continue", attachments: [] })
    expect(latestInterruptId(again)).toBe("i2")
  })

  it("has nothing to retry once a message followed the interruption", () => {
    const interrupted = interruptTurn(inFlight, m("i1")).events
    const retried = startTurn(interrupted, interruptedTurnInput(interrupted)!, m("u2")).events
    expect(interruptedTurnInput(retried)).toBeNull()
  })

  it("has nothing to retry without an interrupted turn", () => {
    expect(interruptedTurnInput(inFlight)).toBeNull()
    expect(latestInterruptId(inFlight)).toBeUndefined()
  })
})

describe("broken sessions", () => {
  it("break the transcript without changing it", () => {
    expect(breakTranscript(inFlight)).toEqual({ events: inFlight, activity: { state: "broken" } })
  })

  it("continue without the missing part: idle, with a gap divider at the end", () => {
    const { events, activity } = continueWithoutHistory(inFlight, m("g1"))
    expect(activity).toEqual({ state: "idle" })
    expect(events.at(-1)).toEqual({ kind: "history_gap", id: "g1", at })
  })

  it("stop for good", () => {
    expect(stopSession(inFlight)).toEqual({
      events: inFlight,
      activity: { state: "stopped", pendingApprovalCount: 0 },
    })
  })
})

describe("withoutStreamingText", () => {
  it("drops partially streamed text and keeps everything else", () => {
    expect(withoutStreamingText(inFlight).map((e) => e.id)).toEqual(["u1", "t1"])
  })
})
