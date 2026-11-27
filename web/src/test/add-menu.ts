import { fireEvent, screen } from "@testing-library/react"

import { advance } from "./render"

/** The devices page header's Add menu trigger ("Add" in English, "添加" in Chinese). */
export const addMenuTrigger = (name: "Add" | "添加" = "Add") => screen.getByRole("button", { name })

/** Chooses `item` from the devices page header's Add menu, with the pointer. */
export async function chooseAdd(item: string, trigger: "Add" | "添加" = "Add") {
  fireEvent.pointerDown(addMenuTrigger(trigger), { button: 0, ctrlKey: false })
  await advance(0)
  fireEvent.click(screen.getByRole("menuitem", { name: item }))
  await advance(50)
}
