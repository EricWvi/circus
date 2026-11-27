import { useMemo, useRef } from "react"

import { lastInputWasKeyboard } from "@/lib/input-modality"

/**
 * Focus for dialogs opened by a button: when one closes, focus returns to
 * the button that opened it, with a focus ring only when it was opened or
 * closed with the keyboard. Once that button is gone, focus goes to what
 * `restore`'s fallback names. The result is stable.
 */
export function useReturnFocus() {
  const state = useRef({ returnTo: null as HTMLElement | null, keyboard: false, handedOff: false })
  return useMemo(
    () => ({
      /** A dialog opens from `trigger`. */
      opened: (trigger: HTMLElement | null) => {
        state.current = { returnTo: trigger, keyboard: lastInputWasKeyboard(), handedOff: false }
      },
      /** The opener is gone for good (its row was deleted): use the fallback. */
      forget: () => {
        state.current.returnTo = null
      },
      /**
       * The dialog closes for another that opens in its place, which takes
       * over the opener: this one's close leaves focus alone.
       */
      handOff: () => {
        const { returnTo } = state.current
        state.current.handedOff = true
        return returnTo
      },
      /** For the dialog's onCloseAutoFocus. */
      restore: (event: Event, fallback?: () => HTMLElement | null) => {
        event.preventDefault()
        const { returnTo, keyboard, handedOff } = state.current
        if (handedOff) return
        const target = returnTo?.isConnected ? returnTo : (fallback?.() ?? null)
        target?.focus({ preventScroll: true, focusVisible: keyboard || lastInputWasKeyboard() } as FocusOptions)
      },
    }),
    [],
  )
}
