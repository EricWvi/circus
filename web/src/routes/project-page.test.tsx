// @vitest-environment happy-dom
import { cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { addQueueDemo, seedState, type MockState } from "@/api/mock/state"
import type { CircusClient } from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { queryKeys } from "@/api/query-client"
import { forgetAllStartKeys } from "@/state/start-keys"
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
  forgetAllStartKeys()
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

describe("the button while a session is created", () => {
  it("says the session is being created, not that it stops anything", async () => {
    const state = seedState()
    let release!: () => void
    const held = new Promise<void>((resolve) => (release = resolve))
    await renderProject("circus", state, (client) => ({
      ...client,
      sessions: { ...client.sessions, createSession: async (req) => (await held, client.sessions.createSession(req)) },
    }))
    fireEvent.change(composer(), { target: { value: "Hello" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(0)
    const button = screen.getByRole("button", { name: "Creating session…" }) as HTMLButtonElement
    expect(button.disabled).toBe(true)
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull()
    release()
    await advance(1000)
  })
})

describe("starting a session again", () => {
  type CreateReq = Parameters<CircusClient["sessions"]["createSession"]>[0]

  it("reuses the request's key when a failed start is sent again, and creates one session", async () => {
    const state = seedState()
    const before = new Set(state.sessions.map((s) => s.sessionId))
    const creates: CreateReq[] = []
    let loseResponse = true
    const { router } = await renderProject("circus", state, (client) => ({
      ...client,
      sessions: {
        ...client.sessions,
        createSession: async (req) => {
          creates.push(req)
          const session = await client.sessions.createSession(req)
          // The server created the session, but its answer never arrived.
          if (loseResponse) throw new ApiError("unavailable", "connection reset")
          return session
        },
      },
    }))
    fireEvent.change(composer(), { target: { value: "Fix the flaky test" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(1000)
    expect(screen.getByRole("alert").textContent).toBe("Can't reach the server. Try again.")
    expect(router.state.location.pathname).toBe("/p/circus")

    // The key outlives the page: leaving it and coming back still retries.
    await router.navigate("/p/circus/settings")
    await advance(0)
    await router.navigate("/p/circus")
    await advance(100)
    loseResponse = false
    fireEvent.change(composer(), { target: { value: "Fix the flaky test" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(1000)
    expect(creates).toHaveLength(2)
    expect(creates[1].clientRequestId).toBe(creates[0].clientRequestId)
    expect(creates[0].clientRequestId).toMatch(/^[0-9a-f-]{36}$/)
    const created = state.sessions.filter((s) => !before.has(s.sessionId))
    expect(created).toHaveLength(1)
    expect(router.state.location.pathname).toBe(`/p/circus/s/${created[0].sessionId}`)

    // Back on the project page once the session exists, the next first message is a new start.
    await router.navigate("/p/circus")
    await advance(100)
    fireEvent.change(composer(), { target: { value: "Something else" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(1000)
    expect(creates).toHaveLength(3)
    expect(creates[2].clientRequestId).not.toBe(creates[0].clientRequestId)
    expect(state.sessions.filter((s) => !before.has(s.sessionId))).toHaveLength(2)
  })

  it("takes a new key when another location now has the chosen index", async () => {
    const state = withCircus((p) => {
      p.locations = [at("devbox", "/srv/a"), at("devbox", "/srv/b")]
      p.lastUsedLocation = 1
    })
    const creates: CreateReq[] = []
    const { queryClient } = await renderProject("circus", state, (client) => ({
      ...client,
      sessions: {
        ...client.sessions,
        createSession: (req) => {
          creates.push(req)
          return Promise.reject(new ApiError("unavailable", "down"))
        },
      },
    }))
    const send = async () => {
      fireEvent.change(composer(), { target: { value: "Hello" } })
      fireEvent.click(screen.getByRole("button", { name: "Submit" }))
      await advance(100)
    }
    await send()
    // Another tab replaces /srv/b with /srv/c, at the same index.
    state.projects.find((p) => p.projectId === "circus")!.locations[1] = at("devbox", "/srv/c")
    void queryClient.invalidateQueries({ queryKey: queryKeys.projects })
    await advance(100)
    expect(screen.getByText("devbox:/srv/c")).toBeTruthy()
    await send()
    expect(creates.map((c) => c.locationIndex)).toEqual([1, 1])
    expect(creates[1].clientRequestId).not.toBe(creates[0].clientRequestId)
  })

  it("keeps the key when a failed start is sent again with other text, and takes a new one in another location", async () => {
    const state = seedState()
    const creates: CreateReq[] = []
    await renderProject("circus", state, (client) => ({
      ...client,
      sessions: {
        ...client.sessions,
        createSession: (req) => {
          creates.push(req)
          return Promise.reject(new ApiError("unavailable", "down"))
        },
      },
    }))
    const send = async (text: string) => {
      fireEvent.change(composer(), { target: { value: text } })
      fireEvent.click(screen.getByRole("button", { name: "Submit" }))
      await advance(100)
    }
    await send("Hello")
    await send("Hello, with more detail")
    fireEvent.click(screen.getByRole("combobox", { name: "Location" }))
    await advance(0)
    fireEvent.click(screen.getAllByRole("option")[1])
    await advance(0)
    await send("Hello")
    expect(creates.map((c) => c.locationIndex)).toEqual([0, 0, 1])
    expect(creates[1].clientRequestId).toBe(creates[0].clientRequestId)
    expect(creates[2].clientRequestId).not.toBe(creates[0].clientRequestId)
  })

  it("explains a key the server says was used for another request, and starts afresh next time", async () => {
    const creates: CreateReq[] = []
    let conflict = true
    const { router } = await renderProject("circus", seedState(), (client) => ({
      ...client,
      sessions: {
        ...client.sessions,
        createSession: (req) => {
          creates.push(req)
          return conflict ? Promise.reject(new ApiError("already_exists", "taken")) : client.sessions.createSession(req)
        },
      },
    }))
    fireEvent.change(composer(), { target: { value: "Hello" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(100)
    expect(screen.getByRole("alert").textContent).toBe(
      "Couldn't start the session: the request clashed with an earlier one. Send again to start it.",
    )
    conflict = false
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(1000)
    expect(creates[1].clientRequestId).not.toBe(creates[0].clientRequestId)
    expect(router.state.location.pathname).toMatch(/^\/p\/circus\/s\//)
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

describe("the permission mode of a new session", () => {
  type CreateReq = Parameters<CircusClient["sessions"]["createSession"]>[0]
  const picker = () => screen.getByRole("combobox", { name: "Permission mode for the new session" })

  it("starts as auto, offers each mode with what it means, and sends the one chosen", async () => {
    const creates: CreateReq[] = []
    const state = seedState()
    const before = new Set(state.sessions.map((s) => s.sessionId))
    const { router } = await renderProject("circus", state, (client) => ({
      ...client,
      sessions: { ...client.sessions, createSession: (req) => (creates.push(req), client.sessions.createSession(req)) },
    }))
    expect(picker().textContent).toBe("Auto")
    fireEvent.click(picker())
    await advance(0)
    const options = screen.getAllByRole("option")
    // Plan mode is not offered: the session could not leave it.
    expect(options.map((o) => o.querySelector(".font-medium")?.textContent)).toEqual([
      "Auto",
      "Default",
      "Accept edits",
      "Don't ask",
      "Bypass permissions",
    ])
    const acceptEdits = screen.getByRole("option", { name: /^Accept edits/ }).textContent
    expect(acceptEdits).toContain(
      "common file-system commands, including deletions and redirections (such as mkdir, touch, mv, cp, rm and >), run without asking",
    )
    expect(acceptEdits).toContain("other commands come to you for approval")
    fireEvent.click(screen.getByRole("option", { name: /^Accept edits/ }))
    await advance(0)
    expect(picker().textContent).toBe("Accept edits")

    fireEvent.change(composer(), { target: { value: "Sketch the migration" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(1000)
    expect(creates.map((c) => c.permissionMode)).toEqual(["acceptEdits"])
    const created = state.sessions.find((s) => !before.has(s.sessionId))!
    expect(created.permissionMode).toBe("acceptEdits")
    expect(router.state.location.pathname).toBe(`/p/circus/s/${created.sessionId}`)
  })

  it("sends auto when left alone", async () => {
    const creates: CreateReq[] = []
    await renderProject("circus", seedState(), (client) => ({
      ...client,
      sessions: { ...client.sessions, createSession: (req) => (creates.push(req), client.sessions.createSession(req)) },
    }))
    fireEvent.change(composer(), { target: { value: "Hello" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(1000)
    expect(creates.map((c) => c.permissionMode)).toEqual(["auto"])
  })
})

describe("the provider of a new session", () => {
  type CreateReq = Parameters<CircusClient["sessions"]["createSession"]>[0]
  const picker = () => screen.getByRole("combobox", { name: "Provider" })
  const openPicker = async () => {
    fireEvent.click(picker())
    await advance(0)
    return screen.getAllByRole("option").map((o) => o.textContent)
  }
  const start = async (text = "Hello") => {
    fireEvent.change(composer(), { target: { value: text } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(50)
  }
  const newSession = (state: MockState, before: Set<string>) => state.sessions.find((s) => !before.has(s.sessionId))!

  it("starts with the user's default, sending none so the server applies it", async () => {
    const creates: CreateReq[] = []
    const state = seedState()
    const before = new Set(state.sessions.map((s) => s.sessionId))
    await renderProject("circus", state, (client) => ({
      ...client,
      sessions: { ...client.sessions, createSession: (req) => (creates.push(req), client.sessions.createSession(req)) },
    }))
    expect(picker().textContent).toBe("My default (Anthropic)")
    expect(screen.queryByText(/Local login uses the worker machine's own Claude login/)).toBeNull()
    expect(await openPicker()).toEqual(["My default (Anthropic)", "Anthropic", "Legacy key", "Team proxy", "Local login"])
    fireEvent.keyDown(document.activeElement!, { key: "Escape" })
    await advance(0)
    await start()
    expect(creates[0].providerId).toBeUndefined()
    expect(newSession(state, before).providerId).toBe("provider-anthropic")
  })

  it("names the project's default first, and sends local login as an empty provider", async () => {
    const creates: CreateReq[] = []
    const state = withCircus((p) => (p.defaultProviderId = "provider-proxy"))
    const before = new Set(state.sessions.map((s) => s.sessionId))
    await renderProject("circus", state, (client) => ({
      ...client,
      sessions: { ...client.sessions, createSession: (req) => (creates.push(req), client.sessions.createSession(req)) },
    }))
    expect(picker().textContent).toBe("Project default (Team proxy)")
    await openPicker()
    fireEvent.click(screen.getByRole("option", { name: "Local login" }))
    await advance(0)
    expect(picker().textContent).toBe("Local login")
    expect(screen.getByText(/A resumed session may find that login expired/)).toBeTruthy()
    await start()
    expect(creates[0].providerId).toBe("")
    expect(newSession(state, before).providerId).toBe("")
  })

  it("offers local login alone as the default without any default provider, sending it once chosen", async () => {
    const creates: CreateReq[] = []
    const state = seedState()
    for (const p of state.providers) p.isDefault = false
    await renderProject("circus", state, (client) => ({
      ...client,
      sessions: { ...client.sessions, createSession: (req) => (creates.push(req), client.sessions.createSession(req)) },
    }))
    expect(picker().textContent).toBe("Local login")
    expect(screen.getByText(/Local login uses the worker machine's own Claude login/)).toBeTruthy()
    expect(await openPicker()).toEqual(["Anthropic", "Legacy key", "Team proxy", "Local login"])
    // Shown, but not chosen: the request names none.
    fireEvent.keyDown(document.activeElement!, { key: "Escape" })
    await advance(0)
    await start("First")
    expect(creates[0].providerId).toBeUndefined()
  })

  it("sends local login once chosen, even when it is what the default means", async () => {
    const creates: CreateReq[] = []
    const state = seedState()
    for (const p of state.providers) p.isDefault = false
    await renderProject("circus", state, (client) => ({
      ...client,
      sessions: { ...client.sessions, createSession: (req) => (creates.push(req), client.sessions.createSession(req)) },
    }))
    await openPicker()
    fireEvent.click(screen.getByRole("option", { name: "Local login" }))
    await advance(0)
    await start()
    expect(creates[0].providerId).toBe("")
  })

  it("uses a request key per provider, so a retry with another provider is a new start", async () => {
    const creates: CreateReq[] = []
    await renderProject("circus", seedState(), (client) => ({
      ...client,
      sessions: {
        ...client.sessions,
        createSession: (req) => {
          creates.push(req)
          return Promise.reject(new ApiError("unavailable", "down"))
        },
      },
    }))
    await start("Hello")
    await start("Hello")
    await openPicker()
    fireEvent.click(screen.getByRole("option", { name: "Team proxy" }))
    await advance(0)
    await start("Hello")
    expect(creates.map((c) => c.providerId)).toEqual([undefined, undefined, "provider-proxy"])
    expect(creates[1].clientRequestId).toBe(creates[0].clientRequestId)
    expect(creates[2].clientRequestId).not.toBe(creates[0].clientRequestId)
  })

  it("drops a picked provider deleted since, and refreshes the list when the server says so", async () => {
    const creates: CreateReq[] = []
    const state = seedState()
    const { queryClient } = await renderProject("circus", state, (client) => ({
      ...client,
      sessions: { ...client.sessions, createSession: (req) => (creates.push(req), client.sessions.createSession(req)) },
    }))
    await openPicker()
    fireEvent.click(screen.getByRole("option", { name: "Team proxy" }))
    await advance(0)
    // Deleted in another tab; the server refuses it and the list is read again.
    state.providers = state.providers.filter((p) => p.providerId !== "provider-proxy")
    await start("Hello")
    expect(screen.getByRole("alert").textContent).toBe("That provider no longer exists. Choose another.")
    expect(picker().textContent).toBe("My default (Anthropic)")
    expect(queryClient.getQueryData<{ providerId: string }[]>(queryKeys.modelProviders)?.map((p) => p.providerId)).not.toContain(
      "provider-proxy",
    )
    await start("Hello again")
    expect(creates.map((c) => c.providerId)).toEqual(["provider-proxy", undefined])
  })

  it("shows why a session in the list waits", async () => {
    const state = seedState()
    addQueueDemo(state)
    await renderProject("circus", state)
    const dot = screen.getByText("Add a dark mode screenshot test").closest("a")!.querySelector("[title]")!
    expect(dot.getAttribute("title")).toBe(
      "Queued: Team proxy already runs as many sessions as it allows. This one starts when one of them ends.",
    )
  })

  it("shows why a session waits even when its last turn failed", async () => {
    const state = seedState()
    addQueueDemo(state)
    state.sessions.find((s) => s.sessionId === "queue-full")!.lastTurnOutcome = "error"
    await renderProject("circus", state)
    const dot = screen.getByText("Add a dark mode screenshot test").closest("a")!.querySelector("[title]")!
    expect(dot.getAttribute("title")).toMatch(/^Queued: Team proxy already runs as many sessions/)
  })
})

describe("a new session without a worker that runs Claude Code", () => {
  const notice = () => screen.queryByRole("status")
  const notInstalled = (state: MockState, name: string) => {
    const agent = state.workers.find((w) => w.name === name)!.agents[0]
    Object.assign(agent, { state: "not_installed", installedVersion: "", wanted: false, wantedVersion: "" })
  }

  it("names the worker of the location's machine when that one lacks it, links to Devices, and still creates the session", async () => {
    // circus starts on eric-mbp, whose machine has its own worker: the
    // session is pinned to it, whatever the other workers have.
    const state = seedState()
    notInstalled(state, "eric-mbp")
    const { router } = await renderProject("circus", state)
    expect(notice()?.textContent).toBe(
      "The worker on the machine of eric-mbp doesn't have Claude Code installed, so new sessions here wait until it does. Install it on Devices",
    )
    expect(within(notice()!).getByRole("link", { name: "Install it on Devices" }).getAttribute("href")).toBe("/devices")

    const before = new Set(state.sessions.map((s) => s.sessionId))
    fireEvent.change(composer(), { target: { value: "Start anyway" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(500)
    const created = state.sessions.filter((s) => !before.has(s.sessionId))
    expect(created).toHaveLength(1)
    expect(created[0].device).toBe("eric-mbp")
    expect(router.state.location.pathname).toBe(`/p/circus/s/${created[0].sessionId}`)
    expect(created[0].queueReason).toBe("agent_not_installed")
  })

  it("names the location's worker when its name differs from the device's", async () => {
    const state = seedState()
    notInstalled(state, "eric-mbp")
    state.workers.find((w) => w.name === "eric-mbp")!.name = "mbp-runner"
    await renderProject("circus", state)
    expect(notice()?.textContent).toBe(
      "The worker on the machine of eric-mbp (mbp-runner) doesn't have Claude Code installed, so new sessions here wait until it does. Install it on Devices",
    )
  })

  it("says nothing when the location's own worker has it, whatever the others have", async () => {
    const state = seedState()
    for (const name of ["cloud-runner", "devbox", "eric-win"]) notInstalled(state, name)
    await renderProject("circus", state)
    expect(notice()).toBeNull()
  })

  it("looks at every worker for a device without one of its own", async () => {
    // palace-builder has no worker of its own: any may run its sessions.
    const state = seedState()
    state.projects.find((p) => p.projectId === "palace")!.lastUsedLocation = 0
    for (const name of ["devbox", "eric-mbp", "eric-win"]) notInstalled(state, name)
    await renderProject("palace", state)
    expect(notice()).toBeNull()

    cleanup()
    notInstalled(state, "cloud-runner")
    await renderProject("palace", state)
    expect(notice()?.textContent).toBe(
      "None of your workers has Claude Code installed, so new sessions wait until one does. Install it on Devices",
    )
  })

  it("says the user has no worker at all", async () => {
    const state = seedState()
    state.workers = []
    await renderProject("circus", state)
    expect(notice()?.textContent).toBe(
      "You have no workers yet, so new sessions wait until one runs Claude Code. Add one on Devices",
    )
  })

  it("reads in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    const state = seedState()
    notInstalled(state, "eric-mbp")
    await renderProject("circus", state)
    expect(notice()?.textContent).toBe(
      "eric-mbp 所在机器上的 worker 没有安装 Claude Code，这里的新会话会一直等到它安装为止。去设备页安装",
    )
  })
})
