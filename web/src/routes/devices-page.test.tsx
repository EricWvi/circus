// @vitest-environment happy-dom
import { cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { DEVICES_LIVE_INTERVAL_MS } from "@/api/hooks"
import { SEED_OWNER_ID, seedState, type MockState } from "@/api/mock/state"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { advance, renderWithBackend } from "@/test/render"

import { DevicesPage } from "./devices-page"

const NOW = Date.parse("2026-11-17T12:00:00Z")
const DAY = 86_400_000

async function renderDevices(state: MockState) {
  const router = createMemoryRouter(
    [
      {
        path: "/devices",
        element: (
          <SidebarProvider>
            <DevicesPage />
          </SidebarProvider>
        ),
      },
      { path: "/p/:projectId", element: <h1>Project page</h1> },
    ],
    { initialEntries: ["/devices"] },
  )
  const result = await renderWithBackend(<RouterProvider router={router} />, { state })
  await advance(50)
  return { ...result, router }
}

/** The list item of the device with that name (the workers below have names too). */
const row = (name: string) => {
  const devices = within(screen.getAllByRole("region")[0])
  const item = devices.getByText(name, { selector: "span" }).closest("li")
  if (!item) throw new Error(`no row for ${name}`)
  return within(item)
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(NOW)
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  await i18n.changeLanguage("en")
})

describe("the device list", () => {
  function seedWithSpare() {
    const state = seedState(NOW)
    // Enrolled but never connected, with nothing reported.
    state.devices.push({
      name: "spare",
      online: false,
      lastSeenUnixMs: 0,
      os: "",
      arch: "",
      version: "",
      owner: SEED_OWNER_ID,
      deviceId: "device-spare",
    })
    return state
  }

  it("lists devices by name with their status, platform and version", async () => {
    await renderDevices(seedWithSpare())
    expect(screen.getByRole("heading", { level: 1, name: "Devices" })).toBeTruthy()
    const list = screen.getAllByRole("list")[0]
    const names = Array.from(list.children, (li) => li.querySelector("span.font-medium")?.textContent)
    expect(names).toEqual(["devbox", "eric-mbp", "eric-win", "palace-builder", "spare"])

    const devbox = row("devbox")
    expect(devbox.getByRole("img", { name: "Online" })).toBeTruthy()
    expect(devbox.getByText("Online")).toBeTruthy()
    expect(devbox.getByText("linux/amd64")).toBeTruthy()
    expect(devbox.getByText("daemon 0.1.0")).toBeTruthy()
    expect(devbox.queryByText(/Last seen/)).toBeNull()

    const win = row("eric-win")
    expect(win.getByRole("img", { name: "Offline" })).toBeTruthy()
    const seen = win.getByText("Last seen 2d ago")
    expect(seen.tagName).toBe("TIME")
    expect(seen.getAttribute("datetime")).toBe(new Date(NOW - 2 * DAY).toISOString())

    const spare = row("spare")
    expect(spare.getByRole("img", { name: "Offline" })).toBeTruthy()
    expect(spare.getByText("Never connected")).toBeTruthy()
    // Empty platform and version are left out, not shown as blanks.
    expect(spare.queryByText(/daemon/)).toBeNull()
    expect(spare.queryByText("/")).toBeNull()
  })

  it("links the projects with a location on each device", async () => {
    const { router } = await renderDevices(seedWithSpare())
    const projectsOf = (device: string) =>
      screen.queryByRole("list", { name: `Projects on ${device}` })

    const links = (device: string) =>
      within(projectsOf(device)!)
        .getAllByRole("link")
        .map((a) => [a.textContent, a.getAttribute("href")])
    expect(links("eric-mbp")).toEqual([
      ["circus", "/p/circus"],
      ["palace", "/p/palace"],
    ])
    expect(links("devbox")).toEqual([
      ["circus", "/p/circus"],
      ["toolbox", "/p/toolbox"],
    ])
    expect(projectsOf("spare")).toBeNull()

    fireEvent.click(within(projectsOf("eric-win")!).getByRole("link", { name: "toolbox" }))
    await advance(0)
    expect(router.state.location.pathname).toBe("/p/toolbox")
  })

  it("leaves out locations on a revoked device of the same name", async () => {
    const state = seedState(NOW)
    const toolbox = state.projects.find((p) => p.projectId === "toolbox")!
    // The location was saved for an earlier eric-win, revoked since.
    toolbox.locations = toolbox.locations.map((l) =>
      l.device === "eric-win" ? { ...l, deviceId: "device-old-eric-win" } : l,
    )
    await renderDevices(state)
    expect(screen.queryByRole("list", { name: "Projects on eric-win" })).toBeNull()
    expect(screen.getByRole("list", { name: "Projects on devbox" })).toBeTruthy()
  })

  it("refetches the status while the page is open, without showing a loading state", async () => {
    const { state } = await renderDevices(seedState(NOW))
    expect(row("eric-win").getByText("Last seen 2d ago")).toBeTruthy()

    state.devices.find((d) => d.name === "eric-win")!.online = true
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect(row("eric-win").getByText("Online")).toBeTruthy()
    expect(screen.queryByRole("list", { name: "Loading devices" })).toBeNull()
  })

  it("reads in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderDevices(seedWithSpare())
    expect(screen.getByRole("heading", { level: 1, name: "设备" })).toBeTruthy()
    expect(row("devbox").getByText("在线")).toBeTruthy()
    expect(row("devbox").getByText("daemon 0.1.0")).toBeTruthy()
    expect(row("eric-win").getByText("前天在线")).toBeTruthy()
    expect(row("spare").getByText("从未连接")).toBeTruthy()
  })
})

