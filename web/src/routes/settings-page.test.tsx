// @vitest-environment happy-dom
import { act, cleanup, fireEvent, render, screen, waitFor, within } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ThemeProvider } from "@/components/theme/theme-provider"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { CODE_FONT_STORAGE_KEY, resetCodeFont, setCodeFont, startCodeFont } from "@/settings/code-font"

import { SettingsPage } from "./settings-page"

// The preview's highlighter can't load in happy-dom; plain text is enough.
vi.mock("@/components/ai-elements/code-block", () => ({
  CodeBlock: ({ code }: { code: string }) => <pre>{code}</pre>,
}))

/**
 * happy-dom has no canvas, so measuring text goes through a fake one:
 * monospace fonts give every glyph the same width, proportional ones don't,
 * and a family that isn't installed falls back to the generic after it.
 */
const INSTALLED: Record<string, "mono" | "proportional"> = {
  Hack: "mono",
  "Fira Code": "mono",
  Inter: "proportional",
}
const GENERIC: Record<string, "mono" | "proportional"> = {
  monospace: "mono",
  serif: "proportional",
  "sans-serif": "proportional",
}
function measure(text: string, font: string) {
  const names = font.replace(/^\d+px /, "").split(",").map((n) => n.trim().replace(/^"|"$/g, ""))
  const name = names.find((n) => n in INSTALLED || n in GENERIC) ?? "serif"
  const kind = INSTALLED[name] ?? GENERIC[name]
  const scale = name in INSTALLED ? 1.1 : name === "serif" ? 0.9 : 1
  return [...text].reduce((sum, c) => sum + (kind === "mono" ? 10 : "il.1".includes(c) ? 4 : 12) * scale, 0)
}

type Query = () => Promise<{ family: string }[]>
const setQuery = (query: Query | undefined) => {
  const w = window as unknown as { queryLocalFonts?: Query }
  if (query) w.queryLocalFonts = query
  else delete w.queryLocalFonts
}
const fontsResolve = () => Promise.resolve(Object.keys(INSTALLED).map((family) => ({ family })))

let stop = () => {}
beforeEach(() => {
  localStorage.clear()
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockImplementation(function () {
    const context = {
      font: "",
      measureText: (text: string) => ({ width: measure(text, context.font) }),
    }
    return context as unknown as CanvasRenderingContext2D
  } as unknown as typeof HTMLCanvasElement.prototype.getContext)
  stop = startCodeFont()
})

afterEach(async () => {
  cleanup()
  resetCodeFont()
  stop()
  setQuery(undefined)
  vi.restoreAllMocks()
  await i18n.changeLanguage("en")
})

const renderPage = () =>
  render(
    <ThemeProvider>
      <SidebarProvider>
        <SettingsPage />
      </SidebarProvider>
    </ThemeProvider>,
  )

const filter = () => screen.getByRole("combobox", { name: "Code font" })
const stored = () => JSON.parse(localStorage.getItem(CODE_FONT_STORAGE_KEY) ?? "null")
const option = (name: string | RegExp) => screen.getByRole("option", { name })
const checked = (el: HTMLElement) => el.getAttribute("data-checked") === "true"
const highlighted = () =>
  screen.getAllByRole("option").find((o) => o.getAttribute("data-selected") === "true")
const typeFilter = (text: string) => fireEvent.change(filter(), { target: { value: text } })
const enter = () => fireEvent.keyDown(filter(), { key: "Enter" })

/**
 * Presses the button that asks for the fonts, from the keyboard (Enter on
 * the focused button) or with a pointer; input-modality.ts tells them apart.
 */
async function showFonts(how: "keyboard" | "pointer" = "keyboard") {
  const button = screen.getByRole("button", { name: /Show installed fonts|Try again/ })
  button.focus()
  if (how === "keyboard") fireEvent.keyDown(button, { key: "Enter" })
  else fireEvent.pointerDown(button)
  await act(async () => {
    fireEvent.click(button)
  })
  return button
}

