import { useRef, type KeyboardEvent } from "react"

/**
 * Radix menus return focus to their trigger on close, which leaves a focus
 * ring behind after a mouse or touch interaction. Skip the return when the
 * menu was opened and closed with a pointer; whenever the keyboard opened or
 * closed it (Escape, Enter or Space on an item), focus goes back as usual.
 *
 * Spread `trigger` on the menu trigger and `content` on the menu content.
 */
export function useMenuFocus() {
  const pointerOnly = useRef(false)
  const usedKeyboard = () => {
    pointerOnly.current = false
  }
  return {
    trigger: {
      onPointerDown: () => {
        pointerOnly.current = true
      },
      onKeyDown: usedKeyboard,
    },
    content: {
      onEscapeKeyDown: usedKeyboard,
      onKeyDown: (event: KeyboardEvent) => {
        if (event.key === "Enter" || event.key === " ") usedKeyboard()
      },
      onCloseAutoFocus: (event: Event) => {
        if (pointerOnly.current) event.preventDefault()
      },
    },
  }
}
