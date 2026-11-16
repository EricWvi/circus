// Remembers whether the user last used the keyboard or a pointer, so
// overlays can decide whether returning focus should show a focus ring.

let keyboard = false

if (typeof document !== "undefined") {
  document.addEventListener(
    "keydown",
    (event) => {
      if (!event.metaKey && !event.ctrlKey && !event.altKey) keyboard = true
    },
    true,
  )
  document.addEventListener("pointerdown", () => (keyboard = false), true)
}

export const lastInputWasKeyboard = () => keyboard
