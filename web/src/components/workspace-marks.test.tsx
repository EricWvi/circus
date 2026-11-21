// @vitest-environment happy-dom
import { cleanup, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { MockState } from "@/api/mock/state"
import { AppSidebar } from "@/components/app-sidebar"
import { NewProjectContext } from "@/components/projects/new-project-context"
import { ThemeProvider } from "@/components/theme/theme-provider"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { ProjectPage } from "@/routes/project-page"
import { advance, renderWithBackend } from "@/test/render"
import { withDefaultProject } from "@/test/workspace-state"

/**
 * "fresh" with sessions in its default folders on devbox (preparing),
 * eric-mbp (failed) and palace-builder (ready), and a stopped one whose
 * folder failed too.
 */
async function renderWithFolders(ui: React.ReactNode) {
  const state: MockState = withDefaultProject("https://example.com/slow.git", ["devbox", "eric-mbp", "palace-builder"])
  const router = createMemoryRouter([{ path: "/p/:projectId", element: ui }], { initialEntries: ["/p/fresh"] })
  const result = await renderWithBackend(<RouterProvider router={router} />, { state })
  const { client } = result.backend
  const start = async (locationIndex: number, title: string) => {
    const created = client.sessions.createSession({ projectId: "fresh", locationIndex, clientRequestId: crypto.randomUUID() })
    await advance(10)
    const session = await created
    state.sessions.find((s) => s.sessionId === session.sessionId)!.title = title
    return state.sessions.find((s) => s.sessionId === session.sessionId)!
  }
  await start(0, "Preparing one")
  const failed = await start(1, "Failed one")
  const ready = await start(2, "Ready one")
  const stopped = await start(1, "Stopped one")
  stopped.state = "stopped"
  await advance(1_000)
  Object.assign(failed.workspace!, { state: "failed", error: "not_empty" })
  Object.assign(ready.workspace!, { state: "ready", action: "cloned", path: "/home/eric/circus/projects/fresh" })
  // The lists refetch every five seconds.
  await advance(5_000)
  return result
}

const row = (title: string) => screen.getByText(title).closest("a")!

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  await i18n.changeLanguage("en")
})

describe("sessions whose folder isn't ready", () => {
  it("are marked in the project's session list", async () => {
    await renderWithFolders(
      <SidebarProvider>
        <ProjectPage />
      </SidebarProvider>,
    )
    expect(within(row("Preparing one")).getByRole("img", { name: "Preparing its folder" })).toBeTruthy()
    const failed = within(row("Failed one")).getByRole("img", { name: "Couldn't prepare its folder" })
    expect(failed.className).toContain("text-destructive")
    expect(within(row("Ready one")).queryByRole("img", { name: /folder/ })).toBeNull()
    expect(within(row("Stopped one")).queryByRole("img", { name: /folder/ })).toBeNull()
  })

  it("are marked in the sidebar", async () => {
    await renderWithFolders(
      <ThemeProvider>
        <NewProjectContext.Provider value={() => {}}>
          <SidebarProvider>
            <AppSidebar />
          </SidebarProvider>
        </NewProjectContext.Provider>
      </ThemeProvider>,
    )
    expect(within(row("Preparing one")).getByRole("img", { name: "Preparing its folder" })).toBeTruthy()
    expect(within(row("Failed one")).getByRole("img", { name: "Couldn't prepare its folder" })).toBeTruthy()
    expect(within(row("Ready one")).queryByRole("img", { name: /folder/ })).toBeNull()
  })

  it("are marked in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderWithFolders(
      <SidebarProvider>
        <ProjectPage />
      </SidebarProvider>,
    )
    expect(within(row("Preparing one")).getByRole("img", { name: "正在准备工作目录" })).toBeTruthy()
    expect(within(row("Failed one")).getByRole("img", { name: "工作目录准备失败" })).toBeTruthy()
  })
})
