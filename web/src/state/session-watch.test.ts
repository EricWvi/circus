import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import type { RawSessionEvent, SessionEventKind } from "@/api/types"

import { FIRST_RETRY_MS, retryDelayMs } from "./connection"
import { createSessionWatch, HEALTHY_STREAM_MS, type SessionWatch } from "./session-watch"

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-21T10:00:00Z"))
})
afterEach(() => vi.useRealTimers())

const event = (seq: number, kind: SessionEventKind, payload: object | null = {}): RawSessionEvent => ({
  seq,
  tsUnixMs: Date.now(),
  kind,
  payloadJson: payload === null ? "" : JSON.stringify(payload),
  ephemeral: seq === 0,
  truncated: false,
})
const keepalive = event(0, "unspecified", null)
const input = (seq: number, id: string) =>
  event(seq, "input_accepted", { input_id: `in-${id}`, client_msg_id: id, priority: "normal", text: `text ${id}` })

/**
 * A fake WatchSession: each call opens a stream the test feeds with `send`
 * and ends with `fail` or `end`.
 */
function server() {
  const streams: {
    afterSeq: number
    signal: AbortSignal
    send: (...events: RawSessionEvent[]) => void
    fail: (error: unknown) => void
    end: () => void
  }[] = []
  return {
    streams,
    watchSession: ({ afterSeq, signal }: { sessionId: string; afterSeq: number; signal: AbortSignal }) => {
      const queue: (RawSessionEvent | { error: unknown } | null)[] = []
      let wake: (() => void) | null = null
      const push = (item: RawSessionEvent | { error: unknown } | null) => {
        queue.push(item)
        wake?.()
      }
      streams.push({
        afterSeq,
        signal,
        send: (...events) => events.forEach(push),
        fail: (error) => push({ error }),
        end: () => push(null),
      })
      return (async function* () {
        while (!signal.aborted) {
          const item = queue.shift()
          if (item === undefined) {
            await new Promise<void>((resolve) => {
              wake = resolve
              signal.addEventListener("abort", () => resolve(), { once: true })
            })
            continue
          }
          if (item === null) return
          if ("error" in item) throw item.error
          yield item
        }
      })()
    },
  }
}

const tick = () => vi.advanceTimersByTimeAsync(0)
let current: SessionWatch | undefined
afterEach(() => current?.stop())

function start(s: ReturnType<typeof server>, onBoundary = vi.fn()) {
  const watch = createSessionWatch({ sessions: s, sessionId: "s1", onBoundary })
  current = watch
  watch.start()
  return { watch, onBoundary }
}

const userTexts = (w: SessionWatch) =>
  w.get().transcript.events.flatMap((e) => (e.kind === "user_message" ? [e.text] : []))

