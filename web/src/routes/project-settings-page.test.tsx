// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { seedState, type MockState } from "@/api/mock/state"
import { queryKeys } from "@/api/query-client"
import type { CircusClient } from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { PROJECTS_WAITING_INTERVAL_MS } from "@/api/hooks"
import { NOTICE_FOLD_MS, NOTICE_VISIBLE_MS } from "@/components/notices"
import { advance, renderWithBackend } from "@/test/render"
import { withDefaultProject } from "@/test/workspace-state"

import { ProjectSettingsPage } from "./project-settings-page"

async function renderSettings(
  projectId: string,
  state: MockState = seedState(),
  wrapClient?: (client: CircusClient) => CircusClient,
) {
  const router = createMemoryRouter(
    [
      {
        path: "/p/:projectId/settings",
        element: (
          <SidebarProvider>
            <ProjectSettingsPage />
          </SidebarProvider>
        ),
      },
      { path: "*", element: null },
    ],
    { initialEntries: [`/p/${projectId}/settings`] },
  )
  const result = await renderWithBackend(<RouterProvider router={router} />, { state, wrapClient })
  await advance(50)
  return { ...result, router }
}

/** The state with every session of a project stopped, so it can be deleted. */
function withStoppedSessions(state: MockState, projectId: string): MockState {
  for (const session of state.sessions) if (session.projectId === projectId) session.state = "stopped"
  return state
}

/** Runs a mock call to completion: its latency is a (fake) timer. */
async function settle<T>(promise: Promise<T>): Promise<T> {
  await advance(0)
  return promise
}

const nameInput = () => screen.getByRole<HTMLInputElement>("textbox", { name: "Name" })
const save = () => screen.getByRole("button", { name: "Save" })

/** A stored location bound to the seed device of that name (or to none). */
const at = (device: string, path: string) => ({ device, path, deviceId: `device-${device}` })

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  vi.restoreAllMocks()
  await i18n.changeLanguage("en")
})

describe("saving a project changed elsewhere", () => {
  it("says so, and reloads after confirming that edits are discarded", async () => {
    const { backend } = await renderSettings("circus")
    expect(nameInput().value).toBe("circus")

    // Another tab renames the project; this page still has the old version.
    vi.setSystemTime(Date.parse("2026-11-17T12:01:00Z"))
    await settle(backend.client.projects.updateProject({ projectId: "circus", name: "circus-ng" }))

    fireEvent.change(nameInput(), { target: { value: "circus-web" } })
    fireEvent.click(save())
    await advance(50)
    const alert = screen.getByRole("alert")
    expect(alert.textContent).toContain("This project was changed elsewhere.")
    // Nothing was overwritten.
    expect((await settle(backend.client.projects.getProject({ projectId: "circus" }))).name).toBe("circus-ng")

    fireEvent.click(within(alert).getByRole("button", { name: "Reload" }))
    await advance(0)
    const dialog = screen.getByRole("dialog", { name: "Discard your changes?" })
    // Cancel keeps the edit.
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }))
    await advance(300)
    expect(nameInput().value).toBe("circus-web")

    fireEvent.click(within(screen.getByRole("alert")).getByRole("button", { name: "Reload" }))
    await advance(0)
    fireEvent.click(screen.getByRole("button", { name: "Discard and reload" }))
    await advance(50)
    expect(nameInput().value).toBe("circus-ng")
    expect(screen.queryByText("This project was changed elsewhere.")).toBeNull()

    // A save from the reloaded form goes through.
    fireEvent.change(nameInput(), { target: { value: "circus-web" } })
    fireEvent.click(save())
    await advance(50)
    expect((await settle(backend.client.projects.getProject({ projectId: "circus" }))).name).toBe("circus-web")
    expect(screen.queryByRole("alert")).toBeNull()
  })

  it("reloads without asking when nothing is left to lose", async () => {
    const { backend } = await renderSettings("circus")
    await settle(backend.client.projects.updateProject({ projectId: "circus", repoUrl: "git@github.com:o/c.git" }))
    fireEvent.change(nameInput(), { target: { value: "circus-web" } })
    fireEvent.click(save())
    await advance(50)
    // Undoing the edit leaves the form as it was loaded.
    fireEvent.change(nameInput(), { target: { value: "circus" } })
    fireEvent.click(within(screen.getByRole("alert")).getByRole("button", { name: "Reload" }))
    await advance(50)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(screen.getByRole<HTMLInputElement>("textbox", { name: /Repository URL/ }).value).toBe(
      "git@github.com:o/c.git",
    )
  })

  it("follows changes made elsewhere while the form is untouched", async () => {
    const { backend, queryClient } = await renderSettings("circus")
    await settle(backend.client.projects.updateProject({ projectId: "circus", name: "renamed" }))
    void queryClient.invalidateQueries()
    await advance(50)
    expect(nameInput().value).toBe("renamed")
  })
})

