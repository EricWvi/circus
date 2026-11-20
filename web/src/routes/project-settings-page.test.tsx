// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { seedState, type MockState } from "@/api/mock/state"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { advance, renderWithBackend } from "@/test/render"

import { ProjectSettingsPage } from "./project-settings-page"

async function renderSettings(projectId: string, state: MockState = seedState()) {
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
  const result = await renderWithBackend(<RouterProvider router={router} />, { state })
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

