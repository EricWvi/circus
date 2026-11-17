// @vitest-environment happy-dom
import { act, cleanup } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { useSessionList } from "@/api/hooks"
import { queryKeys } from "@/api/query-client"
import type { SessionSummary } from "@/api/types"
import type { SessionEvent } from "@/lib/types"
import { advance, renderWithBackend, testTiming } from "@/test/render"

import type { Connection } from "./connection"
import {
  useConversations,
  useConversationSimulation,
  type ConversationSimulation,
  type ConversationsContextValue,
} from "./conversations-context"

const SESSION = "golangci-v2"
const PROJECT = "toolbox"

interface Seen {
  conversations: ConversationsContextValue
  simulation: ConversationSimulation
  connection: Connection
  events: SessionEvent[]
  /** The session as the (held) session lists show it. */
  all?: SessionSummary
  inProject?: SessionSummary
}

type SeenRef = { current?: Seen }

/** Records what a page would see, on every render. */
function Probe({ seen, withProjectList = true }: { seen: SeenRef; withProjectList?: boolean }) {
  const conversations = useConversations()
  const simulation = useConversationSimulation()!
  const all = useSessionList().data?.find((s) => s.sessionId === SESSION)
  record(seen, {
    conversations,
    simulation,
    connection: conversations.connection,
    events: conversations.events(SESSION),
    all,
  })
  return withProjectList ? <ProjectList seen={seen} /> : null
}

function ProjectList({ seen }: { seen: SeenRef }) {
  const inProject = useSessionList(PROJECT).data?.find((s) => s.sessionId === SESSION)
  record(seen, { inProject })
  return null
}

/** Merges what a probe saw into the test's record. */
function record(seen: SeenRef, values: Partial<Seen>) {
  seen.current = { ...seen.current, ...values } as Seen
}

const lastText = (events: SessionEvent[]) => events.findLast((e) => e.kind === "assistant_text")
const now = (seen: SeenRef) => seen.current!

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.restoreAllMocks()
})

async function sendAndStream(seen: SeenRef, ms: number) {
  act(() => now(seen).conversations.send(SESSION, "Check the lint config", []))
  await advance(ms)
}

describe("the view while the connection is down", () => {
  it("changes nothing visible during the grace, then drops partially streamed text", async () => {
    const seen: SeenRef = {}
    await renderWithBackend(<Probe seen={seen} />, { timing: { streamMs: 4_000 } })
    await sendAndStream(seen, 600) // streaming
    const partial = lastText(now(seen).events)
    expect(partial).toMatchObject({ streaming: true })

    act(() => now(seen).simulation.dropConnection())
    await advance(1_500)
    // A blip: the partial text is still there, frozen.
    expect(lastText(now(seen).events)).toEqual(partial)
    await advance(600)
    // The loss is noticed: the ephemeral text goes, the rest stays.
    expect(now(seen).events.some((e) => e.id === partial!.id)).toBe(false)
    expect(now(seen).events.at(-1)).toMatchObject({ kind: "user_message", text: "Check the lint config" })
  })

  it("catches up at once on reconnect, and an unfinished reply resumes streaming", async () => {
    const seen: SeenRef = {}
    // About one word per 400 ms: the reply outlasts the outage.
    await renderWithBackend(<Probe seen={seen} />, {
      timing: { streamMs: 60_000, streamTickMs: 400, outageMs: 2_500 },
    })
    await sendAndStream(seen, 600)
    act(() => now(seen).simulation.dropConnection())
    await advance(2_500) // noticed; the server keeps streaming
    expect(now(seen).events.some((e) => e.kind === "assistant_text" && e.streaming)).toBe(false)

    // Attempts at 1 s and 3 s fail (outage until 2.5 s), the one at 7 s works.
    await advance(5_000)
    expect(now(seen).connection.kind).toBe("connected")
    const caughtUp = lastText(now(seen).events)
    expect(caughtUp).toMatchObject({ streaming: true })
    const length = caughtUp!.kind === "assistant_text" ? caughtUp!.text.length : 0
    await advance(1_000)
    const later = lastText(now(seen).events)
    expect(later!.kind === "assistant_text" && later!.text.length).toBeGreaterThan(length)
    await advance(60_000)
    expect(lastText(now(seen).events)).toMatchObject({ streaming: false })
  })

  it("holds the session lists, refetches included, until the connection is back", async () => {
    const seen: SeenRef = {}
    const { queryClient } = await renderWithBackend(<Probe seen={seen} withProjectList={false} />)
    await sendAndStream(seen, 300)
    expect(now(seen).all?.state).toBe("running")

    act(() => now(seen).simulation.dropConnection())
    await advance(2_000) // the reply finishes on the server meanwhile
    expect(now(seen).simulation.liveState(SESSION)).toBe("idle")
    // A refetch (window focus, a new page) brings the newer state into the cache…
    act(() => void queryClient.refetchQueries({ queryKey: queryKeys.allSessions }))
    await advance(0)
    // …but the view still shows what came with the frozen conversation.
    expect(now(seen).all?.state).toBe("running")

    await advance(testTiming.outageMs + 5_000)
    expect(now(seen).connection.kind).toBe("connected")
    expect(now(seen).all?.state).toBe("idle")
  })

  it("holds a list first fetched during the outage", async () => {
    const seen: SeenRef = {}
    const { rerender } = await renderWithBackend(<Probe seen={seen} withProjectList={false} />)
    await sendAndStream(seen, 300)
    act(() => now(seen).simulation.dropConnection())
    await advance(2_000)
    rerender(<Probe seen={seen} withProjectList />)
    await advance(50)
    expect(now(seen).inProject?.state).toBe("running")

    await advance(testTiming.outageMs + 5_000)
    expect(now(seen).connection.kind).toBe("connected")
    expect(now(seen).all?.state).toBe("idle")
    expect(now(seen).inProject?.state).toBe("idle")
  })

  it("hides text a turn left unsaved while the viewer was away, keeping the marker", async () => {
    const seen: SeenRef = {}
    await renderWithBackend(<Probe seen={seen} />, { timing: { streamMs: 8_000 } })
    await sendAndStream(seen, 600)
    act(() => now(seen).simulation.dropConnection())
    await advance(2_500)
    act(() => now(seen).simulation.killWorker(SESSION))
    await advance(10_000)
    expect(now(seen).connection.kind).toBe("connected")
    expect(now(seen).all?.state).toBe("interrupted")
    expect(now(seen).events.some((e) => e.kind === "assistant_text" && e.unsaved)).toBe(false)
    expect(now(seen).events.at(-1)).toMatchObject({ kind: "turn_interrupted" })
  })

  it("keeps unsaved text the viewer did see", async () => {
    const seen: SeenRef = {}
    await renderWithBackend(<Probe seen={seen} />, { timing: { streamMs: 8_000 } })
    await sendAndStream(seen, 600)
    act(() => now(seen).simulation.killWorker(SESSION))
    await advance(0)
    expect(lastText(now(seen).events)).toMatchObject({ unsaved: true })
  })
})