describe("locations", () => {
  it("shows the default directory and a removed device", async () => {
    const state = seedState()
    const circus = state.projects.find((p) => p.projectId === "circus")!
    circus.locations = [
      at("devbox", ""),
      at("gone-box", "/srv/circus"),
    ]
    await renderSettings("circus", state)
    const path = screen.getByRole<HTMLInputElement>("textbox", { name: "Path for location 1" })
    expect(path.value).toBe("")
    expect(path.placeholder).toBe("~/circus/projects/circus")
    expect(screen.getByText("default")).toBeTruthy()
    expect(screen.getByRole("combobox", { name: "Device for location 2" }).textContent).toContain("removed")

    // Renaming doesn't need the removed device fixed: unchanged locations aren't sent.
    fireEvent.change(nameInput(), { target: { value: "circus2" } })
    fireEvent.click(save())
    await advance(50)
    expect(circus.name).toBe("circus2")
    expect(circus.locations).toHaveLength(2)

    // Changing the locations does. The added row goes to a device without
    // a default location yet.
    fireEvent.click(screen.getByRole("button", { name: "Add location" }))
    expect(screen.getByRole("combobox", { name: "Device for location 3" }).textContent).toContain("eric-mbp")
    fireEvent.click(save())
    await advance(50)
    expect(screen.getByText("This device was removed. Choose another device or remove the location.")).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "Remove location 2" }))
    fireEvent.click(save())
    await advance(50)
    expect(circus.locations).toEqual([at("devbox", ""), at("eric-mbp", "")])
  })
})

describe("a removed device whose name is enrolled again", () => {
  it("is marked as replaced, and saving binds the new device", async () => {
    const state = seedState()
    const circus = state.projects.find((p) => p.projectId === "circus")!
    circus.locations = [at("devbox", "/srv/circus")]
    // devbox was revoked and a new device joined under the same name.
    state.devices.find((d) => d.name === "devbox")!.deviceId = "device-devbox-2"
    const { backend } = await renderSettings("circus", state)
    expect((await settle(backend.client.projects.getProject({ projectId: "circus" }))).locations[0].deviceRevoked).toBe(true)

    const device = screen.getByRole("combobox", { name: "Device for location 1" })
    expect(device.textContent).toContain("devbox")
    expect(device.textContent).toContain("replaced")
    expect(screen.getByText("A new device took this name. Save to use the new device.")).toBeTruthy()
    // Nothing was edited, but there is something to save.
    expect(save().getAttribute("aria-disabled")).toBeNull()
    fireEvent.click(save())
    await advance(50)
    expect(circus.locations).toEqual([{ device: "devbox", path: "/srv/circus", deviceId: "device-devbox-2" }])
    expect((await settle(backend.client.projects.getProject({ projectId: "circus" }))).locations[0].deviceRevoked).toBe(false)
    expect(screen.queryByText("replaced")).toBeNull()
  })
})

describe("a project deleted elsewhere", () => {
  it("leaves the settings when a save finds it gone", async () => {
    const state = seedState()
    const { backend, router } = await renderSettings("toolbox", withStoppedSessions(state, "toolbox"))
    await settle(backend.client.projects.deleteProject({ projectId: "toolbox" }))
    fireEvent.change(nameInput(), { target: { value: "toolbox2" } })
    fireEvent.click(save())
    await advance(100)
    expect(router.state.location.pathname).toBe("/")
  })
})

describe("focus", () => {
  it("stays on Save after saving, moves off a refused Save, and lands on the name after a reload", async () => {
    const { backend } = await renderSettings("circus")
    fireEvent.change(nameInput(), { target: { value: "circus-web" } })
    const button = save()
    button.focus()
    fireEvent.click(button)
    // Saving: read-only, not disabled, so nothing loses focus.
    expect(nameInput().readOnly).toBe(true)
    expect(document.activeElement).toBe(button)
    await advance(50)
    expect(nameInput().readOnly).toBe(false)
    expect(document.activeElement).toBe(save())
    expect(save().getAttribute("aria-disabled")).toBe("true")

    await settle(backend.client.projects.updateProject({ projectId: "circus", repoUrl: "git@h:x" }))
    fireEvent.change(nameInput(), { target: { value: "circus-app" } })
    save().focus()
    fireEvent.click(save())
    await advance(50)
    expect(screen.getByRole("alert").textContent).toContain("This project was changed elsewhere.")
    // Save is disabled now; its wrapper, which explains why, has focus.
    expect(document.activeElement).not.toBe(document.body)

    const reload = within(screen.getByRole("alert")).getByRole("button", { name: "Reload" })
    reload.focus()
    fireEvent.click(reload)
    await advance(0)
    fireEvent.click(screen.getByRole("button", { name: "Discard and reload" }))
    await advance(300)
    expect(screen.queryAllByText("This project was changed elsewhere.")).toEqual([])
    expect(document.activeElement).toBe(nameInput())
  })
})

