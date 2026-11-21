import { describe, expect, it } from "vitest"

import type { RawSessionEvent, SessionEventKind } from "@/api/types"
import { INTERRUPTED_TOOL_ERROR, NO_RESULT_TOOL_ERROR } from "@/lib/settle"
import { interruptedTurnInput } from "@/lib/turns"
import type { SessionEvent, ToolCallEvent } from "@/lib/types"

import fixture from "@/lib/session-events/fake-worker.fixture.json"
import { fakeWorkerEvents, type Scenario } from "@/test/fake-worker-events"

import { applyEvents, emptyTranscript, withoutStreaming, type Transcript } from "./transcript"

const raw = fakeWorkerEvents
const run = (events: RawSessionEvent[], from: Transcript = emptyTranscript) => applyEvents(from, events)
const kinds = (t: Transcript) => t.events.map((e) => e.kind)
const ofKind = <K extends SessionEvent["kind"]>(t: Transcript, kind: K) =>
  t.events.filter((e): e is Extract<SessionEvent, { kind: K }> => e.kind === kind)
const tools = (t: Transcript) => ofKind(t, "tool_call")

/** The events up to (not including) the first one `match` accepts. */
function until(events: RawSessionEvent[], match: (e: RawSessionEvent) => boolean) {
  const i = events.findIndex(match)
  if (i < 0) throw new Error("no such event")
  return events.slice(0, i)
}

describe("a streamed text reply", () => {
  const events = raw("text")

  it("streams the text live, then keeps the durable block under the same id", () => {
    const live = run(until(events, (e) => e.kind === "assistant" && !e.ephemeral))
    expect(live.turnOpen).toBe(true)
    const [message, text] = live.events
    expect(message).toMatchObject({ kind: "user_message", text: "hello", delivery: "delivered" })
    expect(text).toMatchObject({ kind: "assistant_text", text: "You said:\n\n> hello", streaming: true })

    const done = run(events)
    expect(done.turnOpen).toBe(false)
    expect(kinds(done)).toEqual(["user_message", "assistant_text"])
    expect(done.events[1]).toEqual({ kind: "assistant_text", id: text.id, at: expect.any(String), text: "You said:\n\n> hello" })
    expect(done.lastSeq).toBe(5)
  })

  it("shows the user's message as accepted until the worker hands it to the agent", () => {
    const accepted = run(events.slice(0, 2))
    expect(accepted.events).toEqual([
      expect.objectContaining({
        kind: "user_message",
        id: `msg:${JSON.parse(events[0].payloadJson).client_msg_id}`,
        delivery: "accepted",
      }),
    ])
    expect(accepted.turnOpen).toBe(true)
  })
})

describe("thinking", () => {
  it("shows the reasoning with how long it streamed, then the text", () => {
    const t = run(raw("think"))
    expect(kinds(t)).toEqual(["user_message", "reasoning", "assistant_text"])
    const [reasoning] = ofKind(t, "reasoning")
    expect(reasoning).toMatchObject({ text: "Planning a scripted response." })
    expect(reasoning.streaming).toBeUndefined()
    expect(reasoning.durationSeconds).toBeGreaterThanOrEqual(1)
  })
})

describe("tool calls", () => {
  it("runs, then completes with the result's text", () => {
    const events = raw("tool")
    const running = run(until(events, (e) => e.kind === "user" && e.payloadJson.includes("tool_result")))
    expect(tools(running)).toEqual([
      expect.objectContaining({ tool: "Bash", input: { command: "echo hi" }, status: "running" }),
    ])
    const done = run(events)
    expect(kinds(done)).toEqual(["user_message", "tool_call", "assistant_text"])
    expect(tools(done)[0]).toMatchObject({ status: "completed", output: "hi" })
  })

  it("fails a tool the user interrupted, and marks the stop", () => {
    const t = run(raw("interrupt_tool"))
    expect(kinds(t)).toEqual(["user_message", "tool_call", "turn_stopped"])
    expect(tools(t)[0].status).toBe("error")
    expect(tools(t)[0].error).toMatch(/^The tool call was interrupted/)
    // The worker's "[Request interrupted by user]" annotation is not a message.
    expect(ofKind(t, "user_message")).toHaveLength(1)
  })
})

