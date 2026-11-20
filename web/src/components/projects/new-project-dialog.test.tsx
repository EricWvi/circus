// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { SLOW_GIT_REMOTE_MS } from "@/api/mock/directories"
import type { CircusClient } from "@/api/types"
import { advance, renderWithBackend } from "@/test/render"

import { useOpenNewProject } from "./new-project-context"
import { NewProjectDialogProvider } from "./new-project-dialog"

beforeEach(() => {
  vi.useFakeTimers()
  localStorage.clear()
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

function Opener() {
  const open = useOpenNewProject()
  return (
    <button type="button" onClick={open}>
      open
    </button>
  )
}

/** Opens the new project dialog, counting the git remote reads. */
async function openDialog() {
  const reads: { device: string; path: string }[] = []
  const router = createMemoryRouter(
    [
      {
        path: "*",
        element: (
          <NewProjectDialogProvider>
            <Opener />
          </NewProjectDialogProvider>
        ),
      },
    ],
    { initialEntries: ["/"] },
  )
  const wrapClient = (client: CircusClient): CircusClient => ({
    ...client,
    machines: {
      ...client.machines,
      getGitRemote: (req) => {
        reads.push(req)
        return client.machines.getGitRemote(req)
      },
    },
  })
  const { state } = await renderWithBackend(<RouterProvider router={router} />, { wrapClient })
  fireEvent.click(screen.getByRole("button", { name: "open" }))
  await advance(50)
  return { reads, state }
}

const PALACE = "https://git.example.com/infra/palace.git"
const CIRCUS = "https://github.com/eric/circus.git"

const nameInput = () => document.getElementById("new-project-name") as HTMLInputElement
const repoUrl = () => document.getElementById("new-project-repo-url") as HTMLInputElement
const path = () => screen.getByRole<HTMLInputElement>("textbox", { name: "Path for location 1" })
const browse = () => screen.getByRole("button", { name: "Browse folders on devbox" })
/** The live region under the repository URL field. */
const status = () => document.getElementById("new-project-repo-url-remote")!
/** What screen readers get from it. */
const spoken = () => status().querySelector(".sr-only")?.textContent ?? ""
/** What it shows. */
const shown = () => status().querySelector("[aria-hidden]")?.textContent ?? ""

/** Types into the path field and leaves it for `next` (the name field by default), as focus really moves. */
async function typePath(value: string, next: () => HTMLElement = nameInput) {
  act(() => path().focus())
  fireEvent.change(path(), { target: { value } })
  act(() => next().focus())
  await advance(10)
}

/** Opens the picker from the path field, as a click on Browse moves focus, and selects a folder in it. */
async function pick(folder: string) {
  act(() => path().focus())
  act(() => browse().focus())
  fireEvent.click(browse())
  await advance(10)
  fireEvent.click(screen.getByRole("option", { name: new RegExp(`^${folder}`) }))
  await advance(10)
  fireEvent.click(screen.getByRole("button", { name: "Select this folder" }))
  await advance(10)
}

describe("new project repository URL from a folder", () => {
  it("fills an empty field from a picked folder, with a note until edited", async () => {
    const { reads } = await openDialog()
    // The live region is there, empty, before anything is read.
    expect(status().getAttribute("role")).toBe("status")
    expect(status().textContent).toBe("")
    fireEvent.change(path(), { target: { value: "/home/eric/src" } })
    // Leaving the path for Browse doesn't read the half-typed path.
    await pick("circus")
    expect(reads).toEqual([{ device: "devbox", path: "/home/eric/src/circus" }])
    expect(repoUrl().value).toBe(CIRCUS)
    expect(spoken()).toBe("Read from devbox:/home/eric/src/circus")
    expect(shown()).toBe("Read from devbox:/home/eric/src/circus")
    expect(repoUrl().getAttribute("aria-describedby")).toBe(status().id)
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/eric/circus" } })
    expect(status().textContent).toBe("")
    expect(repoUrl().hasAttribute("aria-describedby")).toBe(false)
  })

  it("replaces its own fill when another folder is picked", async () => {
    const { reads } = await openDialog()
    await typePath("/srv/palace")
    expect(repoUrl().value).toBe(PALACE)
    fireEvent.change(path(), { target: { value: "/home/eric/src" } })
    await pick("circus")
    expect(reads.map((r) => r.path)).toEqual(["/srv/palace", "/home/eric/src/circus"])
    expect(repoUrl().value).toBe(CIRCUS)
    expect(spoken()).toBe("Read from devbox:/home/eric/src/circus")
  })

  it("hints at a typed folder's URL when the field has another; a click uses it and focuses the field quietly", async () => {
    const { reads } = await openDialog()
    fireEvent.change(repoUrl(), { target: { value: "https://example.com/mine.git" } })
    await typePath("/home/eric/src/dotfiles")
    expect(reads).toHaveLength(1)
    expect(repoUrl().value).toBe("https://example.com/mine.git")
    expect(spoken()).toBe("devbox:/home/eric/src/dotfiles uses git@github.com:eric/dotfiles.git")
    const use = screen.getByRole("button", { name: "Use this URL" })
    const focus = vi.spyOn(repoUrl(), "focus")
    fireEvent.pointerDown(use)
    act(() => use.focus())
    fireEvent.click(use)
    expect(repoUrl().value).toBe("git@github.com:eric/dotfiles.git")
    expect(spoken()).toBe("Read from devbox:/home/eric/src/dotfiles")
    expect(document.activeElement).toBe(repoUrl())
    expect(focus).toHaveBeenCalledWith({ focusVisible: false })

    // Leaving the path again without changing it doesn't read again.
    await typePath("/home/eric/src/dotfiles")
    expect(reads).toHaveLength(1)
  })

  it("moves keyboard focus to the field, with a ring, when the hint's button goes away", async () => {
    await openDialog()
    fireEvent.change(repoUrl(), { target: { value: "https://example.com/mine.git" } })
    await typePath("/srv/palace")
    const use = screen.getByRole("button", { name: "Use this URL" })
    const focus = vi.spyOn(repoUrl(), "focus")
    act(() => use.focus())
    fireEvent.keyDown(document, { key: "Enter" })
    fireEvent.click(use)
    expect(repoUrl().value).toBe(PALACE)
    expect(document.activeElement).toBe(repoUrl())
    expect(focus).toHaveBeenCalledWith({ focusVisible: true })
  })

  it("shortens a long path on screen, keeping the whole of it in a title and for screen readers", async () => {
    await openDialog()
    await typePath("/home/eric/src/circus/internal/controlplane/webapi")
    expect(shown()).toBe("Read from devbox:/home/eric/s…internal/controlplane/webapi")
    expect(spoken()).toBe("Read from devbox:/home/eric/src/circus/internal/controlplane/webapi")
    const source = screen.getByText("devbox:/home/eric/s…internal/controlplane/webapi")
    expect(source.getAttribute("title")).toBe("devbox:/home/eric/src/circus/internal/controlplane/webapi")
  })

  it("doesn't read the default directory, and drops answers for a path changed since", async () => {
    const { reads } = await openDialog()
    await typePath("")
    expect(reads).toHaveLength(0)
    await typePath("/home/eric/src/toolbox")
    expect(reads).toHaveLength(1)
    fireEvent.change(path(), { target: { value: "/home/eric/src/toolbo" } })
    await advance(SLOW_GIT_REMOTE_MS + 50)
    expect(repoUrl().value).toBe("")
    expect(status().textContent).toBe("")
  })

  it("stays quiet for folders without an origin or outside repositories", async () => {
    const { reads } = await openDialog()
    await typePath("/home/eric/src/palace")
    await typePath("/home/eric/Documents")
    expect(reads).toHaveLength(2)
    expect(repoUrl().value).toBe("")
    expect(status().textContent).toBe("")
  })

  it("only hints when the path is left for the empty repository URL field", async () => {
    await openDialog()
    await typePath("/srv/palace", repoUrl)
    expect(repoUrl().value).toBe("")
    expect(spoken()).toBe(`devbox:/srv/palace uses ${PALACE}`)
  })

  it.each([
    ["a fast device", "/srv/palace"],
    ["a slow device", "/home/eric/src/toolbox"],
  ])("never saves a URL read by pressing Create right after typing a path (%s)", async (_, folder) => {
    const { state, reads } = await openDialog()
    fireEvent.change(nameInput(), { target: { value: "Probe" } })
    const create = screen.getByRole("button", { name: "Create" })
    // A press on Create takes focus from the path field before the click.
    await typePath(folder, () => create)
    expect(reads).toHaveLength(1)
    fireEvent.click(create)
    await advance(SLOW_GIT_REMOTE_MS + 500)
    expect(state.projects.find((p) => p.name === "Probe")?.repoUrl).toBe("")
  })

  it("only hints for answers that arrive after a refused submit", async () => {
    await openDialog()
    act(() => path().focus())
    fireEvent.change(path(), { target: { value: "/home/eric/src/toolbox" } })
    act(() => nameInput().focus())
    // The name is missing: the submit is refused while the read is under way.
    fireEvent.click(screen.getByRole("button", { name: "Create" }))
    await advance(SLOW_GIT_REMOTE_MS + 50)
    expect(repoUrl().value).toBe("")
    expect(spoken()).toBe("devbox:/home/eric/src/toolbox uses git@gitlab.com:eric/toolbox.git")
  })

  it("reads a typed path when Tab moves on past the Browse button", async () => {
    const { reads } = await openDialog()
    act(() => path().focus())
    fireEvent.change(path(), { target: { value: "/srv/palace" } })
    // Tab lands on the row's Browse button first: no read yet.
    act(() => browse().focus())
    expect(reads).toHaveLength(0)
    act(() => screen.getByRole("button", { name: "Remove location 1" }).focus())
    await advance(10)
    expect(reads).toEqual([{ device: "devbox", path: "/srv/palace" }])
    expect(repoUrl().value).toBe(PALACE)
  })

  it("reads a typed path when the picker closes without a selection", async () => {
    const { reads } = await openDialog()
    act(() => path().focus())
    fireEvent.change(path(), { target: { value: "/srv/palace" } })
    act(() => browse().focus())
    fireEvent.click(browse())
    await advance(10)
    const picker = screen.getByRole("dialog", { name: "Folders on devbox" })
    expect(reads).toHaveLength(0)
    fireEvent.keyDown(within(picker).getByRole("combobox", { name: "Folder path" }), { key: "Escape" })
    await advance(10)
    expect(reads).toEqual([{ device: "devbox", path: "/srv/palace" }])
    expect(repoUrl().value).toBe(PALACE)
    // Focus is back on Browse; leaving it doesn't read again.
    act(() => nameInput().focus())
    await advance(10)
    expect(reads).toHaveLength(1)
  })

  it("only hints when focus moves from Browse to Create", async () => {
    await openDialog()
    act(() => path().focus())
    fireEvent.change(path(), { target: { value: "/srv/palace" } })
    act(() => browse().focus())
    act(() => screen.getByRole("button", { name: "Create" }).focus())
    await advance(10)
    expect(repoUrl().value).toBe("")
    expect(spoken()).toBe(`devbox:/srv/palace uses ${PALACE}`)
  })

  it("never saves a URL read by pressing Create where a click doesn't move focus", async () => {
    const { state, reads } = await openDialog()
    fireEvent.change(nameInput(), { target: { value: "Probe" } })
    act(() => path().focus())
    fireEvent.change(path(), { target: { value: "/srv/palace" } })
    const create = screen.getByRole("button", { name: "Create" })
    // Safari: the press blurs the field without focusing the button.
    fireEvent.pointerDown(create)
    act(() => path().blur())
    await advance(20)
    expect(reads).toHaveLength(1)
    fireEvent.pointerUp(create)
    fireEvent.click(create)
    await advance(500)
    expect(state.projects.find((p) => p.name === "Probe")?.repoUrl).toBe("")
  })

  it("keeps the note while the field keeps the URL, after the path changes", async () => {
    await openDialog()
    await typePath("/srv/palace")
    expect(spoken()).toBe("Read from devbox:/srv/palace")
    // No origin there: the filled URL and its note stay.
    await typePath("/home/eric/src/palace")
    expect(repoUrl().value).toBe(PALACE)
    expect(spoken()).toBe("Read from devbox:/srv/palace")
  })

  it("leaves focus alone after a tap on Use this URL", async () => {
    await openDialog()
    fireEvent.change(repoUrl(), { target: { value: "https://example.com/mine.git" } })
    await typePath("/srv/palace")
    const use = screen.getByRole("button", { name: "Use this URL" })
    const focus = vi.spyOn(repoUrl(), "focus")
    fireEvent.pointerDown(use, { pointerType: "touch" })
    fireEvent.click(use)
    expect(repoUrl().value).toBe(PALACE)
    expect(focus).not.toHaveBeenCalled()
    expect(document.activeElement).not.toBe(repoUrl())
  })

  it("defers the read when a click on Browse doesn't move focus, reading the picked folder", async () => {
    const { reads } = await openDialog()
    act(() => path().focus())
    fireEvent.change(path(), { target: { value: "/home/eric/src" } })
    // Safari: the press blurs the field without focusing Browse.
    fireEvent.pointerDown(browse(), { button: 0 })
    act(() => path().blur())
    fireEvent.pointerUp(browse())
    fireEvent.click(browse())
    await advance(10)
    expect(reads).toHaveLength(0)
    fireEvent.click(screen.getByRole("option", { name: /^circus/ }))
    await advance(10)
    fireEvent.click(screen.getByRole("button", { name: "Select this folder" }))
    await advance(10)
    expect(reads).toEqual([{ device: "devbox", path: "/home/eric/src/circus" }])
    expect(repoUrl().value).toBe(CIRCUS)
  })

  it("reads at once when the path is left by a right-click on Browse", async () => {
    const { reads } = await openDialog()
    act(() => path().focus())
    fireEvent.change(path(), { target: { value: "/srv/palace" } })
    fireEvent.pointerDown(browse(), { button: 2 })
    act(() => path().blur())
    await advance(10)
    expect(reads).toEqual([{ device: "devbox", path: "/srv/palace" }])
  })
})