describe("the session watch", () => {
  it("applies a batch at once and reports its boundaries once", async () => {
    const s = server()
    const { watch, onBoundary } = start(s)
    s.streams[0].send(keepalive, input(1, "a"), event(2, "turn_started", { turn_no: "1" }), input(3, "b"))
    await tick()
    expect(userTexts(watch)).toEqual(["text a", "text b"])
    expect(watch.get().transcript.turnOpen).toBe(true)
    expect(onBoundary).toHaveBeenCalledTimes(1)
    expect([...onBoundary.mock.calls[0][0]].sort()).toEqual(["input_accepted", "turn_started"])
  })

  it("reports ledger events as boundaries, live ones too", async () => {
    const s = server()
    const { onBoundary } = start(s)
    s.streams[0].send(keepalive)
    await tick()
    expect(onBoundary).not.toHaveBeenCalled()
    // A collection is announced live only.
    s.streams[0].send(event(0, "ledger", { event: "collected" }))
    await tick()
    expect(onBoundary).toHaveBeenCalledTimes(1)
    expect([...onBoundary.mock.calls[0][0]]).toEqual(["ledger"])
    // A merge is recorded.
    s.streams[0].send(event(1, "ledger", { event: "merged", numbers: { "D?a": 9 } }))
    await tick()
    expect(onBoundary).toHaveBeenCalledTimes(2)
    expect([...onBoundary.mock.calls[1][0]]).toEqual(["ledger"])
    // Other live events still aren't boundaries.
    s.streams[0].send(event(0, "queue", { queue_reason: "provider_full" }))
    await tick()
    expect(onBoundary).toHaveBeenCalledTimes(2)
  })

  it("reopens a stream that served a while at once, after the last durable event, showing nothing twice", async () => {
    const s = server()
    const { watch } = start(s)
    s.streams[0].send(keepalive, input(1, "a"), input(2, "b"))
    await tick()
    await vi.advanceTimersByTimeAsync(HEALTHY_STREAM_MS)
    // The server's hourly end of a stream.
    s.streams[0].fail(new ApiError("unavailable", "lifetime"))
    // At once: the next timer turn.
    await vi.advanceTimersByTimeAsync(1)
    expect(s.streams).toHaveLength(2)
    expect(s.streams[1].afterSeq).toBe(2)
    // A server may send an event again; it is applied once.
    s.streams[1].send(input(2, "b"), input(3, "c"))
    await tick()
    expect(watch.get().connection).toEqual({ kind: "connected" })
    expect(userTexts(watch)).toEqual(["text a", "text b", "text c"])
  })

  it("drops what streamed live when it reconnects", async () => {
    const s = server()
    const { watch } = start(s)
    const partial = (sdkEvent: object) => event(0, "assistant", { type: "stream_event", event: sdkEvent })
    s.streams[0].send(
      keepalive,
      partial({ type: "message_start", message: { id: "m" } }),
      partial({ type: "content_block_start", index: 0, content_block: { type: "text", text: "" } }),
      partial({ type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "Hal" } }),
    )
    await tick()
    expect(watch.get().transcript.events).toMatchObject([{ kind: "assistant_text", text: "Hal", streaming: true }])
    s.streams[0].end()
    await vi.advanceTimersByTimeAsync(retryDelayMs(2))
    s.streams[1].send(keepalive)
    await tick()
    expect(watch.get().transcript.events).toEqual([])
  })

  it("backs off from streams that keep breaking right after they open", async () => {
    const s = server()
    const { watch } = start(s)
    for (const [i, delay] of [retryDelayMs(2), retryDelayMs(3), retryDelayMs(4)].entries()) {
      s.streams[i].send(keepalive)
      await tick()
      s.streams[i].end()
      await tick()
      expect(watch.get().connection).toMatchObject({ kind: "reconnecting", attempt: i + 2 })
      await vi.advanceTimersByTimeAsync(delay - 1)
      expect(s.streams).toHaveLength(i + 1)
      await vi.advanceTimersByTimeAsync(1)
      expect(s.streams).toHaveLength(i + 2)
    }
    // One that serves a while resets the backoff.
    s.streams[3].send(keepalive)
    await vi.advanceTimersByTimeAsync(HEALTHY_STREAM_MS)
    s.streams[3].end()
    await vi.advanceTimersByTimeAsync(1)
    expect(s.streams).toHaveLength(5)
  })

  it("reads the session again a moment after a RESULT, and stops reporting once stopped", async () => {
    const s = server()
    const { watch, onBoundary } = start(s)
    s.streams[0].send(keepalive, event(1, "turn_started", { turn_no: "1" }), event(2, "result", {}))
    await tick()
    expect(onBoundary).toHaveBeenCalledTimes(1)
    await vi.advanceTimersByTimeAsync(1_000)
    expect(onBoundary).toHaveBeenCalledTimes(2)
    s.streams[0].send(event(3, "result", {}))
    await tick()
    expect(onBoundary).toHaveBeenCalledTimes(3)
    watch.stop()
    await vi.advanceTimersByTimeAsync(5_000)
    expect(onBoundary).toHaveBeenCalledTimes(3)
  })

  it("publishes nothing for keepalives", async () => {
    const s = server()
    const { watch } = start(s)
    s.streams[0].send(keepalive, input(1, "a"))
    await tick()
    const before = watch.get()
    s.streams[0].send(keepalive)
    await tick()
    expect(watch.get()).toBe(before)
  })

  it("keeps retrying while the server has too many streams open, and when a stream ends", async () => {
    const s = server()
    const { watch } = start(s)
    s.streams[0].fail(new ApiError("resource_exhausted", "too many streams"))
    await tick()
    await vi.advanceTimersByTimeAsync(FIRST_RETRY_MS)
    s.streams[1].fail(new ApiError("resource_exhausted", "too many streams"))
    await tick()
    await vi.advanceTimersByTimeAsync(retryDelayMs(2))
    expect(s.streams).toHaveLength(3)
    s.streams[2].send(keepalive)
    await tick()
    expect(watch.get().connection).toEqual({ kind: "connected" })
  })

  it("stops for good when the session is gone", async () => {
    const s = server()
    const { watch } = start(s)
    s.streams[0].send(keepalive, input(1, "a"))
    await tick()
    s.streams[0].fail(new ApiError("not_found", "deleted"))
    await tick()
    await vi.advanceTimersByTimeAsync(60_000)
    expect(s.streams).toHaveLength(1)
    expect(watch.get().gone).toBe(true)
    expect(userTexts(watch)).toEqual(["text a"])
  })

  it("stops for good once signed out", async () => {
    const s = server()
    const { watch } = start(s)
    s.streams[0].fail(new ApiError("unauthenticated", "signed out"))
    await tick()
    await vi.advanceTimersByTimeAsync(60_000)
    watch.start()
    expect(s.streams).toHaveLength(1)
    expect(watch.get().gone).toBe(false)
  })

  it("closes the stream on stop and resumes after the last event on start", async () => {
    const s = server()
    const { watch } = start(s)
    s.streams[0].send(keepalive, input(1, "a"))
    await tick()
    watch.stop()
    expect(s.streams[0].signal.aborted).toBe(true)
    await vi.advanceTimersByTimeAsync(60_000)
    expect(s.streams).toHaveLength(1)
    watch.start()
    expect(s.streams[1].afterSeq).toBe(1)
  })

  it("closes the stream when the browser goes offline and reconnects at once when it is back", async () => {
    const s = server()
    const { watch } = start(s)
    s.streams[0].send(keepalive)
    await tick()
    watch.setOnline(false)
    expect(s.streams[0].signal.aborted).toBe(true)
    expect(watch.get().connection.kind).toBe("offline")
    watch.setOnline(true)
    await tick()
    expect(s.streams).toHaveLength(2)
  })

  describe("live-only session state", () => {
    const worker = (at: string) => event(0, "worker", { disconnected_at_unix_ms: at })
    const queue = (reason: string) => event(0, "queue", { queue_reason: reason })

    it("keeps the latest worker disconnect time, stamped with when it arrived", async () => {
      const s = server()
      const { watch } = start(s)
      s.streams[0].send(keepalive, worker("1763719190000"))
      await tick()
      const arrived = Date.now()
      expect(watch.get().transcript.worker).toEqual({ disconnectedAtUnixMs: 1_763_719_190_000, receivedAtMs: arrived })
      await vi.advanceTimersByTimeAsync(1_000)
      s.streams[0].send(worker("0"))
      await tick()
      expect(watch.get().transcript.worker).toEqual({ disconnectedAtUnixMs: 0, receivedAtMs: arrived + 1_000 })
    })

    it("keeps the live values when the stream opens again, and asks for the session's read then", async () => {
      const s = server()
      const onReopen = vi.fn()
      const watch = createSessionWatch({ sessions: s, sessionId: "s1", onReopen })
      current = watch
      watch.start()
      s.streams[0].send(keepalive, worker("1763719190000"), queue("worker_offline"))
      await tick()
      const live = watch.get().transcript
      expect(onReopen).not.toHaveBeenCalled()
      await vi.advanceTimersByTimeAsync(HEALTHY_STREAM_MS)
      s.streams[0].fail(new ApiError("unavailable", "lifetime"))
      await vi.advanceTimersByTimeAsync(1)
      s.streams[1].send(keepalive)
      await tick()
      // Whatever the cached read says is older than these.
      expect(watch.get().transcript.worker).toBe(live.worker)
      expect(watch.get().transcript.queue).toBe(live.queue)
      expect(onReopen).toHaveBeenCalledTimes(1)
      // What the new stream says counts too.
      s.streams[1].send(worker("1763719200000"))
      await tick()
      expect(watch.get().transcript.worker?.disconnectedAtUnixMs).toBe(1_763_719_200_000)
    })

    it("knows the server's clock once a live event told it", async () => {
      const s = server()
      const { watch } = start(s)
      expect(watch.get().clockKnown).toBe(false)
      s.streams[0].send(keepalive)
      await tick()
      expect(watch.get().clockKnown).toBe(true)
    })
  })
})
