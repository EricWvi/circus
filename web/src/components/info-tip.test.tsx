// @vitest-environment happy-dom
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react"
import { afterEach, describe, expect, it } from "vitest"

import { TooltipProvider } from "@/components/ui/tooltip"

import { InfoTip } from "./info-tip"

afterEach(cleanup)

const view = () =>
  render(
    <TooltipProvider>
      <InfoTip content="The details">
        <button type="button">Info</button>
      </InfoTip>
    </TooltipProvider>,
  )

const shown = () => screen.queryAllByText("The details").length > 0
const press = (button: HTMLElement, pointerType: string) => {
  fireEvent.pointerDown(button, { pointerType })
  fireEvent.click(button, { detail: 1 })
}

describe("InfoTip", () => {
  it("stays open on a mouse click, and closes when the mouse leaves", async () => {
    view()
    const button = screen.getByRole("button", { name: "Info" })
    press(button, "mouse")
    expect(shown()).toBe(true)
    press(button, "mouse")
    expect(shown()).toBe(true)
    await act(async () => {
      fireEvent.pointerLeave(button, { pointerType: "mouse" })
    })
    expect(shown()).toBe(false)
  })

  it("toggles on a tap, and ignores the finger leaving", async () => {
    view()
    const button = screen.getByRole("button", { name: "Info" })
    press(button, "touch")
    expect(shown()).toBe(true)
    fireEvent.pointerLeave(button, { pointerType: "touch" })
    expect(shown()).toBe(true)
    await act(async () => {
      press(button, "touch")
    })
    expect(shown()).toBe(false)
  })

  it("opens from the keyboard", () => {
    view()
    fireEvent.click(screen.getByRole("button", { name: "Info" }), { detail: 0 })
    expect(shown()).toBe(true)
  })
})
