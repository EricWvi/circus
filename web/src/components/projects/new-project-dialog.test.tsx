// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { SLOW_GIT_REMOTE_MS } from "@/api/mock/directories"
import type { MockState } from "@/api/mock/state"
import { seedState } from "@/api/mock/state"
import { queryKeys } from "@/api/query-client"
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
  vi.restoreAllMocks()
})

function Opener() {
  const open = useOpenNewProject()
  return (
    <button type="button" onClick={open}>
      open
    </button>
  )
}

/**
 * Opens the new project dialog, counting the git remote reads. `state` is
 * the mock server's data; `wrapClient` stands between the dialog and it.
 */
async function openDialog({
  state = seedState(),
  wrapClient: wrapMore = (client) => client,
}: { state?: MockState; wrapClient?: (client: CircusClient) => CircusClient } = {}) {
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
  const wrapClient = (backend: CircusClient): CircusClient => {
    const client = wrapMore(backend)
    return {
      ...client,
      machines: {
        ...client.machines,
        getGitRemote: (req) => {
          reads.push(req)
          return client.machines.getGitRemote(req)
        },
      },
    }
  }
  const { queryClient } = await renderWithBackend(<RouterProvider router={router} />, { state, wrapClient })
  fireEvent.click(screen.getByRole("button", { name: "open" }))
  await advance(50)
  return { reads, state, queryClient }
}

/**
 * Gives the dialog an exit animation that never ends here, so its form
 * stays mounted while it closes, as in a browser mid-animation.
 */
