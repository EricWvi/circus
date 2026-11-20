// Follows one press of the primary pointer button, for code that must know
// a press is under way before its click: a press blurs a focused field
// before the click lands, and in Safari and Firefox on macOS a clicked
// button doesn't take focus, so the blur names no related target.

/** Longest a press counts, whatever happens. */
export const MAX_PRESS_MS = 2_000

/** Whether a pointerdown is a press of the primary button (any pointer type). */
export const isPrimaryPress = (event: { button: number }) => event.button === 0

/**
 * Calls `onEnd` once the press that just started is over: a tick after its
 * release (so its click has run), at once when the window loses focus or a
 * context menu opens, and after `MAX_PRESS_MS` at the latest.
 */
export function watchPress(onEnd: () => void): void {
  let done = false
  let limit: ReturnType<typeof setTimeout> | undefined
  const end = () => {
    if (done) return
    done = true
    document.removeEventListener("pointerup", released, true)
    document.removeEventListener("pointercancel", released, true)
    document.removeEventListener("contextmenu", end, true)
    window.removeEventListener("blur", end)
    clearTimeout(limit)
    onEnd()
  }
  const released = () => setTimeout(end, 0)
  document.addEventListener("pointerup", released, true)
  document.addEventListener("pointercancel", released, true)
  document.addEventListener("contextmenu", end, true)
  window.addEventListener("blur", end)
  limit = setTimeout(end, MAX_PRESS_MS)
}