describe("approvals", () => {
  const pendingOf = (events: RawSessionEvent[]) =>
    run(until(events, (e) => e.kind === "approval_resolved"))

  it("waits for approval with the server's reason and display, not the denial text", () => {
    const t = pendingOf(raw("approve"))
    const [tool] = tools(t)
    expect(tool).toMatchObject({
      status: "awaiting_approval",
      approval: { reason: "Scripted Bash approval required", display: "echo approved" },
    })
    expect(tool.approval!.approvalId).not.toBe("")
    expect(tool.error).toBeUndefined()
    expect(t.turnOpen).toBe(false)
  })

  it("marks an approved call and shows the retry as a call of its own", () => {
    const t = run(raw("approve"))
    const [first, retry] = tools(t)
    expect(first).toMatchObject({ status: "completed", approval: { decision: "approved" } })
    expect(retry).toMatchObject({ status: "completed", output: "approved", input: { command: "echo approved" } })
    // The approval input the server queued for the agent is not shown as the user's message.
    expect(ofKind(t, "user_message").map((m) => m.text)).toEqual(["/fake approve echo approved"])
  })

  it("marks a denied call", () => {
    const t = run(raw("deny"))
    expect(tools(t)).toEqual([
      expect.objectContaining({ status: "denied", approval: expect.objectContaining({ decision: "denied" }) }),
    ])
  })
})

describe("turns that end early", () => {
  it("keeps the text an interrupt cut short, as unsaved, and marks the stop", () => {
    const t = run(raw("interrupt"))
    expect(kinds(t)).toEqual(["user_message", "assistant_text", "turn_stopped"])
    expect(t.events[1]).toMatchObject({ text: "You sa", unsaved: true })
    expect(t.turnOpen).toBe(false)
  })

  it("drops what was streaming when the user stops the session", () => {
    const t = run(raw("stop"))
    expect(kinds(t)).toEqual(["user_message", "turn_interrupted"])
    expect(t.turnOpen).toBe(false)
  })

  it("marks a turn the worker lost, which can be retried", () => {
    const t = run(raw("crash"))
    expect(kinds(t)).toEqual(["user_message", "turn_interrupted"])
    expect(t.turnOpen).toBe(false)
    expect(interruptedTurnInput(t.events)).toBe("/fake crash")
  })

  it("keeps streamed text as unsaved when the worker is lost mid-stream", () => {
    const events = raw("text")
    const live = until(events, (e) => e.kind === "assistant" && !e.ephemeral)
    const interrupted: RawSessionEvent = {
      seq: 4,
      tsUnixMs: live.at(-1)!.tsUnixMs + 1,
      kind: "turn_interrupted",
      payloadJson: '{"turn_no":"1","outcome":"interrupted"}',
      ephemeral: false,
      truncated: false,
    }
    const t = run([...live, interrupted])
    expect(t.events[1]).toMatchObject({ kind: "assistant_text", streaming: false, unsaved: true })
    expect(t.events[2]).toMatchObject({ kind: "turn_interrupted" })
  })

  it("shows a worker error", () => {
    const t = run(raw("error"))
    expect(ofKind(t, "turn_error")).toEqual([expect.objectContaining({ message: "scripted worker error" })])
  })

  it("shows a broken turn's error, the accepted gap, and the message that waited", () => {
    const t = run(raw("broken"))
    expect(kinds(t)).toEqual([
      "user_message",
      "assistant_text",
      "user_message",
      "turn_error",
      "user_message",
      "history_gap",
      "assistant_text",
    ])
    expect(ofKind(t, "user_message").every((m) => m.delivery === "delivered")).toBe(true)
  })
})

describe("reconnecting", () => {
  it("applies each durable event once, whatever is sent again", () => {
    const events = raw("approve")
    const view = (t: Transcript) => ({ events: t.events, lastSeq: t.lastSeq, turnOpen: t.turnOpen })
    const once = run(events)
    expect(view(run(events, once))).toEqual(view(once))
    expect(view(run(events.slice(5), run(events.slice(0, 12))))).toEqual(view(once))
  })

  it.each(Object.keys(fixture) as Scenario[])("ends with the same events and ids when %s breaks anywhere", (scenario) => {
    const events = raw(scenario)
    const once = run(events)
    for (let cut = 1; cut < events.length; cut++) {
      const before = run(events.slice(0, cut))
      // The new stream starts after the last durable event; ephemeral ones before the cut are gone.
      const after = events.slice(cut).filter((e) => e.seq === 0 || e.seq > before.lastSeq)
      const resumed = run(after, withoutStreaming(before))
      expect(resumed.events.map((e) => e.id)).toEqual(once.events.map((e) => e.id))
      // Thinking time is known only for thinking seen streaming.
      const timeless = (list: readonly SessionEvent[]) =>
        list.map((e) => (e.kind === "reasoning" ? { ...e, durationSeconds: undefined } : e))
      expect(timeless(resumed.events)).toEqual(timeless(once.events))
    }
  })

  it("drops partial text on reconnect, and ignores deltas of a block it didn't see start", () => {
    const events = raw("text")
    const firstDelta = events.findIndex((e) => e.payloadJson.includes("text_delta"))
    const before = run(events.slice(0, firstDelta + 1))
    expect(before.events.at(-1)).toMatchObject({ streaming: true, text: "You " })
    const dropped = withoutStreaming(before)
    expect(kinds(dropped)).toEqual(["user_message"])
    const resumed = run(events.slice(firstDelta + 1, firstDelta + 3), dropped)
    expect(kinds(resumed)).toEqual(["user_message"])
  })
})

