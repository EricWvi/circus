// @vitest-environment happy-dom
import { cleanup, render, screen } from "@testing-library/react"
import { afterEach, describe, expect, it } from "vitest"

import { TooltipProvider } from "@/components/ui/tooltip"

import { DisabledReason } from "./disabled-reason"

afterEach(cleanup)

const view = (reason: string | null) => (
  <TooltipProvider>
    <DisabledReason reason={reason}>
      <button type="button" disabled={reason !== null}>
        Send
      </button>
    </DisabledReason>
  </TooltipProvider>
)

describe("DisabledReason", () => {
  it("keeps the control mounted when the reason clears, and moves focus on to it", () => {
    const { rerender } = render(view("Disconnected"))
    const button = screen.getByRole("button", { name: "Send" })
    const wrapper = screen.getByLabelText("Disconnected")
    wrapper.focus()
    expect(document.activeElement).toBe(wrapper)

    rerender(view(null))
    expect(screen.getByRole("button", { name: "Send" })).toBe(button)
    expect(document.activeElement).toBe(button)
    // Still focusable (but out of the tab order), so a browser doesn't blur it
    // before focus moves on to the control.
    expect(wrapper.getAttribute("tabindex")).toBe("-1")
    expect(wrapper.hasAttribute("aria-label")).toBe(false)
  })

  it("moves focus to the wrapper when the focused control gets a reason", () => {
    const { rerender } = render(view(null))
    const button = screen.getByRole("button", { name: "Send" })
    button.focus()
    rerender(view("Disconnected"))
    expect(document.activeElement).toBe(screen.getByLabelText("Disconnected"))
    expect(screen.getByRole("button", { name: "Send" })).toBe(button)
  })
})