describe("starting offline", () => {
  it("starts disconnected and frozen, and catches up once online", async () => {
    vi.spyOn(navigator, "onLine", "get").mockReturnValue(false)
    const seen: SeenRef = {}
    await renderWithBackend(<Probe seen={seen} withProjectList={false} />)
    expect(now(seen).connection.kind).toBe("offline")
    const before = now(seen).events
    act(() => now(seen).simulation.breakTranscript(SESSION))
    await advance(100)
    expect(now(seen).simulation.liveState(SESSION)).toBe("broken")
    expect(now(seen).all?.state).toBe("idle")
    expect(now(seen).events).toEqual(before)

    vi.spyOn(navigator, "onLine", "get").mockReturnValue(true)
    act(() => {
      window.dispatchEvent(new Event("online"))
    })
    await advance(testTiming.attemptLatencyMs + 50)
    expect(now(seen).connection.kind).toBe("connected")
    expect(now(seen).all?.state).toBe("broken")
  })
})

describe("simulation guards", () => {
  it("kills a worker only while a turn runs, and breaks only idle or interrupted sessions", async () => {
    const seen: SeenRef = {}
    await renderWithBackend(<Probe seen={seen} />)
    await advance(50)
    expect(now(seen).simulation.liveState(SESSION)).toBe("idle")
    const before = now(seen).events
    act(() => now(seen).simulation.killWorker(SESSION)) // idle: nothing to kill
    await advance(0)
    expect(now(seen).events).toEqual(before)
    expect(now(seen).all?.state).toBe("idle")

    await sendAndStream(seen, 300)
    act(() => now(seen).simulation.breakTranscript(SESSION)) // running: not breakable
    await advance(0)
    expect(now(seen).all?.state).toBe("running")
  })
})

describe("retrying an interrupted turn", () => {
  it("sends the turn again once, however often it is asked", async () => {
    const seen: SeenRef = {}
    await renderWithBackend(<Probe seen={seen} />)
    await sendAndStream(seen, 300)
    act(() => now(seen).simulation.killWorker(SESSION))
    await advance(0)
    const users = () => now(seen).events.filter((e) => e.kind === "user_message").length
    const before = users()
    act(() => {
      now(seen).conversations.retryTurn(SESSION)
      now(seen).conversations.retryTurn(SESSION)
    })
    act(() => now(seen).conversations.retryTurn(SESSION))
    await advance(0)
    expect(users()).toBe(before + 1)
    expect(now(seen).events.at(-1)).toMatchObject({ kind: "user_message", text: "Check the lint config" })
    expect(now(seen).all?.state).toBe("running")
  })
})