function stubExitAnimation() {
  const realStyle = globalThis.getComputedStyle
  vi.spyOn(globalThis, "getComputedStyle").mockImplementation((el, pseudo) => {
    const style = realStyle(el, pseudo)
    if (!(el instanceof HTMLElement) || el.getAttribute("role") !== "dialog") return style
    return new Proxy(style, {
      get: (target, key) => {
        if (key === "animationName") return el.dataset.state === "closed" ? "exit" : "enter"
        const value: unknown = Reflect.get(target, key, target)
        return typeof value === "function" ? (value as (...a: unknown[]) => unknown).bind(target) : value
      },
    })
  })
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
    fireEvent.change(nameInput(), { target: { value: "circus" } })
    act(() => path().focus())
    fireEvent.change(path(), { target: { value: "/home/eric/src/toolbox" } })
    act(() => nameInput().focus())
    // The name is taken: the submit is refused while the read is under way.
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

describe("new project layout", () => {
  const pathField = (n: number) =>
    screen.getByRole("textbox", { name: new RegExp(`^(Path for location ${n}|位置 ${n} 的路径)$`) })
  const note = (n = 1) => {
    const id = pathField(n).getAttribute("aria-describedby")
    return id ? document.getElementById(id)?.textContent : undefined
  }
  const submit = async () => {
    fireEvent.click(screen.getByRole("button", { name: "Create" }))
    await advance(500)
  }

  it("puts locations first, then the repository URL, then the name, and focuses the first path quietly", async () => {
    await openDialog()
    const order = [path(), repoUrl(), nameInput()]
    for (let i = 1; i < order.length; i++) {
      expect(order[i - 1].compareDocumentPosition(order[i]) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy()
    }
    expect(document.activeElement).toBe(path())
    expect(path().hasAttribute("data-quiet-focus")).toBe(true)
    act(() => nameInput().focus())
    expect(path().hasAttribute("data-quiet-focus")).toBe(false)
  })

  it("has no note under the default directory, with or without a repository URL", async () => {
    await openDialog()
    expect(note()).toBeUndefined()
    fireEvent.change(repoUrl(), { target: { value: "git@github.com:owner/repo.git" } })
    expect(note()).toBeUndefined()
  })

  it("names another project using a row's location", async () => {
    await openDialog()
    fireEvent.change(path(), { target: { value: "/home/eric/src/toolbox/" } })
    expect(note()).toBe("Also used by project toolbox")
    expect(nameInput().value).toBe("toolbox-2")
    fireEvent.change(path(), { target: { value: "/home/eric/src/elsewhere" } })
    expect(note()).toBeUndefined()
  })

  it("marks a row that needs a path invalid", async () => {
    await openDialog()
    // Four devices: the fifth row is on devbox again, whose default directory is taken.
    for (let i = 0; i < 4; i++) {
      fireEvent.click(screen.getByRole("button", { name: "Add location" }))
      await advance(10)
    }
    fireEvent.click(screen.getByRole("button", { name: "Create" }))
    await advance(50)
    expect(pathField(5).getAttribute("aria-invalid")).toBe("true")
    expect(pathField(4).getAttribute("aria-invalid")).not.toBe("true")
  })

  it("fills the name from the repository URL, and saves it", async () => {
    const { state } = await openDialog()
    expect(nameInput().value).toBe("")
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/owner/hello-world.git" } })
    expect(nameInput().value).toBe("hello-world")
    expect(nameInput().placeholder).toBe("")
    await submit()
    expect(state.projects.find((p) => p.name === "hello-world")?.repoUrl).toBe(
      "https://github.com/owner/hello-world.git",
    )
  })

  it("prefers the picked folder's name over the repository's", async () => {
    const { state } = await openDialog()
    fireEvent.change(path(), { target: { value: "/home/eric/src" } })
    await pick("dotfiles")
    expect(repoUrl().value).toBe("git@github.com:eric/dotfiles.git")
    expect(nameInput().value).toBe("dotfiles")
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/owner/other.git" } })
    expect(nameInput().value).toBe("dotfiles")
    await submit()
    expect(state.projects.find((p) => p.name === "dotfiles")?.locations[0].path).toBe("/home/eric/src/dotfiles")
  })

  it("follows the locations and the repository URL until the name is typed in", async () => {
    await openDialog()
    fireEvent.change(path(), { target: { value: "/home/eric/src/alpha" } })
    expect(nameInput().value).toBe("alpha")
    fireEvent.change(path(), { target: { value: "/home/eric/src/beta" } })
    expect(nameInput().value).toBe("beta")
    // Back to the default directory: the repository's name, then none.
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/owner/gamma.git" } })
    fireEvent.change(path(), { target: { value: "" } })
    expect(nameInput().value).toBe("gamma")
    fireEvent.change(repoUrl(), { target: { value: "" } })
    expect(nameInput().value).toBe("")

    fireEvent.change(repoUrl(), { target: { value: "https://github.com/owner/gamma.git" } })
    fireEvent.change(nameInput(), { target: { value: "gamma web" } })
    fireEvent.change(path(), { target: { value: "/home/eric/src/delta" } })
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/owner/other.git" } })
    expect(nameInput().value).toBe("gamma web")
  })

  it("fills the name again once it is cleared and left", async () => {
    await openDialog()
    fireEvent.change(path(), { target: { value: "/home/eric/src/alpha" } })
    fireEvent.change(nameInput(), { target: { value: "Mine" } })
    act(() => nameInput().focus())
    fireEvent.change(nameInput(), { target: { value: "" } })
    // Nothing is filled in under the cursor, whatever else changes.
    expect(nameInput().value).toBe("")
    fireEvent.change(path(), { target: { value: "/home/eric/src/beta" } })
    expect(nameInput().value).toBe("")

    // Leaving it fills it, and it follows the form again.
    act(() => nameInput().blur())
    expect(nameInput().value).toBe("beta")
    expect(nameInput().getAttribute("aria-invalid")).toBeNull()
    fireEvent.change(path(), { target: { value: "/home/eric/src/gamma" } })
    expect(nameInput().value).toBe("gamma")
  })

  it("keeps a cleared name empty under the cursor when a folder's repository URL arrives", async () => {
    await openDialog()
    // A slow device: the read is still under way once the name has focus.
    await typePath("/home/eric/src/toolbox")
    expect(document.activeElement).toBe(nameInput())
    expect(nameInput().value).toBe("toolbox-2")
    fireEvent.change(nameInput(), { target: { value: "" } })
    await advance(SLOW_GIT_REMOTE_MS + 50)
    expect(repoUrl().value).toBe("git@gitlab.com:eric/toolbox.git")
    expect(nameInput().value).toBe("")
    expect(document.activeElement).toBe(nameInput())
    act(() => nameInput().blur())
    expect(nameInput().value).toBe("toolbox-2")
  })

  it("starts a new form when reopened while closing after Create", async () => {
    stubExitAnimation()
    const { state } = await openDialog()
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/owner/hello-world.git" } })
    await submit()
    expect(state.projects.find((p) => p.name === "hello-world")).toBeDefined()
    expect(screen.getByRole("dialog", { hidden: true }).dataset.state).toBe("closed")
    // The page behind is hidden from screen readers while the dialog is up.
    fireEvent.click(screen.getByText("open"))
    await advance(50)
    expect(nameInput().value).toBe("")
    expect(repoUrl().value).toBe("")
    // The project just created is among the others now.
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/owner/hello-world.git" } })
    expect(nameInput().value).toBe("hello-world-2")
  })

  it("saves a typed name over the derived one", async () => {
    const { state } = await openDialog()
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/owner/hello-world.git" } })
    fireEvent.change(nameInput(), { target: { value: "  Mine  " } })
    await submit()
    expect(state.projects.some((p) => p.name === "hello-world")).toBe(false)
    expect(state.projects.find((p) => p.name === "Mine")).toBeDefined()
  })

  it("adds a number to a taken name, and saves it", async () => {
    const { state } = await openDialog()
    // "circus" is already a project.
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/eric/circus.git" } })
    expect(nameInput().value).toBe("circus-2")
    expect(nameInput().getAttribute("aria-invalid")).toBeNull()
    await submit()
    expect(state.projects.find((p) => p.name === "circus-2")?.repoUrl).toBe("https://github.com/eric/circus.git")
  })

  it("skips numbered names that are taken too, ignoring case", async () => {
    const state = seedState()
    const [first] = state.projects
    state.projects.push(
      { ...first, projectId: "hello", name: "Hello", locations: [] },
      { ...first, projectId: "hello-2", name: "HELLO-2", locations: [] },
    )
    await openDialog({ state })
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/owner/hello.git" } })
    expect(nameInput().value).toBe("hello-3")
  })

  it("asks for a name when there is none to derive", async () => {
    const { state } = await openDialog()
    const count = state.projects.length
    await submit()
    expect(state.projects).toHaveLength(count)
    expect(nameInput().getAttribute("aria-invalid")).toBe("true")
    expect(screen.getByText("Enter a project name.")).toBeDefined()
  })

  it("keeps the name, without errors, while the dialog closes after Create", async () => {
    stubExitAnimation()
    // The refetch after Create waits until released.
    let created = false
    let release = () => {}
    const held = new Promise<void>((resolve) => (release = resolve))
    const { state } = await openDialog({
      wrapClient: (client) => ({
        ...client,
        projects: {
          ...client.projects,
          createProject: async (req) => {
            const project = await client.projects.createProject(req)
            created = true
            return project
          },
          listProjects: async () => {
            if (created) await held
            return client.projects.listProjects()
          },
        },
      }),
    })
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/owner/hello-world.git" } })
    fireEvent.change(path(), { target: { value: "/home/eric/src/hello-world" } })
    const input = nameInput()
    await submit()
    expect(state.projects.find((p) => p.name === "hello-world")).toBeDefined()
    const dialog = screen.getByRole("dialog", { hidden: true })
    expect(dialog.dataset.state).toBe("closed")
    const check = () => {
      expect(input.isConnected).toBe(true)
      expect(input.value).toBe("hello-world")
      expect(input.getAttribute("aria-invalid")).toBeNull()
      expect(within(dialog).queryByText(/already exists|Enter a project name/)).toBeNull()
      // Its own location isn't one another project shares.
      expect(path().hasAttribute("aria-describedby")).toBe(false)
    }
    check()
    release()
    await advance(500)
    check()
  })

  it("shows the server's refusal of a taken name", async () => {
    // Another tab took the name after this one listed the projects.
    const { queryClient } = await openDialog({
      wrapClient: (client) => ({
        ...client,
        projects: {
          ...client.projects,
          createProject: async (req) => {
            await client.projects.createProject({ ...req, name: "toolbox-other" })
            return client.projects.createProject(req)
          },
        },
      }),
    })
    fireEvent.change(repoUrl(), { target: { value: "https://github.com/owner/toolbox-other.git" } })
    expect(nameInput().value).toBe("toolbox-other")
    await submit()
    expect(nameInput().getAttribute("aria-invalid")).toBe("true")
    expect(screen.getByText(/toolbox-other.*already/i)).toBeDefined()
    expect(nameInput().value).toBe("toolbox-other")

    // After the refusal the form uses the projects as listed again.
    act(() => void queryClient.invalidateQueries({ queryKey: queryKeys.projects }))
    await advance(50)
    act(() => nameInput().focus())
    fireEvent.change(nameInput(), { target: { value: "" } })
    act(() => nameInput().blur())
    expect(nameInput().value).toBe("toolbox-other-2")
  })

})