describe("without devices", () => {
  it("says so and offers to add one", async () => {
    const state = seedState(NOW)
    state.devices = []
    await renderDevices(state)

    expect(screen.getByText("No devices yet")).toBeTruthy()
    // The empty state's button is the only one; the header has none.
    expect(screen.getAllByRole("button", { name: "Add device" })).toHaveLength(1)
    // No admin CLI commands any more: adding is the dialog's job.
    expect(document.querySelector("pre")).toBeNull()
    expect(screen.queryByText(/enroll-token/)).toBeNull()
    const empty = screen.getByText("No devices yet").closest("[data-slot=empty]") as HTMLElement
    fireEvent.click(within(empty).getByRole("button", { name: "Add device" }))
    await advance(0)
    expect(screen.getByRole("dialog", { name: "Add device" })).toBeTruthy()
  })
})

describe("when the list can't load", () => {
  it("offers a retry", async () => {
    const { restore } = await renderDevicesFailing(seedState(NOW))
    expect(screen.getByText("Couldn't load devices.")).toBeTruthy()
    restore()
    fireEvent.click(screen.getByRole("button", { name: "Try again" }))
    await advance(50)
    expect(row("devbox").getByText("Online")).toBeTruthy()
  })
})

/** Renders the page while listDevices fails; `restore` makes it work again. */
async function renderDevicesFailing(state: MockState) {
  const router = createMemoryRouter(
    [
      {
        path: "/devices",
        element: (
          <SidebarProvider>
            <DevicesPage />
          </SidebarProvider>
        ),
      },
    ],
    { initialEntries: ["/devices"] },
  )
  let failing = true
  const result = await renderWithBackend(<RouterProvider router={router} />, {
    state,
    wrapClient: (client) => ({
      ...client,
      machines: {
        ...client.machines,
        listDevices: () =>
          failing ? Promise.reject(new Error("boom")) : client.machines.listDevices(),
      },
    }),
  })
  await advance(50)
  return {
    ...result,
    restore: () => {
      failing = false
    },
  }
}

