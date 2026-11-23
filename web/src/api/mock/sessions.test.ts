import { afterEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import type { CircusClient, RawSessionEvent } from "@/api/types"
import { applyEvents, emptyTranscript } from "@/lib/session-events/transcript"

import { createMockBackend } from "./backend"
import { addWorkerDemo, SEED_OWNER_ID, seedState, type MockState } from "./state"

function mock(state: MockState = seedState()) {
  state.cookie = SEED_OWNER_ID
  return { state, ...createMockBackend({ state, latency: () => 0, sessionTiming: { stepMs: 1, chunkMs: 1 } }) }
}

/** Watches a session until `done` holds for the events received, or a second passes. */
async function watchUntil(
  client: CircusClient,
  sessionId: string,
  done: (events: RawSessionEvent[]) => boolean,
  afterSeq = 0,
): Promise<RawSessionEvent[]> {
  const abort = new AbortController()
  const timer = setTimeout(() => abort.abort(), 1_000)
  const events: RawSessionEvent[] = []
  try {
    for await (const event of client.sessions.watchSession({ sessionId, afterSeq, signal: abort.signal })) {
      events.push(event)
      if (done(events)) break
    }
  } finally {
    clearTimeout(timer)
    abort.abort()
  }
  return events
}

const has = (kind: RawSessionEvent["kind"], n = 1) => (events: RawSessionEvent[]) =>
  events.filter((e) => e.kind === kind).length >= n

const view = (events: RawSessionEvent[]) => applyEvents(emptyTranscript, events)

describe("mock sessions", () => {
  it("lists the demo sessions with the state the UI shows", async () => {
    const { client } = mock()
    const sessions = await client.sessions.listSessions({})
    const state = (id: string) => sessions.find((s) => s.sessionId === id)?.state
    expect(state("fix-auth-refresh")).toBe("awaiting_approval")
    expect(state("libvips")).toBe("broken")
    expect(state("dotfiles-go")).toBe("interrupted")
    expect(state("quic-spike")).toBe("stopped")
    expect(state("web-vite")).toBe("idle")
    expect(sessions.every((s) => s.title !== "")).toBe(true)
  })

  it("answers a message with streamed text and a tool call, as the server streams them", async () => {
    const { client } = mock()
    const session = await client.sessions.createSession({ projectId: "circus", locationIndex: 0, clientRequestId: crypto.randomUUID() })
    expect(session.title).toBe("")
    const sent = await client.sessions.sendMessage({ sessionId: session.sessionId, clientMsgId: "c1", text: "hello" })
    const events = await watchUntil(client, session.sessionId, has("result"))
    expect(events[0]).toMatchObject({ kind: "unspecified", ephemeral: true })
    expect(events.some((e) => e.ephemeral && e.kind === "assistant")).toBe(true)
    const t = view(events)
    expect(t.events.map((e) => e.kind)).toEqual(["user_message", "assistant_text", "tool_call", "assistant_text"])
    expect(t.events[0]).toMatchObject({ id: "msg:c1", delivery: "delivered" })
    expect(t.events[2]).toMatchObject({ status: "completed" })
    expect(sent.eventSeq).toBe(1)
    expect((await client.sessions.getSession({ sessionId: session.sessionId })).title).toBe("hello")

    // The same id with the same text is the same message; other text is refused.
    expect(await client.sessions.sendMessage({ sessionId: session.sessionId, clientMsgId: "c1", text: "hello" })).toEqual(sent)
    await expect(
      client.sessions.sendMessage({ sessionId: session.sessionId, clientMsgId: "c1", text: "other" }),
    ).rejects.toMatchObject({ code: "already_exists" })
  })

  it("replays only the events after after_seq", async () => {
    const { client } = mock()
    const all = await watchUntil(client, "web-vite", (events) => events.length > 1)
    const replay = await watchUntil(client, "web-vite", () => false, 2)
    expect(all.length).toBeGreaterThan(1)
    expect(replay.filter((e) => e.seq > 0).every((e) => e.seq > 2)).toBe(true)
  })

  it("asks for approval, and retries the call once approved", async () => {
    const { client } = mock()
    await client.sessions.sendMessage({ sessionId: "web-vite", clientMsgId: "c", text: "please approve this" })
    const pending = await watchUntil(client, "web-vite", has("approval_pending"))
    const [approval] = await client.sessions.listApprovals({ sessionId: "web-vite" })
    expect(approval).toMatchObject({ tool: "bash", display: "git push -u origin HEAD" })
    expect(view(pending).events.at(-1)).toMatchObject({ status: "awaiting_approval" })
    await watchUntil(client, "web-vite", has("result", 2))
    expect((await client.sessions.getSession({ sessionId: "web-vite" })).state).toBe("awaiting_approval")

    await client.sessions.resolveApproval({ approvalId: approval.approvalId, approve: true })
    const events = await watchUntil(client, "web-vite", has("result", 3))
    const tools = view(events).events.filter((e) => e.kind === "tool_call")
    expect(tools.at(-2)).toMatchObject({ approval: { decision: "approved" } })
    expect(tools.at(-1)).toMatchObject({ status: "completed", input: { command: "git push -u origin HEAD" } })
    expect((await client.sessions.getSession({ sessionId: "web-vite" })).state).toBe("idle")
  })

  it("stops a turn, keeping what streamed as unsaved", async () => {
    const { client } = mock()
    await client.sessions.sendMessage({ sessionId: "web-vite", clientMsgId: "c", text: "hello" })
    await watchUntil(client, "web-vite", (events) => events.some((e) => e.payloadJson.includes("text_delta")))
    await client.sessions.interrupt({ sessionId: "web-vite" })
    const t = view(await watchUntil(client, "web-vite", has("turn_stopped")))
    expect(t.events.at(-1)?.kind).toBe("turn_stopped")
    expect(t.turnOpen).toBe(false)
    expect((await client.sessions.getSession({ sessionId: "web-vite" })).state).toBe("idle")
  })

  it("stops a session for good", async () => {
    const { client } = mock()
    await client.sessions.sendMessage({ sessionId: "web-vite", clientMsgId: "c", text: "hello" })
    await client.sessions.stopSession({ sessionId: "web-vite" })
    expect((await client.sessions.getSession({ sessionId: "web-vite" })).state).toBe("stopped")
    await expect(
      client.sessions.sendMessage({ sessionId: "web-vite", clientMsgId: "d", text: "again" }),
    ).rejects.toMatchObject({ code: "failed_precondition" })
  })

  it("lets a broken session go on without the lost history", async () => {
    const { client } = mock()
    await expect(client.sessions.recoverSession({ sessionId: "web-vite" })).rejects.toBeInstanceOf(ApiError)
    await client.sessions.recoverSession({ sessionId: "libvips" })
    const t = view(await watchUntil(client, "libvips", has("transcript_gap_accepted")))
    expect(t.events.at(-1)?.kind).toBe("history_gap")
    expect((await client.sessions.getSession({ sessionId: "libvips" })).state).toBe("idle")
  })

  it("hides sessions of other users' projects", async () => {
    const { client, state } = mock()
    state.cookie = "user-dev"
    expect(await client.sessions.listSessions({})).toEqual([])
    await expect(client.sessions.getSession({ sessionId: "web-vite" })).rejects.toMatchObject({ code: "not_found" })
  })

  it("keeps this tab's sessions when another tab changes projects", () => {
    const { state, adopt, onExternalChange } = mock()
    let calls = 0
    onExternalChange(() => calls++)
    const sessions = state.sessions
    const next = structuredClone({ ...state, sessions: [] })
    adopt(next)
    expect(calls).toBe(0)
    expect(state.sessions).toBe(sessions)
    const fewerLocations = structuredClone(next)
    fewerLocations.projects[0].locations.pop()
    adopt(fewerLocations)
    expect(calls).toBe(1)
  })
})

describe("the worker demo", () => {
  afterEach(() => {
    vi.useRealTimers()
  })
  const demo = () => {
    vi.useFakeTimers()
    vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
    const state = seedState()
    addWorkerDemo(state)
    return mock(state)
  }
  const find = (state: MockState, id: string) => state.sessions.find((s) => s.sessionId === id)!
  /** Follows the session's stream from now on: what it sends live, kind and payload. */
  const follow = (client: CircusClient, sessionId: string, afterSeq: number) => {
    const seen: [string, string][] = []
    const abort = new AbortController()
    void (async () => {
      for await (const e of client.sessions.watchSession({ sessionId, afterSeq, signal: abort.signal })) {
        if (e.kind !== "unspecified") seen.push([e.kind, e.payloadJson])
      }
    })().catch(() => {})
    return { seen, stop: () => abort.abort() }
  }

  it("stopping a session ends its lease: the mark clears, then the turn ends", async () => {
    const { client, state } = demo()
    const s = find(state, "worker-gone-running")
    expect(s.workerDisconnectedAtUnixMs).toBe(Date.now())
    const stream = follow(client, s.sessionId, s.events.at(-1)!.seq)
    await vi.advanceTimersByTimeAsync(10)
    const stopped = client.sessions.stopSession({ sessionId: s.sessionId })
    await vi.advanceTimersByTimeAsync(10)
    await stopped
    stream.stop()
    expect(s.workerDisconnectedAtUnixMs).toBe(0)
    expect(stream.seen).toEqual([
      ["session_state", JSON.stringify({ state: "stopped", reason: "user" })],
      ["worker", JSON.stringify({ disconnected_at_unix_ms: "0" })],
      ["turn_interrupted", JSON.stringify({ turn_no: String(s.turns), outcome: "interrupted" })],
    ])
  })

  it("ends the lease a minute after the disconnect, the mark clearing before the turn ends", async () => {
    const { client, state } = demo()
    const s = find(state, "worker-gone-running")
    const stream = follow(client, s.sessionId, s.events.at(-1)!.seq)
    await vi.advanceTimersByTimeAsync(59_999)
    expect(s.workerDisconnectedAtUnixMs).not.toBe(0)
    expect(s.state).toBe("running")
    await vi.advanceTimersByTimeAsync(1)
    stream.stop()
    expect(s.workerDisconnectedAtUnixMs).toBe(0)
    expect(s.state).toBe("idle")
    expect(stream.seen).toEqual([
      ["worker", JSON.stringify({ disconnected_at_unix_ms: "0" })],
      ["turn_interrupted", JSON.stringify({ turn_no: String(s.turns), outcome: "interrupted" })],
    ])
  })

  it("runs nothing while the worker is away, and the waiting message once the lease ended", async () => {
    const { client, state } = demo()
    const s = find(state, "worker-gone-idle")
    const turns = s.turns
    const sent = client.sessions.sendMessage({ sessionId: s.sessionId, clientMsgId: "m1", text: "hello" })
    await vi.advanceTimersByTimeAsync(10)
    await sent
    await vi.advanceTimersByTimeAsync(10_000)
    expect(s.turns).toBe(turns)
    expect(s.state).toBe("queued")
    await vi.advanceTimersByTimeAsync(50_000)
    expect(s.workerDisconnectedAtUnixMs).toBe(0)
    expect(s.turns).toBe(turns + 1)
  })
})
