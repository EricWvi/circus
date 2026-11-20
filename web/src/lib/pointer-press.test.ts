// @vitest-environment happy-dom
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { isPrimaryPress, MAX_PRESS_MS, watchPress } from "./pointer-press"

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  vi.useRealTimers()
})

describe("watchPress", () => {
  it("counts only the primary button, of any pointer type", () => {
    expect(isPrimaryPress({ button: 0 })).toBe(true)
    expect(isPrimaryPress({ button: 1 })).toBe(false)
    expect(isPrimaryPress({ button: 2 })).toBe(false)
  })

  it("ends a tick after the release, once", () => {
    const onEnd = vi.fn()
    watchPress(onEnd)
    document.dispatchEvent(new Event("pointerup"))
    expect(onEnd).not.toHaveBeenCalled()
    vi.advanceTimersByTime(0)
    expect(onEnd).toHaveBeenCalledTimes(1)
    document.dispatchEvent(new Event("pointercancel"))
    vi.advanceTimersByTime(MAX_PRESS_MS)
    expect(onEnd).toHaveBeenCalledTimes(1)
  })

  it.each([
    ["a context menu opens", () => document.dispatchEvent(new Event("contextmenu"))],
    ["the window loses focus", () => window.dispatchEvent(new Event("blur"))],
  ])("ends at once when %s", (_, happen) => {
    const onEnd = vi.fn()
    watchPress(onEnd)
    happen()
    expect(onEnd).toHaveBeenCalledTimes(1)
  })

  it("ends after the time limit without a release", () => {
    const onEnd = vi.fn()
    watchPress(onEnd)
    vi.advanceTimersByTime(MAX_PRESS_MS - 1)
    expect(onEnd).not.toHaveBeenCalled()
    vi.advanceTimersByTime(1)
    expect(onEnd).toHaveBeenCalledTimes(1)
  })
})