describe("the worker list", () => {
  const workers = () => within(screen.getByRole("region", { name: "Workers" }))
  const workerRow = (name: string) => within(workers().getByText(name, { selector: "span.font-medium" }).closest("li")!)

  it("lists workers with status, version and the devices on the same machine", async () => {
    await renderDevices(seedState(NOW))
    const names = workers()
      .getAllByText(/./, { selector: "span.font-medium" })
      .map((e) => e.textContent)
    expect(names).toEqual(["cloud-runner", "devbox", "eric-mbp", "eric-win"])
    expect(workerRow("devbox").getByText("Online")).toBeTruthy()
    expect(workerRow("devbox").getByText("worker 0.1.0")).toBeTruthy()
    expect(workerRow("devbox").getByText("linux/amd64")).toBeTruthy()
    const same = workerRow("devbox").getByRole("list", { name: "Devices on the same machine as devbox" })
    expect(within(same).getAllByRole("listitem").map((li) => li.textContent)).toEqual(["devbox"])
    expect(workerRow("cloud-runner").queryByRole("list", { name: /^Devices on the same machine/ })).toBeNull()
    expect(workerRow("eric-win").getByText("Last seen 2d ago")).toBeTruthy()
    // Each worker's Claude Code is installed: uninstalling it is all there is
    // to do, besides each row's actions menu.
    const labels = workers().getAllByRole("button").map((b) => b.getAttribute("aria-label"))
    expect(labels.filter((l) => l?.startsWith("Actions for "))).toEqual([
      "Actions for cloud-runner",
      "Actions for devbox",
      "Actions for eric-mbp",
      "Actions for eric-win",
    ])
    expect(labels.filter((l) => !l?.startsWith("Actions for "))).toEqual([
      "Uninstall Claude Code from cloud-runner",
      "Uninstall Claude Code from devbox",
      "Uninstall Claude Code from eric-mbp",
      "Uninstall Claude Code from eric-win",
    ])
  })

  it("says when there are none", async () => {
    const state = seedState(NOW)
    state.workers = []
    await renderDevices(state)
    expect(workers().getByText(/No workers yet/)).toBeTruthy()
  })

  it("reads in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderDevices(seedState(NOW))
    const section = within(screen.getByRole("region", { name: "工作节点" }))
    expect(section.getByRole("list", { name: "与 devbox 在同一台机器上的设备" })).toBeTruthy()
    const toggle = within(section.getByText("devbox", { selector: "span.font-medium" }).closest("li")!).getByRole("switch", {
      name: "合入者",
    })
    expect(document.getElementById(toggle.getAttribute("aria-describedby")!)?.textContent).toBe(
      "可以合入落后于 main 的 Ledger 改动集，并丢弃不是它开的改动集。",
    )
  })
})

/** A promise settled from outside. */
function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (error: unknown) => void
  const promise = new Promise<T>((res, rej) => {
    resolve = res
    reject = rej
  })
  return { promise, resolve, reject }
}