describe("a form that already holds the new values", () => {
  it("takes the new version without a conflict", async () => {
    const { backend, queryClient } = await renderSettings("circus")
    fireEvent.change(nameInput(), { target: { value: "circus-ng" } })
    // Another tab saves the same name.
    await settle(backend.client.projects.updateProject({ projectId: "circus", name: "circus-ng" }))
    void queryClient.invalidateQueries()
    await advance(50)
    expect(nameInput().value).toBe("circus-ng")
    // Saving more on top goes through: the form is on the new version.
    fireEvent.change(screen.getByRole<HTMLInputElement>("textbox", { name: /Repository URL/ }), {
      target: { value: "git@github.com:o/c.git" },
    })
    fireEvent.click(save())
    await advance(50)
    expect(screen.queryByText("This project was changed elsewhere.")).toBeNull()
    const saved = await settle(backend.client.projects.getProject({ projectId: "circus" }))
    expect(saved).toMatchObject({ name: "circus-ng", repoUrl: "git@github.com:o/c.git" })
  })
})

describe("adding a location", () => {
  it("asks for a path, with focus, once every device has a default location", async () => {
    const state = seedState()
    state.projects.find((p) => p.projectId === "circus")!.locations = state.devices.map((d) => at(d.name, ""))
    await renderSettings("circus", state)
    fireEvent.click(screen.getByRole("button", { name: "Add location" }))
    await advance(0)
    const path = screen.getByRole<HTMLInputElement>("textbox", { name: "Path for location 5" })
    expect(document.activeElement).toBe(path)
    expect(path.placeholder).toBe("/home/…")
    expect(screen.getByRole("combobox", { name: "Device for location 5" }).textContent).toContain("devbox")
  })
})

describe("a device removed while the form is open", () => {
  it("refreshes the devices when the server refuses it", async () => {
    const { state } = await renderSettings("circus")
    expect(screen.getByRole("combobox", { name: "Device for location 2" }).textContent).toContain("devbox")
    // devbox is revoked after the devices were loaded.
    state.devices = state.devices.filter((d) => d.name !== "devbox")
    fireEvent.change(screen.getByRole("textbox", { name: "Path for location 2" }), { target: { value: "/srv/c" } })
    fireEvent.click(save())
    await advance(100)
    const device = screen.getByRole("combobox", { name: "Device for location 2" })
    expect(device.textContent).toContain("removed")
    expect(screen.getAllByText("This device was removed. Choose another device or remove the location.").length).toBeGreaterThan(0)
  })
})

describe("discarding and reloading from the keyboard", () => {
  it("puts focus on the name when the dialog closes after the Reload button is gone", async () => {
    // In a browser the dialog closes with an animation, and the reload can
    // finish before it ends; happy-dom has no animations, so the dialog
    // gets one here: Radix then waits for "animationend" to unmount it.
    const real = window.getComputedStyle.bind(window)
    vi.spyOn(window, "getComputedStyle").mockImplementation((element, pseudo) => {
      const style = real(element, pseudo)
      if (element.getAttribute("data-slot") !== "dialog-content") return style
      // Live, like a real computed style: "exit" once the dialog is closing.
      const animationName = () => (element.getAttribute("data-state") === "closed" ? "exit" : "enter")
      return new Proxy(style, {
        get: (target, key) => (key === "animationName" ? animationName() : Reflect.get(target, key)),
      })
    })
    const { backend } = await renderSettings("circus")
    await settle(backend.client.projects.updateProject({ projectId: "circus", repoUrl: "git@h:x" }))
    fireEvent.change(nameInput(), { target: { value: "circus-app" } })
    fireEvent.click(save())
    await advance(50)
    const reload = within(screen.getByRole("alert")).getByRole("button", { name: "Reload" })
    reload.focus()
    fireEvent.keyDown(reload, { key: "Enter" })
    fireEvent.click(reload)
    await advance(0)
    const dialog = screen.getByRole("dialog", { name: "Discard your changes?" })
    const discard = within(dialog).getByRole("button", { name: "Discard and reload" })
    discard.focus()
    fireEvent.keyDown(discard, { key: "Enter" })
    fireEvent.click(discard)
    // The reload finishes while the dialog is still closing.
    await advance(300)
    expect(screen.queryAllByText("This project was changed elsewhere.")).toEqual([])
    expect(dialog.isConnected).toBe(true)
    fireEvent.animationEnd(dialog, { animationName: "exit" })
    await advance(0)
    expect(dialog.isConnected).toBe(false)
    expect(document.activeElement).toBe(nameInput())
  })
})

