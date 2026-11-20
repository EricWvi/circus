import { createContext, type RefObject } from "react"

/**
 * The panel a folder picker centers on horizontally, such as the dialog the
 * path field is in. Without one, the picker lines up with its field.
 */
export const PickerFrameContext = createContext<RefObject<HTMLElement | null> | null>(null)

/** Where a framed picker anchors: the frame's columns, the field's rows. */
export function framedAnchorRect(frame: DOMRectReadOnly, field: DOMRectReadOnly): DOMRect {
  return new DOMRect(frame.x, field.y, frame.width, field.height)
}

/**
 * A virtual anchor for Radix's Popper: as wide as the frame (so `align`
 * "center" centers on it), as high as the field (so the picker still opens
 * above or below the field). Falls back to the field alone when the frame
 * is gone.
 */
export function framedAnchor(
  frame: RefObject<HTMLElement | null>,
  field: HTMLElement,
): { getBoundingClientRect: () => DOMRect; contextElement: Element } {
  return {
    getBoundingClientRect() {
      const fieldRect = field.getBoundingClientRect()
      const frameRect = frame.current?.getBoundingClientRect()
      return frameRect ? framedAnchorRect(frameRect, fieldRect) : fieldRect
    },
    // Scrolling the field's containers moves the anchor too.
    contextElement: field,
  }
}