describe("odd input", () => {
  const base = { tsUnixMs: 0, ephemeral: false, truncated: false }

  it("ignores keepalives, unknown kinds and malformed payloads", () => {
    const t = run([
      { ...base, seq: 0, kind: "unspecified", payloadJson: "", ephemeral: true },
      { ...base, seq: 1, kind: "bgtask_started", payloadJson: "{}" },
      { ...base, seq: 2, kind: "assistant", payloadJson: "not json" },
      { ...base, seq: 3, kind: "user", payloadJson: "[]" },
      { ...base, seq: 4, kind: "approval_resolved", payloadJson: '{"approval_id":"nope","state":"approved"}' },
      { ...base, seq: 5, kind: "input_accepted", payloadJson: "{}" },
      { ...base, seq: 6, kind: "assistant", payloadJson: '{"type":"assistant","message":{"id":"m","content":[1,null,{"type":"server_tool_use"}]}}' },
      { ...base, seq: 7, kind: "user", payloadJson: '{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"unknown"}]}}' },
      { ...base, seq: 8, kind: "tool_progress" as SessionEventKind, payloadJson: '{"x":1}' },
    ])
    expect(t.events).toEqual([])
    expect(t.lastSeq).toBe(8)
  })

  it("shows a placeholder for truncated content, and still marks truncated boundaries", () => {
    const t = run([
      { ...base, seq: 1, kind: "assistant", payloadJson: "", truncated: true },
      { ...base, seq: 2, kind: "turn_interrupted", payloadJson: "", truncated: true },
    ])
    expect(kinds(t)).toEqual(["truncated", "turn_interrupted"])
  })

  it("settles a running tool when the turn ends without its result", () => {
    const events = raw("tool")
    const running = until(events, (e) => e.kind === "user" && e.payloadJson.includes("tool_result"))
    const stopped: RawSessionEvent = {
      ...base,
      seq: 9,
      kind: "turn_stopped",
      payloadJson: '{"turn_no":"1","outcome":"stopped"}',
    }
    const [tool] = tools(run([...running, stopped])) as ToolCallEvent[]
    expect(tool).toMatchObject({ status: "error", error: INTERRUPTED_TOOL_ERROR })
  })
})

describe("scenarios added to the recording", () => {
  it("shows the tool uses of one response as calls of their own", () => {
    const t = run(raw("parallel_tools"))
    expect(kinds(t)).toEqual(["user_message", "tool_call", "tool_call", "assistant_text"])
    expect(tools(t).map((c) => [c.status, c.output])).toEqual([
      ["completed", "hi"],
      ["completed", "hi"],
    ])
    expect(new Set(tools(t).map((c) => c.id)).size).toBe(2)
  })

  it("settles a call whose result was too large to keep once its turn ends", () => {
    const events = raw("truncated_tool_result")
    const before = run(until(events, (e) => e.kind === "result"))
    expect(kinds(before)).toEqual(["user_message", "tool_call", "truncated", "assistant_text"])
    expect(tools(before)[0].status).toBe("running")
    const t = run(events)
    expect(tools(t)[0]).toMatchObject({ status: "error", error: NO_RESULT_TOOL_ERROR })
    expect(t.turnOpen).toBe(false)
  })

  it("marks an approval that expired before the user answered", () => {
    const t = run(raw("approval_expired"))
    expect(tools(t)).toEqual([
      expect.objectContaining({ status: "denied", approval: expect.objectContaining({ decision: "expired" }) }),
    ])
  })

  it("keeps the request time of an approval, for its expiry", () => {
    const events = raw("approve")
    const pending = events.find((e) => e.kind === "approval_pending")!
    const t = run(until(events, (e) => e.kind === "approval_resolved"))
    expect(tools(t)[0].approval?.requestedAtUnixMs).toBe(pending.tsUnixMs)
  })

  it("starts the next turn for a NOW message, keeping what the first had shown", () => {
    const t = run(raw("now"))
    expect(kinds(t)).toEqual(["user_message", "user_message", "assistant_text", "turn_stopped", "assistant_text"])
    // What the first turn had streamed stays, unsaved, after the message that stopped it.
    expect(t.events[2]).toMatchObject({ text: "You sa", unsaved: true })
    expect(ofKind(t, "user_message").map((m) => [m.text, m.delivery])).toEqual([
      ["/fake slow 20", "delivered"],
      ["right now", "delivered"],
    ])
    expect(t.turnNo).toBe(2)
  })

  it("leaves a turn open when it ends without RESULT; the session's state decides then", () => {
    const events = raw("broken")
    const t = run(until(events, (e) => e.kind === "error"))
    expect(t.turnOpen).toBe(true)
    expect(t.turnNo).toBe(2)
  })
})

