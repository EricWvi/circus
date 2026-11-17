import { MonitorIcon, MoonIcon, SunIcon } from "lucide-react"

import type { Theme } from "./theme-context"

/** The themes to choose from, in menu order, with their icons. */
export const themeChoices: { value: Theme; icon: typeof SunIcon }[] = [
  { value: "light", icon: SunIcon },
  { value: "dark", icon: MoonIcon },
  { value: "system", icon: MonitorIcon },
]

export const isTheme = (value: string): value is Theme =>
  themeChoices.some((t) => t.value === value)
