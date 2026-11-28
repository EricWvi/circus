import { screen, within } from "@testing-library/react"

import { i18n } from "@/i18n/i18n"

/** The devices page's Machines section, in any language. */
export const machinesSection = () => document.querySelector<HTMLElement>('section[aria-labelledby="machines-heading"]')!

/** The cards of the Machines section, in order. */
export const machineCards = () => [...machinesSection().querySelectorAll<HTMLElement>(":scope > ul > li")]

/** The machines' names, in order. */
export const machineNames = () =>
  machineCards().map((c) => document.getElementById(c.getAttribute("aria-labelledby")!)!.textContent)

/** The card of the machine with that name. */
export const machineCard = (name: string) => within(machinesSection()).getByRole("listitem", { name })

/** The row of the device with that name, in the current language. */
export const deviceRow = (name: string) => screen.getByRole("listitem", { name: i18n.t("devices.roleRow.device", { name }) })

/** The row of the worker with that name, in the current language. */
export const workerRow = (name: string) => screen.getByRole("listitem", { name: i18n.t("devices.roleRow.worker", { name }) })

/** The rows of a card, in order: their accessible names ("Device devbox", "Worker devbox" in English). */
export const rolesOf = (card: HTMLElement) =>
  [...card.querySelectorAll<HTMLElement>("[data-role-row]")].map((r) => r.getAttribute("aria-label"))

/** The name each row of a card shows, in order; null where it is left out (the machine's own name). */
export const shownNames = (card: HTMLElement) =>
  [...card.querySelectorAll<HTMLElement>("[data-role-row]")].map((r) => r.querySelector("[data-role-name]")?.textContent ?? null)

/** The role label each row of a card shows, in order. */
export const roleLabels = (card: HTMLElement) =>
  [...card.querySelectorAll<HTMLElement>("[data-role-row] [data-role-label]")].map((l) => l.textContent)