describe("the repository URL of a location's folder", () => {
  const CIRCUS = "https://github.com/eric/circus.git"
  const PALACE = "https://git.example.com/infra/palace.git"
  const withUrl = (url: string) => {
    const state = seedState()
    state.projects.find((p) => p.projectId === "circus")!.repoUrl = url
    return state
  }
  const repoUrl = () => document.getElementById("project-repo-url") as HTMLInputElement
  // The page's "Saved" note is a status too.
  const note = () => document.getElementById("project-repo-url-remote")!
  const spoken = () => note().querySelector(".sr-only")?.textContent ?? ""
  const path = (n: number) => screen.getByRole<HTMLInputElement>("textbox", { name: `Path for location ${n}` })
  /** Leaves a path field (after typing `value`, if given) for `next`, as focus really moves. */
  async function leavePath(n: number, value?: string, next: () => HTMLElement = nameInput) {
    act(() => path(n).focus())
    if (value !== undefined) fireEvent.change(path(n), { target: { value } })
    act(() => next().focus())
    await advance(50)
  }

  it("is read only when a path is left, and hinted at without overwriting the saved URL", async () => {
    const { backend } = await renderSettings("circus", withUrl("https://example.com/mine.git"))
    const read = vi.spyOn(backend.client.machines, "getGitRemote")
    await advance(1_000)
    expect(read).not.toHaveBeenCalled()
    expect(note().textContent).toBe("")

    await leavePath(2)
    expect(read).toHaveBeenCalledWith({ device: "devbox", path: "/home/eric/src/circus" })
    expect(repoUrl().value).toBe("https://example.com/mine.git")
    expect(note().getAttribute("role")).toBe("status")
    expect(spoken()).toBe(`devbox:/home/eric/src/circus uses ${CIRCUS}`)
    expect(save().getAttribute("aria-disabled")).toBe("true")

    await i18n.changeLanguage("zh-CN")
    expect(spoken()).toBe(`devbox:/home/eric/src/circus 使用 ${CIRCUS}`)
    const use = screen.getByRole("button", { name: "改用此地址" })
    fireEvent.pointerDown(use)
    fireEvent.click(use)
    expect(repoUrl().value).toBe(CIRCUS)
    expect(spoken()).toBe("读取自 devbox:/home/eric/src/circus")
    // Focus stays in the form rather than falling back to the page.
    expect(document.activeElement).toBe(repoUrl())
    expect(screen.getByRole("button", { name: "保存" }).getAttribute("aria-disabled")).toBeNull()
  })

  it("says nothing when the saved URL is the folder's", async () => {
    const { backend } = await renderSettings("circus", withUrl(CIRCUS))
    const read = vi.spyOn(backend.client.machines, "getGitRemote")
    await leavePath(2)
    expect(read).toHaveBeenCalledWith({ device: "devbox", path: "/home/eric/src/circus" })
    expect(note().textContent).toBe("")
  })

  it("forgets the note once saved", async () => {
    await renderSettings("circus", withUrl("https://example.com/mine.git"))
    await leavePath(2)
    fireEvent.click(screen.getByRole("button", { name: "Use this URL" }))
    expect(spoken()).toBe("Read from devbox:/home/eric/src/circus")
    fireEvent.click(save())
    await advance(50)
    expect(repoUrl().value).toBe(CIRCUS)
    expect(note().textContent).toBe("")
  })

  it("forgets the hint when the form is discarded and reloaded", async () => {
    const { backend } = await renderSettings("circus", withUrl("https://example.com/mine.git"))
    vi.setSystemTime(Date.parse("2026-11-17T12:01:00Z"))
    await settle(backend.client.projects.updateProject({ projectId: "circus", name: "circus-ng" }))
    await leavePath(2, "/srv/palace")
    expect(spoken()).toBe(`devbox:/srv/palace uses ${PALACE}`)
    fireEvent.click(save())
    await advance(50)
    fireEvent.click(within(screen.getByRole("alert")).getByRole("button", { name: "Reload" }))
    await advance(0)
    fireEvent.click(screen.getByRole("button", { name: "Discard and reload" }))
    await advance(50)
    expect(path(2).value).toBe("/home/eric/src/circus")
    expect(note().textContent).toBe("")
    expect(screen.queryByRole("button", { name: "Use this URL" })).toBeNull()
  })

  it("never saves a URL read by pressing Save right after changing a path", async () => {
    const { state } = await renderSettings("circus")
    // A press on Save takes focus from the path field before the click.
    await leavePath(2, "/srv/palace", save)
    fireEvent.click(save())
    await advance(500)
    const saved = state.projects.find((p) => p.projectId === "circus")!
    expect(saved.locations[1].path).toBe("/srv/palace")
    expect(saved.repoUrl).toBe("")
    expect(repoUrl().value).toBe("")
  })

  it("never saves a URL read by pressing Save where a click doesn't move focus", async () => {
    const { state } = await renderSettings("circus")
    act(() => path(2).focus())
    fireEvent.change(path(2), { target: { value: "/srv/palace" } })
    // Safari: the press blurs the field without focusing the button.
    fireEvent.pointerDown(save())
    act(() => path(2).blur())
    await advance(20)
    fireEvent.pointerUp(save())
    fireEvent.click(save())
    await advance(500)
    const saved = state.projects.find((p) => p.projectId === "circus")!
    expect(saved.locations[1].path).toBe("/srv/palace")
    expect(saved.repoUrl).toBe("")
  })
})

describe("layout", () => {
  it("keeps name, repository URL and locations in that order, without default-directory notes or a derived name", async () => {
    await renderSettings("circus")
    const repo = document.getElementById("project-repo-url") as HTMLInputElement
    const firstPath = screen.getByRole("textbox", { name: "Path for location 1" })
    expect(nameInput().compareDocumentPosition(repo) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    expect(repo.compareDocumentPosition(firstPath) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "Add location" }))
    await advance(10)
    // The new row uses its device's default directory.
    const added = screen.getByRole("textbox", { name: "Path for location 3" })
    expect((added as HTMLInputElement).value).toBe("")
    expect(added.hasAttribute("aria-describedby")).toBe(false)
    expect(screen.queryByText("An empty folder is created here")).toBeNull()
    fireEvent.change(nameInput(), { target: { value: "" } })
    expect(nameInput().placeholder).toBe("")
  })
})


