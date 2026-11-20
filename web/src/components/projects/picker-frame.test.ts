// @vitest-environment happy-dom
import { describe, expect, it } from "vitest"

import { framedAnchor, framedAnchorRect } from "./picker-frame"

const element = (rect: DOMRect) => {
  const el = document.createElement("div")
  el.getBoundingClientRect = () => rect
  return el
}

describe("framedAnchorRect", () => {
  it("takes the columns from the frame and the rows from the field", () => {
    const rect = framedAnchorRect(new DOMRect(400, 100, 576, 500), new DOMRect(632, 490, 280, 32))
    expect([rect.x, rect.y, rect.width, rect.height]).toEqual([400, 490, 576, 32])
  })
})

describe("framedAnchor", () => {
  it("measures both elements on every call", () => {
    let frameRect = new DOMRect(400, 100, 576, 500)
    const frame = element(new DOMRect())
    frame.getBoundingClientRect = () => frameRect
    const field = element(new DOMRect(632, 490, 280, 32))
    const anchor = framedAnchor({ current: frame }, field)
    expect(anchor.getBoundingClientRect().x).toBe(400)
    frameRect = new DOMRect(200, 100, 576, 500)
    expect(anchor.getBoundingClientRect().x).toBe(200)
    expect(anchor.contextElement).toBe(field)
  })

  it("falls back to the field once the frame is gone", () => {
    const field = element(new DOMRect(632, 490, 280, 32))
    const rect = framedAnchor({ current: null }, field).getBoundingClientRect()
    expect([rect.x, rect.y, rect.width, rect.height]).toEqual([632, 490, 280, 32])
  })
})
