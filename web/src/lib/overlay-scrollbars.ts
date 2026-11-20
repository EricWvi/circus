const minThumb = 32
const idleMs = 900
// Thumbs are thin; the pointer may land this far outside one and still take it.
const grabSlop = 3

export type ThumbGeometry = {
  size: number
  offset: number
  // Content pixels moved per pixel of thumb travel, so a drag tracks the pointer exactly.
  ratio: number
}

// Where a thumb sits along its track for a scroll position; null when there is nothing to scroll.
export function thumbGeometry(
  track: number,
  content: number,
  scroll: number,
): ThumbGeometry | null {
  const range = content - track
  if (range <= 0) return null
  const size = Math.max(minThumb, (track * track) / content)
  const travel = track - size
  const offset = (Math.min(Math.max(scroll, 0), range) / range) * travel
  return { size, offset, ratio: range / travel }
}

type Axis = "vertical" | "horizontal"

type Thumb = {
  scroller: Element
  axis: Axis
  node: HTMLDivElement
  idle?: number
}

type Drag = {
  thumb: Thumb
  pointer: number
  from: number
  scroll: number
}

// Classic scrollbars take width from the layout; overlay ones (macOS, phones) report none.
function classicScrollbars() {
  const probe = document.createElement("div")
  probe.style.cssText =
    "position:absolute;top:-999px;width:100px;height:100px;overflow:scroll"
  document.body.append(probe)
  const width = probe.offsetWidth - probe.clientWidth
  probe.remove()
  return width > 0
}

// The visible box a scroller's thumb runs along, in viewport coordinates.
function trackOf(scroller: Element) {
  if (scroller === document.scrollingElement)
    return {
      left: 0,
      top: 0,
      width: window.innerWidth,
      height: window.innerHeight,
    }
  const rect = scroller.getBoundingClientRect()
  return {
    left: rect.left + scroller.clientLeft,
    top: rect.top + scroller.clientTop,
    width: scroller.clientWidth,
    height: scroller.clientHeight,
  }
}

// Scrolls that the reader cannot see as such, like a clipped box moved by scrollIntoView, get
// no thumb: only the page and boxes that declare themselves scrollable on that axis do. Boxes
// that hide their scrollbar on every platform (the `no-scrollbar` utility, as in the sidebar)
// get none either.
function userScrollable(scroller: Element, axis: Axis) {
  if (scroller === document.scrollingElement) return true
  if (scroller.classList.contains("no-scrollbar")) return false
  const style = getComputedStyle(scroller)
  const overflow = axis === "vertical" ? style.overflowY : style.overflowX
  return overflow === "auto" || overflow === "scroll"
}

function scrollPosition(scroller: Element, axis: Axis) {
  return axis === "vertical" ? scroller.scrollTop : scroller.scrollLeft
}

/*
 * Windows keeps full-width scrollbars on the page and on every scrolling box, taking room from
 * the content. Where scrollbars take width, all native ones are hidden and a thin thumb stands in
 * for whichever box is moving: shown while it scrolls, fading once it stops, draggable while
 * shown. Overlay platforms already behave this way, so they keep their own.
 *
 * Thumbs live in one layer on the body and never take the pointer themselves. A thumb over a
 * dialog or a menu would otherwise be "outside" it, and Radix would dismiss it. Instead the window catches a press over a shown thumb before
 * anything beneath sees it, and the drag runs from there. Returns the uninstaller.
 */