describe("default folders", () => {
  /**
   * The settings of "fresh", whose locations are default folders on these
   * devices, after starting a session in each and then running `edit` on
   * the mock's state.
   */
  async function renderFresh(repoUrl: string, devices: string[], edit: (state: MockState) => void = () => {}) {
    const state = withDefaultProject(repoUrl, devices)
    const result = await renderSettings("fresh", state)
    for (const [locationIndex] of devices.entries()) {
      const created = result.backend.client.sessions.createSession({
        projectId: "fresh",
        locationIndex,
        clientRequestId: crypto.randomUUID(),
      })
      await advance(10)
      await created
    }
    edit(state)
    act(() => void result.queryClient.invalidateQueries({ queryKey: queryKeys.projects }))
    await advance(10)
    return result
  }

  /** The line under the default location on `device`. */
  const statusOf = (device: string) => {
    const select = screen
      .getAllByRole("combobox", { name: /^Device for location/ })
      .find((c) => c.textContent?.startsWith(device))!
    return select.closest("li")!.querySelector<HTMLElement>("[data-workspace]")
  }

  it("shows each folder's state, and its absolute path once ready", async () => {
    const { state } = await renderFresh("https://example.com/fresh.git", ["devbox", "eric-mbp", "eric-win"])
    // Sessions just started: pending everywhere.
    expect(statusOf("devbox")?.textContent).toBe("Preparing starts shortly")
    // While folders only wait, the projects are read again once a minute.
    await advance(PROJECTS_WAITING_INTERVAL_MS)
    expect(statusOf("eric-win")?.textContent).toBe("Waiting for eric-win to come online")
    const devbox = statusOf("devbox")!
    expect(devbox.textContent).toBe("Ready at /home/eric/circus/projects/fresh")
    expect(statusOf("eric-mbp")?.textContent).toBe("Ready at /Users/eric/circus/projects/fresh")
    // The path field describes it.
    const path = screen.getByRole("textbox", { name: "Path for location 1" })
    expect(document.getElementById(path.getAttribute("aria-describedby")!)?.textContent).toBe(devbox.textContent)
    expect(state.sessions.length).toBeGreaterThan(0)
  })

  it("says what a folder is waiting for or why it failed", async () => {
    const { queryClient } = await renderFresh("https://example.com/slow.git", ["devbox", "eric-mbp", "palace-builder"], (state) => {
      const ws = (device: string) => state.sessions.find((s) => s.projectId === "fresh" && s.device === device)!.workspace!
      Object.assign(ws("eric-mbp"), { state: "failed", error: "git_unavailable" })
      Object.assign(ws("palace-builder"), { waiting: "daemon_outdated" })
    })
    expect(statusOf("eric-mbp")?.textContent).toBe("Couldn't prepare the folder: git is not installed on eric-mbp")
    expect(statusOf("palace-builder")?.textContent).toBe("Update circus on palace-builder to prepare the folder")
    await advance(1_000)
    act(() => void queryClient.invalidateQueries({ queryKey: queryKeys.projects }))
    await advance(10)
    expect(statusOf("devbox")?.textContent).toBe("Cloning the repository…")
  })

  it("leaves a location without a folder as it was", async () => {
    await renderSettings("circus")
    expect(document.querySelector("[data-workspace]")).toBeNull()
  })
})