describe("odd input, continued", () => {
  const base = { tsUnixMs: Date.parse("2026-11-21T10:00:00Z"), ephemeral: false, truncated: false }
  const json = (v: unknown) => JSON.stringify(v)
  const partial = (event: object): RawSessionEvent => ({
    ...base,
    seq: 0,
    ephemeral: true,
    kind: "assistant",
    payloadJson: json({ type: "stream_event", event }),
  })
  const text = (index: number, delta: string) => [
    partial({ type: "content_block_start", index, content_block: { type: "text", text: "" } }),
    partial({ type: "content_block_delta", index, delta: { type: "text_delta", text: delta } }),
  ]
  const durable = (seq: number, messageId: string, body: string): RawSessionEvent => ({
    ...base,
    seq,
    kind: "assistant",
    payloadJson: json({ type: "assistant", message: { id: messageId, content: [{ type: "text", text: body }] } }),
  })

  it("keeps the blocks after a truncated one in place, live and on replay", () => {
    const live = [
      partial({ type: "message_start", message: { id: "m" } }),
      ...text(0, "HUGE"),
      { ...base, seq: 1, kind: "assistant" as const, payloadJson: "", truncated: true },
      ...text(1, "second"),
      durable(2, "m", "second"),
    ]
    const shown = (t: Transcript) => t.events.map((e) => [e.kind, e.kind === "assistant_text" ? e.text : ""])
    const want = [
      ["truncated", ""],
      ["assistant_text", "second"],
    ]
    const t = run(live)
    expect(shown(t)).toEqual(want)
    expect(t.events.filter((e) => e.kind === "assistant_text" && e.streaming)).toEqual([])
    const replay = run(live.filter((e) => !e.ephemeral))
    expect(shown(replay)).toEqual(want)
    // The same ids live and on replay, also while the block streamed.
    expect(t.events.map((e) => e.id)).toEqual(replay.events.map((e) => e.id))
    expect(run(live.slice(0, -1)).events.at(-1)?.id).toBe(replay.events.at(-1)?.id)
  })

  it("numbers the blocks around a truncated one in the middle the same live and on replay", () => {
    const live = [
      partial({ type: "message_start", message: { id: "m" } }),
      ...text(0, "first"),
      durable(1, "m", "first"),
      ...text(1, "HUGE"),
      { ...base, seq: 2, kind: "assistant" as const, payloadJson: "", truncated: true },
      ...text(2, "third"),
      durable(3, "m", "third"),
    ]
    const ids = (t: Transcript) => t.events.map((e) => e.id)
    expect(ids(run(live))).toEqual(["m:0", "truncated:2", "m:1"])
    expect(ids(run(live.filter((e) => !e.ephemeral)))).toEqual(["m:0", "truncated:2", "m:1"])
  })

  it("skips an event it can't handle, and keeps the rest of the batch", () => {
    const input = (seq: number, id: string, tsUnixMs = base.tsUnixMs): RawSessionEvent => ({
      ...base,
      seq,
      tsUnixMs,
      kind: "input_accepted",
      payloadJson: json({ input_id: `in-${id}`, client_msg_id: id, text: id }),
    })
    const t = run([input(1, "a"), input(2, "b", 9e15), input(3, "c")])
    expect(ofKind(t, "user_message").map((m) => m.text)).toEqual(["a", "b", "c"])
    expect(t.lastSeq).toBe(3)
  })

  it("never shows a subagent's messages or the worker's notes as the user's", () => {
    const user = (seq: number, payload: object): RawSessionEvent => ({
      ...base,
      seq,
      kind: "user",
      payloadJson: json({ type: "user", uuid: `u${seq}`, ...payload }),
    })
    const t = run([
      // A Task subagent's prompt, reply and tool call.
      user(1, { parent_tool_use_id: "toolu_task", message: { role: "user", content: "Find the flaky test" } }),
      {
        ...base,
        seq: 2,
        kind: "assistant",
        payloadJson: json({
          type: "assistant",
          parent_tool_use_id: "toolu_task",
          message: { id: "sub", content: [{ type: "tool_use", id: "toolu_sub", name: "Bash", input: {} }] },
        }),
      },
      // Notes for the agent, without the synthetic mark.
      user(3, { message: { role: "user", content: [{ type: "text", text: "[Request interrupted by user]" }] } }),
      user(4, { message: { role: "user", content: "Part of the conversation history is missing." } }),
    ])
    expect(t.events).toEqual([])
  })

  it("starts no new transcript object for frames that change nothing", () => {
    const t = run(raw("text"))
    const again = run([{ ...base, seq: 0, kind: "unspecified", payloadJson: "", ephemeral: true }], t)
    expect(again.events).toBe(t.events)
  })
})