describe("the integrator switch", () => {
  const workers = () => within(screen.getByRole("region", { name: "Workers" }))
  const toggleOf = (name: string) =>
    within(workers().getByText(name, { selector: "span.font-medium" }).closest("li")!).getByRole("switch", {
      name: "Integrator",
    })
  const isOn = (name: string) => toggleOf(name).getAttribute("aria-checked") === "true"

  type Grant = { workerId: string; integrator: boolean }

  /**
   * Renders the page with setWorkerGrant held until the test settles it:
   * `calls` records each request, and `next` the latest pending one.
   */
  async function renderHeld(state: MockState = seedState(NOW)) {
    const calls: Grant[] = []
    let next: { req: Grant; settle: ReturnType<typeof deferred<void>> } | undefined
    const result = await renderWithBackend(<RouterProvider router={devicesRouter()} />, {
      state,
      wrapClient: (client) => ({
        ...client,
        machines: {
          ...client.machines,
          setWorkerGrant: async (req) => {
            calls.push(req)
            const settle = deferred<void>()
            next = { req, settle }
            await settle.promise
            return client.machines.setWorkerGrant(req)
          },
        },
      }),
    })
    await advance(50)
    return {
      ...result,
      calls,
      /** Lets the held request through to the mock server. */
      release: async () => {
        next!.settle.resolve()
        await advance(50)
      },
      fail: async (error: unknown) => {
        next!.settle.reject(error)
        await advance(50)
      },
    }
  }

  function devicesRouter() {
    return createMemoryRouter(
      [
        {
          path: "/devices",
          element: (
            <SidebarProvider>
              <DevicesPage />
            </SidebarProvider>
          ),
        },
      ],
      { initialEntries: ["/devices"] },
    )
  }

  it("shows each worker's grant, explained", async () => {
    await renderDevices(seedState(NOW))
    expect(isOn("cloud-runner")).toBe(true)
    expect(["devbox", "eric-mbp", "eric-win"].map(isOn)).toEqual([false, false, false])
    const hint = document.getElementById(toggleOf("devbox").getAttribute("aria-describedby")!)
    expect(hint?.textContent).toBe(
      "Can merge ledger changesets that fall behind main, and discard changesets it did not open.",
    )
  })

  it("turns the grant on with the worker's id, pending until the server answers", async () => {
    const { calls, release, state } = await renderHeld()
    fireEvent.click(toggleOf("devbox"))
    await advance(0)
    expect(calls).toEqual([{ workerId: "worker-devbox", integrator: true }])
    // The requested state, marked busy, while the request is out.
    expect(isOn("devbox")).toBe(true)
    expect(toggleOf("devbox").getAttribute("aria-busy")).toBe("true")
    expect(workers().getByRole("status", { name: "Saving" })).toBeTruthy()
    // Pressed again meanwhile: ignored.
    fireEvent.click(toggleOf("devbox"))
    await advance(0)
    expect(calls).toHaveLength(1)
    expect(isOn("devbox")).toBe(true)

    await release()
    expect(isOn("devbox")).toBe(true)
    expect(toggleOf("devbox").getAttribute("aria-busy")).toBe("false")
    expect(workers().queryByRole("status")).toBeNull()
    expect(state.workers.find((w) => w.workerId === "worker-devbox")?.integrator).toBe(true)
  })

  it("turns the grant off", async () => {
    const { calls, release, state } = await renderHeld()
    fireEvent.click(toggleOf("cloud-runner"))
    await advance(0)
    expect(calls).toEqual([{ workerId: "worker-cloud-runner", integrator: false }])
    expect(isOn("cloud-runner")).toBe(false)
    await release()
    expect(isOn("cloud-runner")).toBe(false)
    expect(state.workers.find((w) => w.workerId === "worker-cloud-runner")?.integrator).toBe(false)
  })

  it("shows the server's worker, not the request", async () => {
    // A server that keeps the grant off whatever is asked.
    await renderWithBackend(<RouterProvider router={devicesRouter()} />, {
      state: seedState(NOW),
      wrapClient: (client) => ({
        ...client,
        machines: {
          ...client.machines,
          setWorkerGrant: (req) => client.machines.setWorkerGrant({ ...req, integrator: false }),
        },
      }),
    })
    await advance(50)
    fireEvent.click(toggleOf("devbox"))
    await advance(50)
    expect(isOn("devbox")).toBe(false)
    expect(workers().queryByRole("alert")).toBeNull()
  })

  it("goes back and says so when the change fails, until the next try", async () => {
    const { fail, release } = await renderHeld()
    fireEvent.click(toggleOf("devbox"))
    await advance(0)
    await fail(new ApiError("unavailable", "down"))
    expect(isOn("devbox")).toBe(false)
    expect(toggleOf("devbox").getAttribute("aria-busy")).toBe("false")
    expect(workers().getByRole("alert").textContent).toBe("Couldn't change Integrator. Try again.")

    fireEvent.click(toggleOf("devbox"))
    await advance(0)
    expect(workers().queryByRole("alert")).toBeNull()
    await release()
    expect(isOn("devbox")).toBe(true)
  })

  it("says so in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    const { fail } = await renderHeld()
    const section = within(screen.getByRole("region", { name: "工作节点" }))
    fireEvent.click(within(section.getByText("devbox", { selector: "span.font-medium" }).closest("li")!).getByRole("switch"))
    await advance(0)
    await fail(new ApiError("unavailable", "down"))
    expect(section.getByRole("alert").textContent).toBe("无法更改合入者设置，请重试。")
  })

  it("drops a worker that is gone, with a notice that outlives its row", async () => {
    const { state, release } = await renderHeld()
    fireEvent.click(toggleOf("devbox"))
    await advance(0)
    state.workers = state.workers.filter((w) => w.workerId !== "worker-devbox")
    await release()
    expect(workers().queryByText("devbox", { selector: "span.font-medium" })).toBeNull()
    expect(isOn("cloud-runner")).toBe(true)
    expect(workers().queryByRole("alert")).toBeNull()
    expect(screen.getByText("Worker devbox no longer exists")).toBeTruthy()
    expect(screen.getByText("Its Integrator setting wasn't changed.")).toBeTruthy()
  })

  it("keeps a change in flight when the page is left and opened again", async () => {
    // The server applies each change at once; only the first answer is held.
    const held = deferred<void>()
    const calls: Grant[] = []
    const router = createMemoryRouter(
      [
        {
          path: "/devices",
          element: (
            <SidebarProvider>
              <DevicesPage />
            </SidebarProvider>
          ),
        },
        { path: "/elsewhere", element: <h1>Elsewhere</h1> },
      ],
      { initialEntries: ["/devices"] },
    )
    const { state } = await renderWithBackend(<RouterProvider router={router} />, {
      state: seedState(NOW),
      wrapClient: (client) => ({
        ...client,
        machines: {
          ...client.machines,
          setWorkerGrant: async (req) => {
            calls.push(req)
            const worker = await client.machines.setWorkerGrant(req)
            if (calls.length === 1) await held.promise
            return worker
          },
        },
      }),
    })
    await advance(50)
    fireEvent.click(toggleOf("devbox"))
    await advance(0)
    await router.navigate("/elsewhere")
    await advance(10)
    await router.navigate("/devices")
    await advance(50)
    // Still the requested state, busy, though the cached list says off.
    expect(isOn("devbox")).toBe(true)
    expect(toggleOf("devbox").getAttribute("aria-busy")).toBe("true")
    expect(workers().getByRole("status", { name: "Saving" })).toBeTruthy()
    fireEvent.click(toggleOf("devbox"))
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect(calls).toHaveLength(1)
    expect(isOn("devbox")).toBe(true)

    held.resolve()
    await advance(50)
    expect(isOn("devbox")).toBe(true)
    expect(toggleOf("devbox").getAttribute("aria-busy")).toBe("false")
    fireEvent.click(toggleOf("devbox"))
    await advance(50)
    expect(calls).toEqual([
      { workerId: "worker-devbox", integrator: true },
      { workerId: "worker-devbox", integrator: false },
    ])
    expect(isOn("devbox")).toBe(false)
    expect(state.workers.find((w) => w.workerId === "worker-devbox")?.integrator).toBe(false)
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect(isOn("devbox")).toBe(false)
  })

  it("drops the failure once a list shows the change applied after all", async () => {
    // Applied, but the answer is lost on the way back.
    await renderWithBackend(<RouterProvider router={devicesRouter()} />, {
      state: seedState(NOW),
      wrapClient: (client) => ({
        ...client,
        machines: {
          ...client.machines,
          setWorkerGrant: async (req) => {
            await client.machines.setWorkerGrant(req)
            throw new ApiError("unavailable", "connection reset")
          },
        },
      }),
    })
    await advance(50)
    fireEvent.click(toggleOf("devbox"))
    await advance(50)
    expect(isOn("devbox")).toBe(false)
    expect(workers().getByRole("alert").textContent).toBe("Couldn't change Integrator. Try again.")
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect(isOn("devbox")).toBe(true)
    expect(workers().queryByRole("alert")).toBeNull()
  })

  it("shows the answer at once, without waiting for a list", async () => {
    let slowLists = false
    await renderWithBackend(<RouterProvider router={devicesRouter()} />, {
      state: seedState(NOW),
      wrapClient: (client) => ({
        ...client,
        machines: {
          ...client.machines,
          listWorkers: async () => {
            const list = await client.machines.listWorkers()
            if (slowLists) await new Promise((resolve) => setTimeout(resolve, 2000))
            return list
          },
        },
      }),
    })
    await advance(50)
    slowLists = true
    fireEvent.click(toggleOf("devbox"))
    await advance(50)
    expect(isOn("devbox")).toBe(true)
    expect(toggleOf("devbox").getAttribute("aria-busy")).toBe("false")
  })

  it("keeps the requested state when a list sees the change before its answer", async () => {
    const held = deferred<void>()
    await renderWithBackend(<RouterProvider router={devicesRouter()} />, {
      state: seedState(NOW),
      wrapClient: (client) => ({
        ...client,
        machines: {
          ...client.machines,
          setWorkerGrant: async (req) => {
            const worker = await client.machines.setWorkerGrant(req)
            await held.promise
            return worker
          },
        },
      }),
    })
    await advance(50)
    fireEvent.click(toggleOf("devbox"))
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect(isOn("devbox")).toBe(true)
    expect(toggleOf("devbox").getAttribute("aria-busy")).toBe("true")
    held.resolve()
    await advance(50)
    expect(isOn("devbox")).toBe(true)
  })

  it("keeps the requested state through list refreshes while the change is out", async () => {
    const { release } = await renderHeld()
    fireEvent.click(toggleOf("devbox"))
    await advance(0)
    // The server hasn't applied it: every refresh still says off.
    for (let i = 0; i < 3; i++) {
      await advance(DEVICES_LIVE_INTERVAL_MS)
      expect(isOn("devbox")).toBe(true)
    }
    await release()
    expect(isOn("devbox")).toBe(true)
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect(isOn("devbox")).toBe(true)
  })

  it("ignores a list fetched before the change that arrives after it", async () => {
    // List requests read the server at once and answer when let go.
    let holdLists = false
    const heldLists: (() => void)[] = []
    const grant = deferred<void>()
    await renderWithBackend(<RouterProvider router={devicesRouter()} />, {
      state: seedState(NOW),
      wrapClient: (client) => ({
        ...client,
        machines: {
          ...client.machines,
          listWorkers: async () => {
            const list = await client.machines.listWorkers()
            if (holdLists) await new Promise<void>((resolve) => heldLists.push(resolve))
            return list
          },
          setWorkerGrant: async (req) => {
            await grant.promise
            return client.machines.setWorkerGrant(req)
          },
        },
      }),
    })
    await advance(50)
    fireEvent.click(toggleOf("devbox"))
    await advance(0)
    holdLists = true
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect(heldLists.length).toBeGreaterThan(0)
    // The change lands, then the older list (grant off) arrives.
    holdLists = false
    grant.resolve()
    await advance(50)
    for (const resolve of heldLists) resolve()
    await advance(50)
    expect(isOn("devbox")).toBe(true)
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect(isOn("devbox")).toBe(true)
  })
})