describe("deleting a project with default folders", () => {
  /**
   * "fresh" with ready folders on devbox and eric-mbp, a pending one on the
   * offline eric-win, none on palace-builder, every session stopped, and
   * then `edit` run on the mock's state.
   */
  async function renderDeletable(
    repoUrl: string,
    wrapClient?: (client: CircusClient) => CircusClient,
    devices = ["devbox", "eric-mbp", "eric-win", "palace-builder"],
    edit: (state: MockState) => void = () => {},
  ) {
    const state = withDefaultProject(repoUrl, devices)
    const router = createMemoryRouter(
      [
        {
          path: "/p/:projectId/settings",
          element: (
            <SidebarProvider>
              <ProjectSettingsPage />
            </SidebarProvider>
          ),
        },
        { path: "*", element: <h1>Home</h1> },
      ],
      { initialEntries: ["/p/fresh/settings"] },
    )
    const result = await renderWithBackend(<RouterProvider router={router} />, { state, wrapClient })
    const { client } = result.backend
    for (const [locationIndex, device] of devices.entries()) {
      if (device === "palace-builder") continue
      const created = client.sessions.createSession({ projectId: "fresh", locationIndex, clientRequestId: crypto.randomUUID() })
      await advance(10)
      await created
    }
    await advance(4_000)
    for (const s of state.sessions) if (s.projectId === "fresh") s.state = "stopped"
    edit(state)
    act(() => void result.queryClient.invalidateQueries())
    await advance(10)
    return { ...result, router }
  }

  const openDialog = async (name = "Delete project") => {
    fireEvent.click(screen.getByRole("button", { name }))
    await advance(10)
    return screen.getByRole("dialog")
  }

  it("offers to delete ready folders on online devices, unticked, and says the others stay", async () => {
    const requests: { projectId: string; removeWorkspacesOn?: string[] }[] = []
    const { router } = await renderDeletable(
      "",
      (client) => ({
        ...client,
        projects: {
          ...client.projects,
          deleteProject: (req) => {
            requests.push(req)
            return client.projects.deleteProject(req)
          },
        },
      }),
    )
    const dialog = within(await openDialog())
    const devbox = dialog.getByRole("checkbox", { name: "Also delete /home/eric/circus/projects/fresh on devbox" })
    const mbp = dialog.getByRole("checkbox", { name: "Also delete /Users/eric/circus/projects/fresh on eric-mbp" })
    expect(dialog.getAllByRole("checkbox")).toHaveLength(2)
    expect(devbox.getAttribute("aria-checked")).toBe("false")
    expect(mbp.getAttribute("aria-checked")).toBe("false")
    // eric-win's folder was never prepared (it is offline): it can't stay.
    // No folder on palace-builder either: nothing about them.
    expect(dialog.queryByText(/eric-win|palace-builder/)).toBeNull()

    // A label click ticks too.
    fireEvent.click(dialog.getByText("Also delete /Users/eric/circus/projects/fresh on eric-mbp"))
    await advance(0)
    expect(mbp.getAttribute("aria-checked")).toBe("true")
    fireEvent.click(dialog.getByRole("button", { name: "Delete" }))
    await advance(50)
    expect(requests).toEqual([{ projectId: "fresh", removeWorkspacesOn: ["eric-mbp"] }])
    expect(router.state.location.pathname).toBe("/")
    // Everything was removed: nothing to report.
    expect(screen.queryByRole("region", { name: "fresh was deleted" })).toBeNull()
  })

  it("says a ready folder on a device that is offline now stays there", async () => {
    await renderDeletable("", undefined, undefined, (state) => {
      state.devices.find((d) => d.name === "eric-mbp")!.online = false
    })
    const dialog = within(await openDialog())
    expect(dialog.getAllByRole("checkbox")).toHaveLength(1)
    expect(dialog.getByText("eric-mbp is offline, so /Users/eric/circus/projects/fresh stays on it.")).toBeTruthy()
  })

  it("drops the tick of a device that goes offline while the dialog is open, and keeps focus in it", async () => {
    const requests: { removeWorkspacesOn?: string[] }[] = []
    const { state } = await renderDeletable("", (client) => ({
      ...client,
      projects: { ...client.projects, deleteProject: (req) => (requests.push(req), client.projects.deleteProject(req)) },
    }))
    const dialog = within(await openDialog())
    const mbp = dialog.getByRole("checkbox", { name: "Also delete /Users/eric/circus/projects/fresh on eric-mbp" })
    fireEvent.click(mbp)
    mbp.focus()
    await advance(0)
    expect(mbp.getAttribute("aria-checked")).toBe("true")

    // The dialog reads the devices live.
    state.devices.find((d) => d.name === "eric-mbp")!.online = false
    await advance(10_000)
    expect(dialog.queryByRole("checkbox", { name: /eric-mbp/ })).toBeNull()
    expect(dialog.getByText("eric-mbp is offline, so /Users/eric/circus/projects/fresh stays on it.")).toBeTruthy()
    expect(document.activeElement).toBe(dialog.getByRole("checkbox", { name: /devbox/ }))

    // Back online: unticked.
    state.devices.find((d) => d.name === "eric-mbp")!.online = true
    await advance(10_000)
    expect(dialog.getByRole("checkbox", { name: /eric-mbp/ }).getAttribute("aria-checked")).toBe("false")
    fireEvent.click(dialog.getByRole("button", { name: "Delete" }))
    await advance(50)
    expect(requests).toEqual([{ projectId: "fresh", removeWorkspacesOn: [] }])
  })

  it("says it is checking the devices until it knows which are online", async () => {
    let release = () => {}
    let hold = false
    const { queryClient } = await renderDeletable("", (client) => ({
      ...client,
      machines: {
        ...client.machines,
        listDevices: async () => {
          if (hold) await new Promise<void>((resolve) => (release = resolve))
          return client.machines.listDevices()
        },
      },
    }))
    hold = true
    act(() => queryClient.removeQueries({ queryKey: queryKeys.devices }))
    const dialog = within(await openDialog())
    expect(dialog.getByRole("status").textContent).toBe("Checking which devices are online…")
    expect(dialog.queryAllByRole("checkbox")).toEqual([])
    act(() => release())
    await advance(50)
    expect(dialog.queryByRole("status")).toBeNull()
    expect(dialog.getAllByRole("checkbox")).toHaveLength(2)
  })

  it("offers nothing for a folder that isn't ready", async () => {
    await renderDeletable("https://example.com/slow.git", undefined, ["devbox", "eric-mbp"])
    const dialog = await openDialog()
    expect(within(dialog).queryAllByRole("checkbox")).toEqual([])
    expect(within(dialog).queryByText(/eric-mbp|devbox/)).toBeNull()
  })

  it("sends no device when nothing is ticked", async () => {
    const requests: { removeWorkspacesOn?: string[] }[] = []
    await renderDeletable("", (client) => ({
      ...client,
      projects: { ...client.projects, deleteProject: (req) => (requests.push(req), client.projects.deleteProject(req)) },
    }))
    const dialog = within(await openDialog())
    fireEvent.click(dialog.getByRole("button", { name: "Delete" }))
    await advance(50)
    expect(requests).toEqual([{ projectId: "fresh", removeWorkspacesOn: [] }])
  })

  it("reports folders that couldn't be deleted or were kept", async () => {
    await renderDeletable("https://example.com/remove-fails.git", (client) => ({
      ...client,
      projects: {
        ...client.projects,
        deleteProject: async (req) => {
          const res = await client.projects.deleteProject(req)
          // eric-mbp went offline meanwhile.
          return {
            workspaceRemovals: res.workspaceRemovals.map((r) =>
              r.device === "eric-mbp" ? { ...r, outcome: "kept" as const, error: "unspecified" as const } : r,
            ),
          }
        },
      },
    }))
    const dialog = within(await openDialog())
    for (const box of dialog.getAllByRole("checkbox")) fireEvent.click(box)
    await advance(0)
    fireEvent.click(dialog.getByRole("button", { name: "Delete" }))
    await advance(50)
    expect(screen.getByRole("heading", { name: "Home" })).toBeTruthy()
    const notice = within(screen.getByRole("region", { name: "fresh was deleted" }))
    expect(
      notice.getByText("Couldn't delete /home/eric/circus/projects/fresh on devbox: permission denied"),
    ).toBeTruthy()
    expect(
      notice.getByText(
        "Kept /Users/eric/circus/projects/fresh on eric-mbp: the device was offline, out of date, had no ready folder or didn't answer in time.",
      ),
    ).toBeTruthy()
    // The device's own words, in the code font.
    expect(notice.getByText(/Permission denied$/).className).toContain("font-mono")
    fireEvent.click(notice.getByRole("button", { name: "Close" }))
    await advance(NOTICE_FOLD_MS)
    expect(screen.queryByRole("region", { name: "fresh was deleted" })).toBeNull()
  })

  it("keeps a failure until the user has done something, then lets it go by itself", async () => {
    await renderDeletable("https://example.com/remove-fails.git")
    const dialog = within(await openDialog())
    fireEvent.click(dialog.getAllByRole("checkbox")[0])
    await advance(0)
    fireEvent.click(dialog.getByRole("button", { name: "Delete" }))
    await advance(50)
    const region = () => screen.queryByRole("region", { name: "fresh was deleted" })
    // The click on Delete came before it: not seen yet.
    await advance(60_000)
    expect(region()).not.toBeNull()
    fireEvent.pointerDown(document.body)
    await advance(NOTICE_VISIBLE_MS - 100)
    expect(region()).not.toBeNull()
    await advance(100)
    // It folds away.
    await advance(NOTICE_FOLD_MS)
    expect(region()).toBeNull()
  })

  it("doesn't go by itself while the pointer is on it or focus in it", async () => {
    await renderDeletable("https://example.com/remove-fails.git")
    fireEvent.click(within(await openDialog()).getAllByRole("checkbox")[0])
    await advance(0)
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete" }))
    await advance(50)
    const region = () => screen.queryByRole("region", { name: "fresh was deleted" })
    const box = region()!.parentElement!
    fireEvent.pointerDown(document.body)
    fireEvent.pointerEnter(box)
    await advance(NOTICE_VISIBLE_MS * 3)
    expect(region()).not.toBeNull()
    fireEvent.pointerLeave(box)
    const close = within(region()!).getByRole("button", { name: "Close" })
    act(() => close.focus())
    await advance(NOTICE_VISIBLE_MS * 3)
    expect(region()).not.toBeNull()
    // Counted again once it lets go.
    act(() => close.blur())
    await advance(NOTICE_VISIBLE_MS)
    await advance(NOTICE_FOLD_MS)
    expect(region()).toBeNull()
  })

  it("puts the notice above the page, not over it", async () => {
    await renderDeletable("https://example.com/remove-fails.git")
    fireEvent.click(within(await openDialog()).getAllByRole("checkbox")[0])
    await advance(0)
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Delete" }))
    await advance(50)
    const notice = screen.getByRole("region", { name: "fresh was deleted" })
    // In the flow of the page, before it; nothing positions it on top.
    expect(notice.compareDocumentPosition(screen.getByRole("heading", { name: "Home" })) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    for (let el: HTMLElement | null = notice; el; el = el.parentElement) {
      expect(el.className).not.toMatch(/\b(fixed|absolute)\b/)
    }
  })

  it("speaks Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderDeletable("", undefined, undefined, (state) => {
      state.devices.find((d) => d.name === "eric-mbp")!.online = false
    })
    const dialog = within(await openDialog("删除项目"))
    expect(dialog.getByRole("checkbox", { name: "同时删除 devbox 上的工作目录 /home/eric/circus/projects/fresh" })).toBeTruthy()
    expect(dialog.getByText("eric-mbp 离线，工作目录 /Users/eric/circus/projects/fresh 会保留在该设备上。")).toBeTruthy()
  })
})