export function installOverlayScrollbars() {
  if (!classicScrollbars()) return () => {}
  const root = document.documentElement
  root.dataset.scrollbar = "thin"
  const thumbs: Thumb[] = []
  const seen = new WeakMap<Element, { top: number; left: number }>()
  let frame = 0
  let drag: Drag | null = null
  let swallowClick = false

  const place = (thumb: Thumb) => {
    const { scroller, axis, node } = thumb
    const track = trackOf(scroller)
    const vertical = axis === "vertical"
    // A box that stops being scrollable, like one clipping its overflow as it animates shut,
    // takes its thumb away at once instead of leaving it where the box was.
    const geometry = userScrollable(scroller, axis)
      ? thumbGeometry(
          vertical ? track.height : track.width,
          vertical ? scroller.scrollHeight : scroller.scrollWidth,
          scrollPosition(scroller, axis),
        )
      : null
    node.hidden = geometry === null
    if (!geometry) return geometry
    // The thumb hangs inside the far edge of the track; CSS sets its thickness and inset.
    node.style.left = `${vertical ? track.left + track.width : track.left + geometry.offset}px`
    node.style.top = `${vertical ? track.top + geometry.offset : track.top + track.height}px`
    node.style[vertical ? "height" : "width"] = `${geometry.size}px`
    return geometry
  }

  const placeAll = () => {
    frame = 0
    for (let index = thumbs.length - 1; index >= 0; index -= 1) {
      const thumb = thumbs[index]
      if (thumb.scroller.isConnected) {
        place(thumb)
        continue
      }
      // The box went away with its view; its thumb goes with it.
      window.clearTimeout(thumb.idle)
      thumb.node.remove()
      thumbs.splice(index, 1)
    }
    // Boxes also move and resize without scrolling, as an animated panel does, and nothing else
    // reports that; a shown thumb follows its box each frame until it fades.
    if (
      thumbs.some(
        ({ node }) => node.dataset.visible !== undefined && !node.hidden,
      )
    )
      schedule()
  }
  const schedule = () => {
    if (!frame) frame = requestAnimationFrame(placeAll)
  }

  // Hover and drag hold a thumb open past the idle timeout; it fades once both let go.
  const fadeLater = (thumb: Thumb) => {
    window.clearTimeout(thumb.idle)
    thumb.idle = window.setTimeout(() => {
      if (drag?.thumb === thumb || thumb.node.dataset.hover !== undefined)
        return
      delete thumb.node.dataset.visible
    }, idleMs)
  }

  const show = (scroller: Element, axis: Axis) => {
    let thumb = thumbs.find(
      (candidate) => candidate.scroller === scroller && candidate.axis === axis,
    )
    if (!thumb) {
      const node = document.createElement("div")
      node.className = "overlay-scrollbar"
      node.dataset.axis = axis
      node.setAttribute("aria-hidden", "true")
      document.body.append(node)
      thumb = { scroller, axis, node }
      thumbs.push(thumb)
      place(thumb)
    }
    thumb.node.dataset.visible = ""
    fadeLater(thumb)
  }

  const onScroll = (event: Event) => {
    const scroller =
      event.target === document
        ? document.scrollingElement
        : (event.target as Element)
    if (!scroller) return
    // A box first seen scrolling is taken to have started at its origin, as nearly all do.
    const before = seen.get(scroller) ?? { top: 0, left: 0 }
    const now = { top: scroller.scrollTop, left: scroller.scrollLeft }
    seen.set(scroller, now)
    if (before.top !== now.top && userScrollable(scroller, "vertical"))
      show(scroller, "vertical")
    if (before.left !== now.left && userScrollable(scroller, "horizontal"))
      show(scroller, "horizontal")
    // Any scroll can move other boxes, and their thumbs with them.
    schedule()
  }

  const grabbed = (event: PointerEvent) =>
    thumbs.find((thumb) => {
      if (thumb.node.dataset.visible === undefined || thumb.node.hidden)
        return false
      const rect = thumb.node.getBoundingClientRect()
      return (
        event.clientX >= rect.left - grabSlop &&
        event.clientX <= rect.right + grabSlop &&
        event.clientY >= rect.top - grabSlop &&
        event.clientY <= rect.bottom + grabSlop
      )
    })

  const onPointerDown = (event: PointerEvent) => {
    swallowClick = false
    if (event.button !== 0) return
    const thumb = grabbed(event)
    if (!thumb) return
    // Nothing beneath may treat this press as its own: no focus, selection, link or dismissal.
    event.preventDefault()
    event.stopImmediatePropagation()
    if (event.target instanceof Element)
      event.target.setPointerCapture(event.pointerId)
    drag = {
      thumb,
      pointer: event.pointerId,
      from: thumb.axis === "vertical" ? event.clientY : event.clientX,
      scroll: scrollPosition(thumb.scroller, thumb.axis),
    }
    thumb.node.dataset.dragging = ""
    root.dataset.scrollbarDragging = ""
  }

  const onPointerMove = (event: PointerEvent) => {
    if (drag) {
      if (drag.pointer !== event.pointerId) return
      const { thumb } = drag
      const geometry = place(thumb)
      if (!geometry) return
      const delta =
        (thumb.axis === "vertical" ? event.clientY : event.clientX) - drag.from
      const target = drag.scroll + delta * geometry.ratio
      if (thumb.axis === "vertical") thumb.scroller.scrollTop = target
      else thumb.scroller.scrollLeft = target
      return
    }
    // Pointing at a shown thumb thickens it and keeps it from fading.
    const hovered = grabbed(event)
    for (const thumb of thumbs) {
      const over = thumb === hovered
      if (over === (thumb.node.dataset.hover !== undefined)) continue
      if (over) thumb.node.dataset.hover = ""
      else {
        delete thumb.node.dataset.hover
        fadeLater(thumb)
      }
    }
  }

  const onPointerUp = (event: PointerEvent) => {
    if (drag?.pointer !== event.pointerId) return
    event.stopImmediatePropagation()
    const { thumb } = drag
    drag = null
    delete thumb.node.dataset.dragging
    delete root.dataset.scrollbarDragging
    // The release still produces a click on whatever lies under the thumb.
    swallowClick = event.type === "pointerup"
    fadeLater(thumb)
  }

  const onClick = (event: MouseEvent) => {
    if (!swallowClick) return
    swallowClick = false
    event.preventDefault()
    event.stopImmediatePropagation()
  }

  const capture = { capture: true, passive: true } as const
  document.addEventListener("scroll", onScroll, capture)
  window.addEventListener("resize", schedule)
  window.addEventListener("pointerdown", onPointerDown, true)
  window.addEventListener("pointermove", onPointerMove, capture)
  window.addEventListener("pointerup", onPointerUp, true)
  window.addEventListener("pointercancel", onPointerUp, true)
  window.addEventListener("click", onClick, true)
  return () => {
    cancelAnimationFrame(frame)
    document.removeEventListener("scroll", onScroll, capture)
    window.removeEventListener("resize", schedule)
    window.removeEventListener("pointerdown", onPointerDown, true)
    window.removeEventListener("pointermove", onPointerMove, capture)
    window.removeEventListener("pointerup", onPointerUp, true)
    window.removeEventListener("pointercancel", onPointerUp, true)
    window.removeEventListener("click", onClick, true)
    for (const thumb of thumbs) {
      window.clearTimeout(thumb.idle)
      thumb.node.remove()
    }
    delete root.dataset.scrollbar
    delete root.dataset.scrollbarDragging
  }
}