describe("deleting", () => {
  const openDelete = async (kind: "devices" | "workers", name: string) => {
    const list = document.querySelector<HTMLElement>(`section[aria-labelledby="${kind}-heading"]`)!
    fireEvent.pointerDown(within(list).getByRole("button", { name: `Actions for ${name}` }), { button: 0, ctrlKey: false })
    await advance(0)
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete…" }))
    await advance(0)
  }

  it("confirms with what deleting a device does, then deletes it", async () => {
    const { state } = await renderDevices(seedState(NOW))
    await openDelete("devices", "eric-win")
    const dialog = within(screen.getByRole("dialog", { name: "Delete device eric-win?" }))
    expect(dialog.getAllByRole("listitem").map((li) => li.textContent)).toEqual([
      "Its credential is revoked, and its connection, if any, is closed.",
      "The name eric-win is free for another device or worker to join under.",
      "Project locations on it show the device as removed.",
      "A worker on the same machine is not affected.",
      "If it runs as an installed service, it removes that service and its credential file when it learns of this.",
    ])
    // Offline: no warning.
    expect(dialog.queryByText(/is online/)).toBeNull()
    fireEvent.click(dialog.getByRole("button", { name: "Delete" }))
    await advance(50)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(state.devices.some((d) => d.name === "eric-win")).toBe(false)
    expect(within(screen.getAllByRole("list")[0]).queryByText("eric-win", { selector: "span" })).toBeNull()
    // The worker of the same name stays.
    expect(state.workers.some((w) => w.name === "eric-win")).toBe(true)
  })

  it("warns when it is online, and says how deleting a worker differs", async () => {
    const { state } = await renderDevices(seedState(NOW))
    await openDelete("workers", "devbox")
    const dialog = within(screen.getByRole("dialog", { name: "Delete worker devbox?" }))
    expect(dialog.getByText("devbox is online: it is disconnected at once, and sessions running on it fail.")).toBeTruthy()
    expect(dialog.getByText("It leaves every model provider it is assigned to, and loses its Integrator grant.")).toBeTruthy()
    expect(dialog.getByText("A device on the same machine is not affected.")).toBeTruthy()
    // Cancel keeps it.
    fireEvent.click(dialog.getByRole("button", { name: "Cancel" }))
    await advance(10)
    expect(state.workers.some((w) => w.name === "devbox")).toBe(true)
  })

  it("takes a device deleted meanwhile as deleted, and deletes once however often confirmed", async () => {
    const { state, client } = await renderDevices(seedState(NOW))
    let calls = 0
    const deleteDevice = client.machines.deleteDevice.bind(client.machines)
    client.machines.deleteDevice = (req) => {
      calls++
      return deleteDevice(req)
    }
    await openDelete("devices", "eric-win")
    state.devices = state.devices.filter((d) => d.name !== "eric-win")
    const confirm = screen.getByRole("button", { name: "Delete" })
    // Enter, Space and a click while the first is under way.
    fireEvent.click(confirm)
    fireEvent.click(confirm)
    fireEvent.keyDown(confirm, { key: "Enter" })
    await advance(50)
    expect(calls).toBe(1)
    // not_found: it is gone, as asked; the dialog closes.
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(within(screen.getAllByRole("list")[0]).queryByText("eric-win", { selector: "span" })).toBeNull()
  })

  it("returns focus to the row's menu after Cancel, and to Add device once the row is gone", async () => {
    await renderDevices(seedState(NOW))
    const list = () => document.querySelector<HTMLElement>('section[aria-labelledby="devices-heading"]')!
    const trigger = () => within(list()).getByRole("button", { name: "Actions for eric-win" })
    const openMenu = async () => {
      fireEvent.pointerDown(trigger(), { button: 0, ctrlKey: false })
      await advance(0)
    }
    await openMenu()
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete…" }))
    await advance(0)
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }))
    await advance(300)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(document.activeElement).toBe(trigger())

    await openMenu()
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete…" }))
    await advance(0)
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    await advance(300)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Add device" }))
  })

  it("reads in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderDevices(seedState(NOW))
    const list = document.querySelector<HTMLElement>('section[aria-labelledby="devices-heading"]')!
    fireEvent.pointerDown(within(list).getByRole("button", { name: "devbox 的操作" }), { button: 0, ctrlKey: false })
    await advance(0)
    fireEvent.click(screen.getByRole("menuitem", { name: "删除…" }))
    await advance(0)
    const dialog = within(screen.getByRole("dialog", { name: "删除设备 devbox？" }))
    expect(dialog.getByText("devbox 当前在线：它会立即断开连接，在它上面运行的会话会失败。")).toBeTruthy()
  })
})
