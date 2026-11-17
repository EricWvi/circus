// Remembers whether the user last used the keyboard or a pointer (and
// which kind), so overlays can decide whether returning focus should show a
// focus ring, and actions whether moving focus to a text field would pop up
// a touch keyboard.

let keyboard = false
let pointerType = "mouse"

if (typeof document !== "undefined") {
  document.addEventListener(
    "keydown",
    (event) => {
      if (!event.metaKey && !event.ctrlKey && !event.altKey) keyboard = true
    },
    true,
  )
  document.addEventListener(
    "pointerdown",
    (event) => {
      keyboard = false
      pointerType = event.pointerType
    },
    true,
  )
}

export const lastInputWasKeyboard = () => keyboard

/** The last input was a finger (or pen) on a touch screen. */
export const lastInputWasTouch = () => !keyboard && (pointerType === "touch" || pointerType === "pen")
