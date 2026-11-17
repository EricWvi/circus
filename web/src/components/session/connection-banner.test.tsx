// @vitest-environment happy-dom
import { cleanup, fireEvent, render, screen } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { Connection } from "@/state/connection"

import { ConnectionBanner } from "./connection-banner"

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

const lostAt = Date.parse("2026-11-17T11:59:57Z")
const reconnecting = (nextRetryAt: number | null): Connection => ({
  kind: "reconnecting",
  lostAt,
  attempt: 2,
  nextRetryAt,
})

describe("ConnectionBanner", () => {
  it("keeps the same live region node when the banner appears and goes", () => {
    const onRetryNow = vi.fn()
    const { rerender } = render(
      <ConnectionBanner connection={reconnecting(Date.now() + 4_000)} noticed={false} onRetryNow={onRetryNow} />,
    )
    const region = screen.getByRole("status")
    expect(region.textContent).toBe("")
    expect(screen.queryByRole("button")).toBeNull()

    rerender(<ConnectionBanner connection={reconnecting(Date.now() + 4_000)} noticed onRetryNow={onRetryNow} />)
    expect(screen.getByRole("status")).toBe(region)
    expect(region.textContent).toBe("Reconnecting…")
    expect(screen.getByText("Reconnecting… retrying in 4s")).toBeTruthy()

    rerender(<ConnectionBanner connection={{ kind: "connected" }} noticed={false} onRetryNow={onRetryNow} />)
    expect(screen.getByRole("status")).toBe(region)
    expect(region.textContent).toBe("")
  })

  it("ignores Retry now while an attempt runs, keeping the button focusable", () => {
    const onRetryNow = vi.fn()
    const { rerender } = render(
      <ConnectionBanner connection={reconnecting(null)} noticed onRetryNow={onRetryNow} />,
    )
    const button = screen.getByRole("button", { name: "Retry now" })
    button.focus()
    expect(button.getAttribute("aria-disabled")).toBe("true")
    expect(button.hasAttribute("disabled")).toBe(false)
    fireEvent.click(button)
    expect(onRetryNow).not.toHaveBeenCalled()
    expect(document.activeElement).toBe(button)

    // The attempt failed or was dropped: a countdown again, and the button works.
    rerender(<ConnectionBanner connection={reconnecting(Date.now())} noticed onRetryNow={onRetryNow} />)
    expect(button.hasAttribute("aria-disabled")).toBe(false)
    fireEvent.click(button)
    expect(onRetryNow).toHaveBeenCalledTimes(1)
    expect(document.activeElement).toBe(button)
  })
})
