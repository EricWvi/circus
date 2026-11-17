// @vitest-environment happy-dom
import { describe, expect, it } from "vitest"

import { lastInputWasKeyboard, lastInputWasTouch } from "./input-modality"

const pointer = (pointerType: string) => {
  const event = new Event("pointerdown", { bubbles: true })
  Object.assign(event, { pointerType })
  document.body.dispatchEvent(event)
}

describe("input modality", () => {
  it("tells touch from mouse and keyboard", () => {
    pointer("touch")
    expect(lastInputWasTouch()).toBe(true)
    pointer("mouse")
    expect(lastInputWasTouch()).toBe(false)
    pointer("touch")
    document.body.dispatchEvent(new KeyboardEvent("keydown", { key: "Tab", bubbles: true }))
    expect(lastInputWasKeyboard()).toBe(true)
    expect(lastInputWasTouch()).toBe(false)
  })
})