describe("deleting a project with a ledger", () => {
  it("says that the ledger goes too, with its number of decisions, and forgets it", async () => {
    const { queryClient } = await renderSettings("circus", withStoppedSessions(seedState(), "circus"))
    queryClient.setQueryData(queryKeys.decision("circus", 4, ""), { number: 4 })
    fireEvent.click(screen.getByRole("button", { name: "Delete project" }))
    await advance(10)
    const dialog = within(screen.getByRole("dialog"))
    expect(dialog.getByText("Its stopped sessions and its Ledger, 16 decisions with their history, are deleted too.")).toBeTruthy()
    expect(queryClient.getQueryData(queryKeys.ledger("circus"))).toBeTruthy()
    fireEvent.click(dialog.getByRole("button", { name: "Delete" }))
    await advance(50)
    expect(queryClient.getQueryCache().findAll({ queryKey: queryKeys.ledgerAll("circus") })).toEqual([])
  })

  it("names the ledger even when it is empty", async () => {
    await renderSettings("toolbox", withStoppedSessions(seedState(), "toolbox"))
    fireEvent.click(screen.getByRole("button", { name: "Delete project" }))
    await advance(10)
    expect(within(screen.getByRole("dialog")).getByText(
      "Its stopped sessions and its Ledger (decisions and their history) are deleted too.",
    )).toBeTruthy()
  })
})

