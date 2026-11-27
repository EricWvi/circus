// @vitest-environment happy-dom
import { cleanup, screen } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { seedState } from "@/api/mock/state"
import { AppSidebar } from "@/components/app-sidebar"
import { NewProjectContext } from "@/components/projects/new-project-context"
import { ThemeProvider } from "@/components/theme/theme-provider"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { advance, renderWithBackend } from "@/test/render"

async function renderSidebar() {
  const router = createMemoryRouter(
    [
      {
        path: "/devices",
        element: (
          <ThemeProvider>
            <NewProjectContext.Provider value={() => {}}>
              <SidebarProvider>
                <AppSidebar devicesActive />
              </SidebarProvider>
            </NewProjectContext.Provider>
          </ThemeProvider>
        ),
      },
    ],
    { initialEntries: ["/devices"] },
  )
  const result = await renderWithBackend(<RouterProvider router={router} />, { state: seedState() })
  await advance(50)
  return result
}

/** The sidebar footer's links (the user menu is a button, not a link). */
const footerLinks = () =>
  Array.from(document.querySelectorAll<HTMLAnchorElement>("[data-sidebar=footer] a"), (a) => ({
    href: a.getAttribute("href"),
    current: a.getAttribute("aria-current"),
  }))

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-22T12:00:00Z"))
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  await i18n.changeLanguage("en")
})

describe("the sidebar footer", () => {
  it("links to Devices, which holds the providers, and to nothing else", async () => {
    await renderSidebar()
    expect(footerLinks()).toEqual([{ href: "/devices", current: "page" }])
    expect(screen.getByRole("link", { name: /^Devices/ })).toBeTruthy()
    expect(screen.queryByRole("link", { name: /Providers/ })).toBeNull()
    expect(document.querySelector('a[href^="/providers"]')).toBeNull()
  })

  it("has no providers entry in Chinese either", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderSidebar()
    expect(footerLinks()).toEqual([{ href: "/devices", current: "page" }])
    expect(screen.queryByText("模型提供方")).toBeNull()
  })
})