describe("SettingsPage", () => {
  it("shows the appearance settings with the defaults", () => {
    renderPage()
    expect(screen.getByRole("heading", { level: 1, name: "Settings" })).toBeTruthy()
    expect(screen.getByRole("heading", { level: 2, name: "Appearance" })).toBeTruthy()
    expect(checked(option("Default (platform)"))).toBe(true)
    const size = screen.getByLabelText("Code font size") as HTMLInputElement
    expect(size.value).toBe("14")
    const hint = document.getElementById(size.getAttribute("aria-describedby")!)
    expect(hint?.textContent).toBe("10–24 px")
  })

  it("steps the size within the range and marks the limits", () => {
    renderPage()
    const size = screen.getByLabelText("Code font size") as HTMLInputElement
    const larger = screen.getByRole("button", { name: "Larger" })
    const smaller = screen.getByRole("button", { name: "Smaller" })
    fireEvent.click(larger)
    expect(size.value).toBe("15")
    expect(stored()).toEqual({ family: null, size: 15 })

    fireEvent.change(size, { target: { value: "24" } })
    expect(larger.getAttribute("aria-disabled")).toBe("true")
    fireEvent.click(larger)
    expect(stored().size).toBe(24)

    fireEvent.change(size, { target: { value: "30" } })
    expect(size.getAttribute("aria-invalid")).toBe("true")
    expect(stored().size).toBe(24)
    fireEvent.blur(size)
    expect(size.value).toBe("24")
    expect(size.hasAttribute("aria-invalid")).toBe(false)

    fireEvent.change(size, { target: { value: "10" } })
    expect(smaller.getAttribute("aria-disabled")).toBe("true")
    expect(larger.hasAttribute("aria-disabled")).toBe(false)
    fireEvent.click(smaller)
    expect(stored().size).toBe(10)
  })

  it("takes a typed name without the font API, with a hint", () => {
    setQuery(undefined)
    renderPage()
    expect(screen.getByText(/can't list installed fonts/)).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Show installed fonts" })).toBeNull()

    typeFilter("No Such Font")
    expect(option(/Use “No Such Font”/).textContent).toContain("not found on this device")
    typeFilter("monospace")
    const generic = option(/Use “monospace”/)
    expect(generic.textContent).toContain("not a font name")
    expect(generic.getAttribute("aria-disabled")).toBe("true")

    typeFilter("Hack")
    expect(option(/Use “Hack”/).textContent).toContain("installed")
    enter()
    expect(stored()).toEqual({ family: "Hack", size: 14 })
    expect(checked(option(/Use “Hack”/))).toBe(true)
    expect(highlighted()?.textContent).toContain("Use “Hack”")

    // Without a filter, the choice is its own row.
    typeFilter("")
    expect(checked(option(/Hack/))).toBe(true)
  })

  it("lists installed fonts, monospace first, and filters them", async () => {
    setQuery(vi.fn(fontsResolve))
    renderPage()
    await showFonts()
    await waitFor(() => expect(screen.getByRole("group", { name: "Monospace" })).toBeTruthy())
    expect(document.activeElement).toBe(filter())
    const mono = within(screen.getByRole("group", { name: "Monospace" })).getAllByRole("option")
    expect(mono.map((o) => o.textContent)).toEqual(["Fira Code", "Hack"])
    expect(within(screen.getByRole("group", { name: "Other fonts" })).getByRole("option").textContent).toBe(
      "Inter",
    )

    // A prefix of a listed family offers that family, not the typed name.
    typeFilter("fi")
    expect(screen.queryByRole("option", { name: /Use “/ })).toBeNull()
    expect(highlighted()?.textContent).toBe("Fira Code")
    enter()
    expect(stored()).toEqual({ family: "Fira Code", size: 14 })
    expect(checked(option("Fira Code"))).toBe(true)
  })

  it("moves the highlight to the default on reset", async () => {
    setQuery(vi.fn(fontsResolve))
    renderPage()
    await showFonts()
    await waitFor(() => expect(option("Hack")).toBeTruthy())
    fireEvent.click(option("Hack"))
    expect(stored()?.family).toBe("Hack")
    fireEvent.click(screen.getByRole("button", { name: "Reset code font" }))
    expect(stored()).toBeNull()
    expect(checked(option("Default (platform)"))).toBe(true)
    expect(highlighted()?.textContent).toBe("Default (platform)")
  })

  it("leaves focus on the button after a click, so no ring appears in the filter", async () => {
    const query = vi.fn<Query>(() => Promise.resolve([]))
    setQuery(query)
    renderPage()
    const button = await showFonts("pointer")
    await waitFor(() => expect(screen.getByText(/was not allowed/)).toBeTruthy())
    // Still the same, mounted button, now offering to try again.
    expect(screen.getByRole("button", { name: "Try again" })).toBe(button)
    expect(document.activeElement).toBe(button)

    query.mockImplementation(fontsResolve)
    await showFonts("pointer")
    await waitFor(() => expect(option("Hack")).toBeTruthy())
    expect(document.activeElement).not.toBe(filter())
  })

  it("doesn't let Enter on a generic family name fall through to Default", () => {
    setCodeFont({ family: "Hack", size: 14 })
    renderPage()
    typeFilter("monospace")
    expect(option(/Use “monospace”/).getAttribute("aria-disabled")).toBe("true")
    enter()
    expect(stored()).toEqual({ family: "Hack", size: 14 })
  })

  it("offers to try again when access is denied, with focus in the filter", async () => {
    const query = vi.fn<Query>(() => Promise.reject(new DOMException("no", "NotAllowedError")))
    setQuery(query)
    renderPage()
    await showFonts()
    await waitFor(() => expect(screen.getByText(/was not allowed/)).toBeTruthy())
    expect(document.activeElement).toBe(filter())

    // A dismissed prompt: no fonts at all.
    query.mockImplementation(() => Promise.resolve([]))
    await showFonts()
    await waitFor(() => expect(query).toHaveBeenCalledTimes(2))
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy()

    query.mockImplementation(fontsResolve)
    await showFonts()
    await waitFor(() => expect(option("Hack")).toBeTruthy())
    expect(screen.queryByText(/was not allowed/)).toBeNull()
    expect(document.activeElement).toBe(filter())
  })

  it("shows a stored font that isn't listed as the current one", () => {
    setCodeFont({ family: `Bob's "Mono"`, size: 18 })
    renderPage()
    const row = option(/Bob's "Mono"/)
    expect(checked(row)).toBe(true)
    expect(row.textContent).toContain("Current")
    expect((screen.getByLabelText("Code font size") as HTMLInputElement).value).toBe("18")
  })

  it("is translated", async () => {
    await i18n.changeLanguage("zh-CN")
    renderPage()
    for (const label of ["外观", "代码字体", "代码字号", "默认（随平台）"]) {
      expect(screen.getAllByText(label).length).toBeGreaterThan(0)
    }
  })
})
