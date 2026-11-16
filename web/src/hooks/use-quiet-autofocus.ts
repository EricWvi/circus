import { useState } from "react"

/**
 * Autofocus for a page's first input without the keyboard focus ring: focus
 * placed on load is not the user's doing. The caret still shows; once focus
 * leaves the input it gets the normal ring again.
 *
 * Spread the result on the input; it relies on the `data-quiet-focus`
 * styles in index.css.
 */
export function useQuietAutoFocus() {
  const [quiet, setQuiet] = useState(true)
  return {
    autoFocus: true,
    "data-quiet-focus": quiet ? "" : undefined,
    onBlur: () => setQuiet(false),
  }
}