describe("the default provider", () => {
  const picker = () => screen.getByRole("combobox", { name: "Default provider" })
  async function choose(name: string) {
    fireEvent.click(picker())
    await advance(0)
    fireEvent.click(screen.getByRole("option", { name }))
    await advance(0)
  }

  it("is saved with the rest of the form, moving the version", async () => {
    const { state } = await renderSettings("circus")
    expect(picker().textContent).toBe("None")
    expect(
      screen.getByText("Without one, new sessions use your default provider, or local login if you have none."),
    ).toBeTruthy()
    const before = state.projects.find((p) => p.projectId === "circus")!.updatedAtUnixMs
    fireEvent.click(picker())
    await advance(0)
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual(["None", "Anthropic", "Legacy key", "Team proxy"])
    fireEvent.click(screen.getByRole("option", { name: "Team proxy" }))
    await advance(0)
    expect(screen.getByText("New sessions in this project use it unless they pick another.")).toBeTruthy()
    vi.setSystemTime(Date.parse("2026-11-17T12:01:00Z"))
    fireEvent.click(save())
    await advance(50)
    const project = state.projects.find((p) => p.projectId === "circus")!
    expect(project.defaultProviderId).toBe("provider-proxy")
    expect(project.updatedAtUnixMs).toBeGreaterThan(before)
    expect(screen.getByText("Saved")).toBeTruthy()

    await choose("None")
    fireEvent.click(save())
    await advance(50)
    expect(state.projects.find((p) => p.projectId === "circus")!.defaultProviderId).toBe("")
  })

  it("is sent only when it changed, so renaming works while it names a deleted provider", async () => {
    const state = seedState()
    state.projects.find((p) => p.projectId === "circus")!.defaultProviderId = "provider-gone"
    const sent: (string | undefined)[] = []
    await renderSettings("circus", state, (client) => ({
      ...client,
      projects: {
        ...client.projects,
        updateProject: (req) => (sent.push(req.defaultProviderId), client.projects.updateProject(req)),
      },
    }))
    fireEvent.change(nameInput(), { target: { value: "circus-web" } })
    fireEvent.click(save())
    await advance(50)
    expect(sent).toEqual([undefined])
    expect(state.projects.find((p) => p.projectId === "circus")!.name).toBe("circus-web")
    expect(screen.getByText("Saved")).toBeTruthy()
  })

  it("says when it names a deleted provider", async () => {
    const state = seedState()
    state.projects.find((p) => p.projectId === "circus")!.defaultProviderId = "provider-gone"
    await renderSettings("circus", state)
    expect(picker().textContent).toBe("Deleted provider")
    expect(picker().getAttribute("aria-invalid")).toBe("true")
  })

  it("says so when the provider was deleted before the save", async () => {
    const { state } = await renderSettings("circus")
    await choose("Team proxy")
    state.providers = state.providers.filter((p) => p.providerId !== "provider-proxy")
    fireEvent.click(save())
    await advance(50)
    expect(screen.getByText("That provider no longer exists. Choose another.")).toBeTruthy()
    // The list is fetched again, so the choice reads as deleted.
    expect(picker().textContent).toBe("Deleted provider")
    await choose("Anthropic")
    expect(screen.queryByText("That provider no longer exists. Choose another.")).toBeNull()
  })

  it("is caught up when another tab changes it on an untouched form", async () => {
    const { backend, queryClient } = await renderSettings("circus")
    vi.setSystemTime(Date.parse("2026-11-17T12:01:00Z"))
    await settle(backend.client.projects.updateProject({ projectId: "circus", defaultProviderId: "provider-anthropic" }))
    act(() => void queryClient.invalidateQueries({ queryKey: queryKeys.projects }))
    await advance(50)
    expect(picker().textContent).toBe("Anthropic")
  })
})
