import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import {
  createConnectionController,
  lossNoticed,
  retryDelayMs,
  type ConnectionController,
} from "./connection"
import { createSimulatedLink } from "./simulated-link"

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
})
afterEach(() => vi.useRealTimers())

/** An attempt the test resolves by hand. */
function manualAttempts() {
  const pending: ((ok: boolean) => void)[] = []
  return {
    attempt: vi.fn(() => new Promise<boolean>((resolve) => pending.push(resolve))),
    resolve: (ok: boolean) => pending.shift()?.(ok),
    get count() {
      return pending.length
    },
  }
}

/** Lets resolved attempt promises settle. */
const flush = async () => {
  await Promise.resolve()
  await Promise.resolve()
}

let c: ConnectionController
afterEach(() => c?.dispose())

describe("retryDelayMs", () => {
  it("backs off 1, 2, 4, 8… seconds, capped at 30", () => {
    expect([1, 2, 3, 4, 5, 6, 7, 10].map(retryDelayMs)).toEqual([
      1_000, 2_000, 4_000, 8_000, 16_000, 30_000, 30_000, 30_000,
    ])
  })
})

describe("connection core", () => {
  it("retries on the backoff schedule, with no countdown while an attempt runs", async () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt })
    const t0 = Date.now()
    c.lost()
    expect(c.get()).toEqual({ kind: "reconnecting", lostAt: t0, attempt: 1, nextRetryAt: t0 + 1_000 })
    vi.advanceTimersByTime(1_000)
    expect(c.get()).toEqual({ kind: "reconnecting", lostAt: t0, attempt: 1, nextRetryAt: null })
    a.resolve(false)
    await flush()
    expect(c.get()).toMatchObject({ attempt: 2, nextRetryAt: Date.now() + 2_000, lostAt: t0 })
    vi.advanceTimersByTime(2_000)
    a.resolve(false)
    await flush()
    expect(c.get()).toMatchObject({ attempt: 3, nextRetryAt: Date.now() + 4_000 })
    vi.advanceTimersByTime(4_000)
    a.resolve(true)
    await flush()
    expect(c.get()).toEqual({ kind: "connected" })
  })

  it("caps the wait at 30 seconds", async () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt })
    c.lost()
    for (const delay of [1_000, 2_000, 4_000, 8_000, 16_000, 30_000]) {
      vi.advanceTimersByTime(delay)
      a.resolve(false)
      await flush()
    }
    expect(c.get()).toMatchObject({ attempt: 7, nextRetryAt: Date.now() + 30_000 })
  })

  it("keeps the schedule after a failed manual retry and ignores retries while one runs", async () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt })
    c.lost()
    vi.advanceTimersByTime(1_000)
    a.resolve(false)
    await flush()
    const scheduled = c.get()
    expect(scheduled).toMatchObject({ attempt: 2, nextRetryAt: Date.now() + 2_000 })

    vi.advanceTimersByTime(500)
    c.retryNow()
    expect(c.get()).toMatchObject({ attempt: 2, nextRetryAt: null })
    c.retryNow() // ignored: an attempt runs
    c.retryNow()
    expect(a.attempt).toHaveBeenCalledTimes(2)
    a.resolve(false)
    await flush()
    expect(c.get()).toEqual(scheduled)

    // Clicking again and again never moves the schedule forward.
    for (let i = 0; i < 4; i++) {
      c.retryNow()
      a.resolve(false)
      await flush()
    }
    expect(c.get()).toEqual(scheduled)
  })

  it("connects on a successful manual retry", async () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt })
    c.lost()
    c.retryNow()
    a.resolve(true)
    await flush()
    expect(c.get()).toEqual({ kind: "connected" })
    vi.advanceTimersByTime(60_000)
    expect(a.attempt).toHaveBeenCalledTimes(1) // the cancelled timer never fired
  })

  it("stops waiting when the stream resumes by itself, and ignores the running attempt", async () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt })
    c.lost()
    c.resumed()
    vi.advanceTimersByTime(60_000)
    expect(a.attempt).not.toHaveBeenCalled()

    c.lost()
    vi.advanceTimersByTime(1_000)
    c.resumed()
    c.lost() // lost again before the old attempt answers
    a.resolve(true) // the stale success must not connect
    await flush()
    expect(c.get()).toMatchObject({ kind: "reconnecting", attempt: 1 })
  })

  it("resumes the schedule on start after a dispose while waiting", async () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt })
    const listener = vi.fn()
    c.subscribe(listener)
    c.lost()
    c.dispose()
    vi.advanceTimersByTime(5_000)
    expect(a.attempt).not.toHaveBeenCalled()
    c.lost() // still lost: nothing new, and nothing stuck either
    vi.advanceTimersByTime(0)
    expect(a.attempt).toHaveBeenCalledTimes(1) // overdue, so it runs at once
    a.resolve(false)
    await flush()
    expect(c.get()).toMatchObject({ kind: "reconnecting", attempt: 2, nextRetryAt: Date.now() + 2_000 })
  })

  it("publishes a dispose during an attempt and runs that attempt again on start", async () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt })
    const listener = vi.fn()
    c.subscribe(listener)
    c.lost()
    vi.advanceTimersByTime(1_000)
    expect(c.get()).toMatchObject({ nextRetryAt: null })
    listener.mockClear()
    c.dispose()
    // Published: the view shows a countdown again (due now), not "attempting" forever.
    expect(listener).toHaveBeenCalled()
    expect(c.get()).toMatchObject({ kind: "reconnecting", attempt: 1, nextRetryAt: Date.now() })
    a.resolve(true) // the dropped attempt answers late: ignored
    await flush()
    expect(c.get()).toMatchObject({ kind: "reconnecting" })

    c.start()
    vi.advanceTimersByTime(0)
    expect(a.attempt).toHaveBeenCalledTimes(2)
    a.resolve(false)
    await flush()
    expect(c.get()).toMatchObject({ attempt: 2, nextRetryAt: Date.now() + 2_000 })
    c.retryNow()
    expect(a.attempt).toHaveBeenCalledTimes(3)
  })

  it("stays closed whatever calls come late", async () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt })
    c.lost()
    vi.advanceTimersByTime(1_000) // attempt 1 running
    c.close()
    a.resolve(true) // its answer is ignored
    await flush()
    c.lost()
    c.retryNow()
    c.start()
    c.setOnline(false)
    c.setOnline(true)
    c.resumed()
    vi.advanceTimersByTime(60_000)
    await flush()
    expect(a.attempt).toHaveBeenCalledTimes(1)
    expect(c.get()).toMatchObject({ kind: "reconnecting", attempt: 1 })
  })

  it("cancels everything on dispose", async () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt })
    const listener = vi.fn()
    c.subscribe(listener)
    c.lost()
    vi.advanceTimersByTime(1_000)
    listener.mockClear()
    c.dispose()
    a.resolve(true)
    await flush()
    vi.advanceTimersByTime(60_000)
    expect(a.attempt).toHaveBeenCalledTimes(1)
    // Only dispose's own publish; the late answer and the timers did nothing.
    expect(listener).toHaveBeenCalledTimes(1)
  })

  it("counts going offline as losing the stream and reconnects when back online", async () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt })
    const t0 = Date.now()
    c.setOnline(false)
    expect(c.get()).toEqual({ kind: "offline", lostAt: t0 })
    vi.advanceTimersByTime(60_000)
    expect(a.attempt).not.toHaveBeenCalled() // no attempts while offline
    c.retryNow()
    expect(a.attempt).not.toHaveBeenCalled()

    c.setOnline(true)
    vi.advanceTimersByTime(0)
    expect(c.get()).toEqual({ kind: "reconnecting", lostAt: t0, attempt: 1, nextRetryAt: null })
    a.resolve(true)
    await flush()
    expect(c.get()).toEqual({ kind: "connected" })
  })

  it("goes offline during an outage without losing when it started", async () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt })
    const t0 = Date.now()
    c.lost()
    vi.advanceTimersByTime(1_000) // attempt 1 running
    c.setOnline(false)
    expect(c.get()).toEqual({ kind: "offline", lostAt: t0 })
    a.resolve(true) // the attempt from before going offline doesn't count
    await flush()
    expect(c.get()).toEqual({ kind: "offline", lostAt: t0 })
    c.setOnline(true)
    vi.advanceTimersByTime(0)
    a.resolve(false)
    await flush()
    expect(c.get()).toMatchObject({ kind: "reconnecting", lostAt: t0, attempt: 2 })
  })

  it("starts lost when the browser starts offline", () => {
    const a = manualAttempts()
    c = createConnectionController({ attempt: a.attempt, online: false })
    expect(c.get()).toEqual({ kind: "offline", lostAt: Date.now() })
  })
})

describe("lossNoticed", () => {
  it("shows a loss only once it has lasted 2 seconds", () => {
    const lostAt = Date.now()
    const lost = { kind: "reconnecting", lostAt, attempt: 1, nextRetryAt: null } as const
    expect(lossNoticed(lost, lostAt + 1_999)).toBe(false)
    expect(lossNoticed(lost, lostAt + 2_000)).toBe(true)
    expect(lossNoticed({ kind: "offline", lostAt }, lostAt + 5_000)).toBe(true)
    expect(lossNoticed({ kind: "connected" }, lostAt + 5_000)).toBe(false)
  })
})

describe("simulated link", () => {
  it("fails attempts until the outage is over", async () => {
    const link = createSimulatedLink({ latencyMs: 300 })
    c = createConnectionController({ attempt: link.attempt })
    link.dropFor(10_000)
    c.lost()
    for (const delay of [1_000, 2_000, 4_000]) {
      await vi.advanceTimersByTimeAsync(delay + 300)
      expect(c.get().kind).toBe("reconnecting")
    }
    // Attempt 4 runs at 15.9 s, after the 10 s outage.
    await vi.advanceTimersByTimeAsync(8_000 + 300)
    expect(c.get()).toEqual({ kind: "connected" })
  })
})
