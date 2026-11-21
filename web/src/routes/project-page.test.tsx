// @vitest-environment happy-dom
import { cleanup, fireEvent, screen } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { seedState, type MockState } from "@/api/mock/state"
import type { CircusClient } from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { advance, renderWithBackend } from "@/test/render"

import { ProjectPage } from "./project-page"
import { SessionPage } from "./session-page"

async function renderProject(
  projectId: string,
  state: MockState,
  wrapClient?: (client: CircusClient) => CircusClient,
) {
  const router = createMemoryRouter(
    [
      {
        path: "/p/:projectId",
        element: (
          <SidebarProvider>
            <ProjectPage />
          </SidebarProvider>
        ),
      },
      { path: "/p/:projectId/settings", element: <h1>Settings page</h1> },
      {
        path: "/p/:projectId/s/:sessionId",
        element: (
          <SidebarProvider>
            <SessionPage />
          </SidebarProvider>
        ),
      },
      { path: "*", element: null },
    ],
    { initialEntries: [`/p/${projectId}`] },
  )
  const result = await renderWithBackend(<RouterProvider router={router} />, { state, wrapClient })
  await advance(50)
  return { ...result, router }
}

/** The demo data with the circus project changed by `edit`. */
function withCircus(edit: (project: MockState["projects"][number]) => void): MockState {
  const state = seedState()
  edit(state.projects.find((p) => p.projectId === "circus")!)
  return state
}

const composer = () => screen.getByRole<HTMLTextAreaElement>("textbox")

/** A stored location bound to the seed device of that name (or to none). */
const at = (device: string, path: string) => ({ device, path, deviceId: `device-${device}` })

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  await i18n.changeLanguage("en")
})

describe("a project without a usable location", () => {
  it("can't start a session and links to the settings to add one", async () => {
    const { router } = await renderProject(
      "circus",
      withCircus((p) => {
        p.locations = []
        p.repoUrl = "git@github.com:owner/circus.git"
      }),
    )
    expect(composer().disabled).toBe(true)
    expect(composer().placeholder).toBe("Add a location in the project settings to start a session")
    expect(screen.getByText(/has no locations yet/)).toBeTruthy()
    // A repository-only project shows where its code comes from; not a link.
    const repo = screen.getByText("git@github.com:owner/circus.git")
    expect(repo.closest("a")).toBeNull()

    fireEvent.click(screen.getByRole("link", { name: "Add one in settings" }))
    await advance(0)
    expect(router.state.location.pathname).toBe("/p/circus/settings")
  })

  it("says so when every location's device was removed", async () => {
    await renderProject(
      "circus",
      withCircus((p) => {
        p.locations = [at("gone-1", "/a"), at("gone-2", "")]
        p.repoUrl = "https://github.com/owner/circus.git"
      }),
    )
    expect(composer().disabled).toBe(true)
    expect(screen.getByText(/locations were removed/)).toBeTruthy()
    expect(screen.getByRole("link", { name: "Add a location in settings" })).toBeTruthy()
    // An http(s) repository URL is a link.
    expect(screen.getByRole("link", { name: "https://github.com/owner/circus.git" }).getAttribute("href")).toBe(
      "https://github.com/owner/circus.git",
    )
  })
})

describe("locations", () => {
  it("starts in a usable location, marks the removed one and shows the default directory", async () => {
    const state = withCircus((p) => {
      p.locations = [at("gone-box", "/srv/circus"), at("devbox", "")]
      p.lastUsedLocation = 0
    })
    await renderProject("circus", state)
    expect(composer().disabled).toBe(false)
    const picker = screen.getByRole("combobox", { name: "Location" })
    expect(picker.textContent).toContain("devbox:~/circus/projects/circus")
    expect(picker.textContent).toContain("default")

    const before = new Set(state.sessions.map((s) => s.sessionId))
    fireEvent.change(composer(), { target: { value: "Hello" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(50)
    const created = state.sessions.filter((s) => !before.has(s.sessionId))
    expect(created).toMatchObject([{ projectId: "circus", device: "devbox", cwd: "~/circus/projects/circus" }])
    expect(state.projects.find((p) => p.projectId === "circus")!.lastUsedLocation).toBe(1)
  })
})

describe("the first message", () => {
  it("opens the new session, and a message that failed to send waits there with a retry", async () => {
    const state = seedState()
    const before = new Set(state.sessions.map((s) => s.sessionId))
    const sends: unknown[] = []
    let fail = true
    const { router } = await renderProject("circus", state, (client) => ({
      ...client,
      sessions: {
        ...client.sessions,
        sendMessage: (req) => {
          sends.push(req)
          return fail ? Promise.reject(new ApiError("unavailable", "down")) : client.sessions.sendMessage(req)
        },
      },
    }))
    fireEvent.change(composer(), { target: { value: "Fix the flaky test" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(100)
    const created = state.sessions.filter((s) => !before.has(s.sessionId))
    expect(created).toHaveLength(1)
    expect(router.state.location.pathname).toBe(`/p/circus/s/${created[0].sessionId}`)
    expect(screen.getByText("Fix the flaky test")).toBeTruthy()
    expect(screen.getByText("Not sent: can't reach the server.")).toBeTruthy()

    fail = false
    fireEvent.click(screen.getByRole("button", { name: "Retry" }))
    await advance(100)
    expect(sends).toHaveLength(2)
    expect(sends[1]).toEqual(sends[0])
    expect(state.sessions.filter((s) => !before.has(s.sessionId))).toHaveLength(1)
    expect(screen.queryByText(/Not sent/)).toBeNull()
  })
})

describe("a project deleted elsewhere", () => {
  it("leaves the page when starting a session finds it gone", async () => {
    const state = seedState()
    for (const session of state.sessions) if (session.projectId === "toolbox") session.state = "stopped"
    const { backend, router } = await renderProject("toolbox", state)
    const gone = backend.client.projects.deleteProject({ projectId: "toolbox" })
    await advance(0)
    await gone
    fireEvent.change(composer(), { target: { value: "Hello" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(100)
    expect(router.state.location.pathname).toBe("/")
  })
})
