import { useRef } from "react"

import { lastInputWasKeyboard } from "@/lib/input-modality"

/**
 * Focus handling for a Radix dialog or select. When the overlay was opened
 * or closed with the keyboard, focus returns to whatever had it when the
 * overlay opened (Radix dialogs opened without a DialogTrigger would drop
 * it). After pointer-only use, returning focus would leave a ring behind,
 * so focus stays put instead; pass `triggerRef` (a select's trigger) to keep
 * focus there for the Tab order without the ring.
 */
export function useOverlayFocus<T extends HTMLElement = HTMLElement>() {
  const openedByKeyboard = useRef(false)
  const returnTo = useRef<HTMLElement | null>(null)
  const triggerRef = useRef<T>(null)
  return {
    triggerRef,
    onOpenChange: (open: boolean) => {
      if (!open) return
      openedByKeyboard.current = lastInputWasKeyboard()
      returnTo.current = document.activeElement instanceof HTMLElement ? document.activeElement : null
    },
    onCloseAutoFocus: (event: Event) => {
      event.preventDefault()
      if (openedByKeyboard.current || lastInputWasKeyboard()) {
        const target = returnTo.current?.isConnected ? returnTo.current : triggerRef.current
        target?.focus({ preventScroll: true })
        return
      }
      triggerRef.current?.focus({ preventScroll: true, focusVisible: false } as FocusOptions)
    },
  }
}
