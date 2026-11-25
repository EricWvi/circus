import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { oneSessionLedgerRead } from "./hooks"

/** A read the test settles by hand; `signal` is the one it was sent with. */
function pending<T>() {
  const read = { signal: undefined as AbortSignal | undefined, resolve: (_v: T) => {}, started: false }
  const fn = (signal: AbortSignal) => {
    read.signal = signal
    read.started = true
    return new Promise<T>((resolve) => (read.resolve = resolve))
  }
  return { read, fn }
}

const flush = () => vi.advanceTimersByTimeAsync(0)

beforeEach(() => vi.useFakeTimers())
afterEach(() => vi.useRealTimers())

describe("session ledger reads, one at a time", () => {
  it("sends the next read only once the one before is in", async () => {
    const first = pending<string>()
    const second = pending<string>()
    const a = oneSessionLedgerRead(first.fn)
    const b = oneSessionLedgerRead(second.fn)
    await flush()
    expect([first.read.started, second.read.started]).toEqual([true, false])
    first.read.resolve("a")
    await expect(a).resolves.toBe("a")
    await flush()
    expect(second.read.started).toBe(true)
    second.read.resolve("b")
    await expect(b).resolves.toBe("b")
  })

  it("gives up on a read that hangs, aborting it, and goes on with the next", async () => {
    const hung = pending<string>()
    const next = pending<string>()
    const a = oneSessionLedgerRead(hung.fn, undefined, 1_000)
    const aResult = a.catch((e: unknown) => e)
    const b = oneSessionLedgerRead(next.fn)
    await vi.advanceTimersByTimeAsync(1_000)
    expect(await aResult).toMatchObject({ code: "deadline_exceeded" })
    expect(hung.read.signal?.aborted).toBe(true)
    await flush()
    expect(next.read.started).toBe(true)
    next.read.resolve("b")
    await expect(b).resolves.toBe("b")
  })

  it("abandons a read cancelled while it runs, and doesn't send one cancelled while it waits", async () => {
    const running = pending<string>()
    const waiting = pending<string>()
    const last = pending<string>()
    const cancelRunning = new AbortController()
    const cancelWaiting = new AbortController()
    const a = oneSessionLedgerRead(running.fn, cancelRunning.signal).catch((e: unknown) => e)
    const b = oneSessionLedgerRead(waiting.fn, cancelWaiting.signal).catch((e: unknown) => e)
    const c = oneSessionLedgerRead(last.fn)
    await flush()
    cancelWaiting.abort(new Error("left"))
    cancelRunning.abort(new Error("closed"))
    expect(await a).toMatchObject({ message: "closed" })
    expect(running.read.signal?.aborted).toBe(true)
    expect(await b).toMatchObject({ message: "left" })
    expect(waiting.read.started).toBe(false)
    await flush()
    expect(last.read.started).toBe(true)
    last.read.resolve("c")
    await expect(c).resolves.toBe("c")
  })
})
