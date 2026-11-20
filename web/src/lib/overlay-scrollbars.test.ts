// @vitest-environment happy-dom
import { expect, it, onTestFinished, vi } from "vitest"
import { installOverlayScrollbars, thumbGeometry } from "./overlay-scrollbars"

it("places the thumb in proportion to the scroll and keeps a grabbable size", () => {
  expect(thumbGeometry(800, 3200, 1200)).toEqual({
    size: 200,
    offset: 300,
    ratio: 4,
  })
  expect(thumbGeometry(800, 80000, 0)).toEqual({
    size: 32,
    offset: 0,
    ratio: 79200 / 768,
  })
  // Overscroll past either end pins the thumb instead of running off the track.
  expect(thumbGeometry(800, 3200, 5000)?.offset).toBe(600)
  expect(thumbGeometry(800, 3200, -40)?.offset).toBe(0)
})

it("draws nothing when there is nothing to scroll", () => {
  expect(thumbGeometry(800, 800, 0)).toBeNull()
})

it("leaves overlay scrollbars to the platform", () => {
  const uninstall = installOverlayScrollbars()
  expect(document.documentElement.dataset.scrollbar).toBeUndefined()
  uninstall()
})

// A box that scrolls under classic scrollbars, with the thumbs installed and frames run by hand:
// happy-dom does no layout and reports no scrollbar width, so the probe and the box's geometry are
// given. Every width reads 83 of 100, so the box's track is 83 wide.
function classicScroller() {
  vi.spyOn(HTMLElement.prototype, "offsetWidth", "get").mockReturnValue(100)
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(83)
  const frames: FrameRequestCallback[] = []
  vi.spyOn(window, "requestAnimationFrame").mockImplementation((callback) => {
    frames.push(callback)
    return frames.length
  })
  const runFrame = () => frames.splice(0).forEach((callback) => callback(0))
  const box = document.createElement("div")
  box.style.overflowY = "auto"
  document.body.append(box)
  let rect = new DOMRect(100, 100, 400, 600)
  vi.spyOn(box, "getBoundingClientRect").mockImplementation(() => rect)
  Object.defineProperty(box, "clientHeight", { value: 600 })
  Object.defineProperty(box, "scrollHeight", { value: 1800 })
  const moveTo = (next: DOMRect) => {
    rect = next
  }
  const uninstall = installOverlayScrollbars()
  onTestFinished(() => {
    uninstall()
    box.remove()
    vi.restoreAllMocks()
  })
  return { box, runFrame, moveTo }
}

// A shown thumb, as the page sees it: where it hangs, or that it is gone.
function shownThumb() {
  const node = document.querySelector<HTMLElement>(".overlay-scrollbar")
  if (!node || node.hidden || node.dataset.visible === undefined) return null
  return { left: node.style.left, top: node.style.top }
}

it("keeps a shown thumb on its box as the box moves without scrolling", () => {
  const { box, runFrame, moveTo } = classicScroller()
  box.scrollTop = 300
  box.dispatchEvent(new Event("scroll"))
  runFrame()
  expect(shownThumb()).toEqual({ left: "183px", top: "200px" })
  moveTo(new DOMRect(40, 60, 400, 600))
  runFrame()
  expect(shownThumb()).toEqual({ left: "123px", top: "160px" })
})

// A scrolled box that clips its overflow, as it may while animating shut: its thumb must not
// stay behind where the box was.
it("takes a shown thumb away once its box stops being scrollable", () => {
  const { box, runFrame } = classicScroller()
  box.scrollTop = 300
  box.dispatchEvent(new Event("scroll"))
  runFrame()
  expect(shownThumb()).not.toBeNull()
  box.style.overflowY = "hidden"
  runFrame()
  expect(shownThumb()).toBeNull()
})

it("gives no thumb to a box that hides its scrollbar everywhere", () => {
  const { box, runFrame } = classicScroller()
  box.classList.add("no-scrollbar")
  box.scrollTop = 300
  box.dispatchEvent(new Event("scroll"))
  runFrame()
  expect(shownThumb()).toBeNull()
})
