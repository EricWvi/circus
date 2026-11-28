// @vitest-environment happy-dom
import { cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { DEVICES_LIVE_INTERVAL_MS } from "@/api/hooks"
import { SEED_OWNER_ID, seedMachine, seedState, type MockState } from "@/api/mock/state"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { addMenuTrigger, chooseAdd } from "@/test/add-menu"
import {
  deviceRow,
  machineCard,
  machineCards,
  machineNames,
  machinesSection,
  roleLabels,
  rolesOf,
  shownNames,
  workerRow,
} from "@/test/machines"
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

/** The row of the device with that name. */
const row = (name: string) => within(deviceRow(name))

/** Closes the open dialog with Escape, through its exit animation. */
async function closeDialog() {
  fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" })
  await advance(300)
  expect(screen.queryByRole("dialog")).toBeNull()
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

describe("the machine list", () => {
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
      hostname: "",
      machineKey: "machine-spare",
    })
    return state
  }

  it("shows one card per machine, by name ignoring case, with a row for each of its roles", async () => {
    await renderDevices(seedWithSpare())
    expect(screen.getByRole("heading", { level: 1, name: "Devices" })).toBeTruthy()
    expect(screen.getByRole("heading", { level: 2, name: "Machines" })).toBeTruthy()
    expect(screen.queryByRole("heading", { name: "Workers" })).toBeNull()
    // Named by the hostname the device reported (ERIC-WIN, erics-mbp.local),
    // else the worker's (cloud-runner), else the device's name
    // (palace-builder, spare). ERIC-WIN sorts among the others, not first.
    expect(machineNames()).toEqual(["cloud-runner", "devbox", "ERIC-WIN", "erics-mbp.local", "palace-builder", "spare"])
    expect(rolesOf(machineCard("devbox"))).toEqual(["Device devbox", "Worker devbox"])
    expect(rolesOf(machineCard("erics-mbp.local"))).toEqual(["Device eric-mbp", "Worker eric-mbp"])
    // A worker on a machine of its own, and devices without a worker.
    expect(rolesOf(machineCard("cloud-runner"))).toEqual(["Worker cloud-runner"])
    expect(rolesOf(machineCard("palace-builder"))).toEqual(["Device palace-builder"])
    expect(rolesOf(machineCard("spare"))).toEqual(["Device spare"])
    // The rows say their role, and the old "same machine" line is gone.
    expect(roleLabels(machineCard("devbox"))).toEqual(["Device", "Worker"])
    expect(screen.queryByText("Same machine as")).toBeNull()
  })

  it("leaves out a role's name where it is the machine's, ignoring case, and shows it where it differs", async () => {
    const state = seedState(NOW)
    // A device of another name on eric-mbp's machine (erics-mbp.local).
    state.devices.find((d) => d.name === "eric-mbp")!.name = "laptop"
    await renderDevices(state)
    // The same name: devbox, palace-builder (named after its device), cloud-runner.
    expect(shownNames(machineCard("devbox"))).toEqual([null, null])
    expect(shownNames(machineCard("palace-builder"))).toEqual([null])
    expect(shownNames(machineCard("cloud-runner"))).toEqual([null])
    // The same ignoring case: ERIC-WIN and eric-win.
    expect(shownNames(machineCard("ERIC-WIN"))).toEqual([null, null])
    // Different names are shown.
    expect(shownNames(machineCard("erics-mbp.local"))).toEqual(["laptop", "eric-mbp"])

    // The rows and their menus still name their roles, shown or not.
    expect(rolesOf(machineCard("ERIC-WIN"))).toEqual(["Device eric-win", "Worker eric-win"])
    expect(rolesOf(machineCard("erics-mbp.local"))).toEqual(["Device laptop", "Worker eric-mbp"])
    for (const [r, menu] of [
      [deviceRow("eric-win"), "Actions for device eric-win"],
      [workerRow("eric-win"), "Actions for worker eric-win"],
      [deviceRow("devbox"), "Actions for device devbox"],
      [deviceRow("laptop"), "Actions for device laptop"],
    ] as const) {
      expect(within(r).getByRole("button", { name: menu })).toBeTruthy()
    }
    // The row reads: role, version, link state.
    const win = deviceRow("eric-win")
    expect(within(win).queryByText("eric-win")).toBeNull()
    expect(within(win).getByText("v0.1.0")).toBeTruthy()
    expect(within(win).getByText("Last seen 2d ago")).toBeTruthy()
  })

  it("lines up the role labels of a card with both kinds of rows, as wide as the longer label", async () => {
    await renderDevices(seedState(NOW))
    // Each label's cell holds both labels, the other one hidden from view
    // and from assistive technology, so both cells take the longer one's width.
    for (const r of [deviceRow("devbox"), workerRow("devbox")]) {
      const cell = r.querySelector("[data-role-label]")!.parentElement!
      expect([...cell.children].map((c) => c.textContent).sort()).toEqual(["Device", "Worker"])
      const hidden = [...cell.children].find((c) => !c.hasAttribute("data-role-label"))!
      expect(hidden.getAttribute("aria-hidden")).toBe("true")
      expect(hidden.className).toContain("invisible")
      expect(cell.className).toContain("grid")
    }
    // A card with one kind of row has nothing to line up with: no padding.
    const solo = deviceRow("palace-builder").querySelector("[data-role-label]")!.parentElement!
    expect([...solo.children].map((c) => c.textContent)).toEqual(["Device"])
  })

  it("puts the platform on the machine, and each role's status and version on its row", async () => {
    await renderDevices(seedWithSpare())
    const devbox = within(machineCard("devbox"))
    expect(devbox.getAllByText("linux/amd64")).toHaveLength(1)

    const device = row("devbox")
    expect(device.getByRole("img", { name: "Online" })).toBeTruthy()
    expect(device.getByText("Online")).toBeTruthy()
    // The version alone: the role label says whose it is.
    expect(device.getByText("v0.1.0")).toBeTruthy()
    expect(device.queryByText(/daemon/)).toBeNull()
    expect(device.queryByText(/Last seen/)).toBeNull()
    const worker = within(workerRow("devbox"))
    expect(worker.getByText("Online")).toBeTruthy()
    expect(worker.getByText("v0.1.0")).toBeTruthy()
    expect(worker.queryByText(/^worker/)).toBeNull()

    // An offline machine: both roles say when they were last seen.
    for (const r of [deviceRow("eric-win"), workerRow("eric-win")]) {
      expect(within(r).getByRole("img", { name: "Offline" })).toBeTruthy()
      const seen = within(r).getByText("Last seen 2d ago")
      expect(seen.tagName).toBe("TIME")
      expect(seen.getAttribute("datetime")).toBe(new Date(NOW - 2 * DAY).toISOString())
    }

    const spare = row("spare")
    expect(spare.getByRole("img", { name: "Offline" })).toBeTruthy()
    expect(spare.getByText("Never connected")).toBeTruthy()
    // Empty platform and version are left out, not shown as blanks.
    expect(spare.queryByText(/^v/)).toBeNull()
    expect(within(machineCard("spare")).queryByText("/")).toBeNull()
  })

  it("gives each role its own menu, named by its role", async () => {
    await renderDevices(seedState(NOW))
    const labels = within(machineCard("devbox"))
      .getAllByRole("button")
      .map((b) => b.getAttribute("aria-label"))
      .filter((l) => l?.startsWith("Actions for "))
    expect(labels).toEqual(["Actions for device devbox", "Actions for worker devbox"])
    expect(within(workerRow("devbox")).getByRole("switch", { name: "Integrator" })).toBeTruthy()
    expect(within(workerRow("devbox")).getByRole("list", { name: "Agents on devbox" })).toBeTruthy()
    expect(within(deviceRow("devbox")).queryByRole("switch")).toBeNull()
  })

  it("groups by machine key, not by name", async () => {
    const state = seedState(NOW)
    // A worker named like a device on another machine, and a device and a
    // worker of different names on one machine without a hostname.
    state.workers.find((w) => w.name === "cloud-runner")!.hostname = ""
    state.devices.push({
      ...state.devices.find((d) => d.name === "palace-builder")!,
      name: "cloud-runner",
      deviceId: "device-cloud-runner",
      machineKey: "machine-elsewhere",
      hostname: "",
    })
    state.devices.find((d) => d.name === "palace-builder")!.machineKey = seedMachine("cloud-runner")
    await renderDevices(state)
    // Named by the device's name: cloud-runner's machine is now palace-builder.
    expect(machineNames()).toEqual(["cloud-runner", "devbox", "ERIC-WIN", "erics-mbp.local", "palace-builder"])
    expect(rolesOf(machineCard("palace-builder"))).toEqual(["Device palace-builder", "Worker cloud-runner"])
    expect(rolesOf(machineCard("cloud-runner"))).toEqual(["Device cloud-runner"])
  })

  it("names a machine after its online role, not an old identity left on it", async () => {
    const state = seedState(NOW)
    // Enrolled again instead of rejoined: the old device stays, offline, with the old hostname.
    state.devices.push({
      ...state.devices.find((d) => d.name === "devbox")!,
      name: "aaa-old",
      deviceId: "device-aaa-old",
      online: false,
      lastSeenUnixMs: NOW - 30 * DAY,
      hostname: "oldname",
      os: "windows",
    })
    await renderDevices(state)
    expect(machineNames()).toContain("devbox")
    expect(machineNames()).not.toContain("oldname")
    expect(rolesOf(machineCard("devbox"))).toEqual(["Device aaa-old", "Device devbox", "Worker devbox"])
    expect(within(machineCard("devbox")).getByText("linux/amd64")).toBeTruthy()
    expect(shownNames(machineCard("devbox"))).toEqual(["aaa-old", null, null])
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
    expect(deviceRow("devbox").contains(projectsOf("devbox"))).toBe(true)
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
    state.workers.find((w) => w.name === "cloud-runner")!.online = false
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect(row("eric-win").getByText("Online")).toBeTruthy()
    expect(within(workerRow("eric-win")).getByText("Last seen 2d ago")).toBeTruthy()
    expect(within(workerRow("cloud-runner")).getByText("Last seen just now")).toBeTruthy()
    expect(screen.queryByRole("list", { name: "Loading machines" })).toBeNull()
  })

  it("reads in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderDevices(seedWithSpare())
    expect(screen.getByRole("heading", { level: 1, name: "设备" })).toBeTruthy()
    expect(screen.getByRole("heading", { level: 2, name: "机器" })).toBeTruthy()
    expect(roleLabels(machineCard("devbox"))).toEqual(["设备", "工作节点"])
    expect(shownNames(machineCard("erics-mbp.local"))).toEqual(["eric-mbp", "eric-mbp"])
    expect(shownNames(machineCard("devbox"))).toEqual([null, null])
    expect(rolesOf(machineCard("devbox"))).toEqual(["设备 devbox", "工作节点 devbox"])
    expect(row("devbox").getByText("在线")).toBeTruthy()
    expect(row("devbox").getByText("v0.1.0")).toBeTruthy()
    expect(within(workerRow("devbox")).getByText("v0.1.0")).toBeTruthy()
    expect(within(machineCard("devbox")).getByRole("button", { name: "设备 devbox 的操作" })).toBeTruthy()
    expect(within(machineCard("devbox")).getByRole("button", { name: "工作节点 devbox 的操作" })).toBeTruthy()
    expect(row("eric-win").getByText("前天在线")).toBeTruthy()
    expect(row("spare").getByText("从未连接")).toBeTruthy()
    const toggle = within(workerRow("devbox")).getByRole("switch", { name: "合入者" })
    expect(document.getElementById(toggle.getAttribute("aria-describedby")!)?.textContent).toBe(
      "可以合入落后于 main 的 Ledger 改动集，并丢弃不是它开的改动集。",
    )
  })
})

describe("the header's Add menu", () => {
  const items = () => screen.getAllByRole("menuitem").map((item) => item.textContent)

  it("adds a device, a worker or a model provider, returning focus to the menu", async () => {
    await renderDevices(seedState(NOW))
    const trigger = addMenuTrigger()
    expect(trigger.textContent).toBe("Add")
    expect(trigger.getAttribute("aria-haspopup")).toBe("menu")
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false })
    await advance(0)
    expect(items()).toEqual(["Add device", "Add worker", "Add model provider"])
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" })
    await advance(0)

    await chooseAdd("Add device")
    let dialog = within(screen.getByRole("dialog", { name: "Add device" }))
    expect(dialog.getByRole("radio", { name: /^Device/ })).toHaveProperty("checked", true)
    await closeDialog()
    expect(document.activeElement).toBe(trigger)

    await chooseAdd("Add worker")
    dialog = within(screen.getByRole("dialog", { name: "Add worker" }))
    expect(dialog.getByRole("radio", { name: /^Worker/ })).toHaveProperty("checked", true)
    await closeDialog()
    expect(document.activeElement).toBe(trigger)

    await chooseAdd("Add model provider")
    dialog = within(screen.getByRole("dialog", { name: "Add provider" }))
    expect(dialog.getByLabelText("Name")).toBeTruthy()
    await closeDialog()
    expect(document.activeElement).toBe(trigger)
  })

  it("works from the keyboard", async () => {
    await renderDevices(seedState(NOW))
    const trigger = addMenuTrigger()
    trigger.focus()
    for (const key of ["Enter", " ", "ArrowDown"]) {
      fireEvent.keyDown(trigger, { key })
      await advance(0)
      expect(screen.getByRole("menu")).toBeTruthy()
      expect(document.activeElement?.textContent).toBe("Add device")
      fireEvent.keyDown(document.activeElement!, { key: "Escape" })
      await advance(0)
      expect(screen.queryByRole("menu")).toBeNull()
      expect(document.activeElement).toBe(trigger)
    }

    fireEvent.keyDown(trigger, { key: "Enter" })
    await advance(0)
    fireEvent.keyDown(document.activeElement!, { key: "ArrowDown" })
    await advance(0)
    expect(document.activeElement?.textContent).toBe("Add worker")
    fireEvent.keyDown(document.activeElement!, { key: "ArrowDown" })
    await advance(0)
    expect(document.activeElement?.textContent).toBe("Add model provider")
    fireEvent.keyDown(document.activeElement!, { key: "ArrowUp" })
    await advance(0)
    expect(document.activeElement?.textContent).toBe("Add worker")
    fireEvent.keyDown(document.activeElement!, { key: "Enter" })
    await advance(50)
    expect(within(screen.getByRole("dialog", { name: "Add worker" })).getByRole("radio", { name: /^Worker/ })).toHaveProperty(
      "checked",
      true,
    )
    await closeDialog()
    expect(document.activeElement).toBe(trigger)
  })

  it("reads in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderDevices(seedState(NOW))
    const trigger = addMenuTrigger("添加")
    expect(trigger.textContent).toBe("添加")
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false })
    await advance(0)
    expect(items()).toEqual(["添加设备", "添加工作节点", "添加模型提供方"])
    fireEvent.keyDown(screen.getByRole("menu"), { key: "Escape" })
    await advance(0)

    await chooseAdd("添加设备", "添加")
    expect(within(screen.getByRole("dialog", { name: "添加设备" })).getByRole("radio", { name: /^设备/ })).toHaveProperty("checked", true)
    await closeDialog()
    await chooseAdd("添加工作节点", "添加")
    expect(
      within(screen.getByRole("dialog", { name: "添加工作节点" })).getByRole("radio", { name: /^工作节点/ }),
    ).toHaveProperty("checked", true)
    await closeDialog()
    await chooseAdd("添加模型提供方", "添加")
    expect(screen.getByRole("dialog", { name: "添加提供方" })).toBeTruthy()
    await closeDialog()
    expect(document.activeElement).toBe(trigger)
  })
})

describe("without machines", () => {
  it("says so and offers to add a device or a worker", async () => {
    const state = seedState(NOW)
    state.devices = []
    state.workers = []
    await renderDevices(state)

    expect(screen.getByText("No machines yet")).toBeTruthy()
    // One empty state, besides the header's menu.
    expect(document.querySelectorAll("[data-slot=empty]")).toHaveLength(1)
    expect(addMenuTrigger()).toBeTruthy()
    // No admin CLI commands any more: adding is the dialog's job.
    expect(document.querySelector("pre")).toBeNull()
    expect(screen.queryByText(/enroll-token/)).toBeNull()
    const empty = within(screen.getByText("No machines yet").closest<HTMLElement>("[data-slot=empty]")!)
    expect(empty.getAllByRole("button").map((b) => b.textContent)).toEqual(["Add device", "Add worker"])

    const addDevice = empty.getByRole("button", { name: "Add device" })
    fireEvent.click(addDevice)
    await advance(0)
    let dialog = within(screen.getByRole("dialog", { name: "Add device" }))
    expect(dialog.getByRole("radio", { name: /^Device/ })).toHaveProperty("checked", true)
    await closeDialog()
    expect(document.activeElement).toBe(addDevice)

    const addWorker = empty.getByRole("button", { name: "Add worker" })
    fireEvent.click(addWorker)
    await advance(0)
    dialog = within(screen.getByRole("dialog", { name: "Add worker" }))
    expect(dialog.getByRole("radio", { name: /^Worker/ })).toHaveProperty("checked", true)
    // Only chosen: the user can still pick another role. The title stays
    // the one of the button pressed.
    fireEvent.click(dialog.getByRole("radio", { name: /^Device/ }))
    expect(dialog.getByRole("radio", { name: /^Device/ })).toHaveProperty("checked", true)
    expect(screen.getByRole("dialog", { name: "Add worker" })).toBeTruthy()
    await closeDialog()
    expect(document.activeElement).toBe(addWorker)
  })

  it("lists the machines of the one role there is", async () => {
    const state = seedState(NOW)
    state.devices = []
    await renderDevices(state)
    expect(screen.queryByText("No machines yet")).toBeNull()
    expect(machineNames()).toEqual(["cloud-runner", "devbox", "ERIC-WIN", "erics-mbp.local"])
    expect(machineCards().map(rolesOf)).toEqual([
      ["Worker cloud-runner"],
      ["Worker devbox"],
      ["Worker eric-win"],
      ["Worker eric-mbp"],
    ])
  })

  it("reads in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    const state = seedState(NOW)
    state.devices = []
    state.workers = []
    await renderDevices(state)
    expect(screen.getByText("还没有机器")).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "添加设备" }))
    await advance(0)
    expect(within(screen.getByRole("dialog")).getByRole("radio", { name: /^设备/ })).toHaveProperty("checked", true)
    await closeDialog()
    fireEvent.click(screen.getByRole("button", { name: "添加工作节点" }))
    await advance(0)
    expect(within(screen.getByRole("dialog", { name: "添加工作节点" })).getByRole("radio", { name: /^工作节点/ })).toHaveProperty(
      "checked",
      true,
    )
  })
})

describe("when a list can't load", () => {
  it.each(["listDevices", "listWorkers"] as const)("offers a retry (%s)", async (call) => {
    const { restore } = await renderDevicesFailing(seedState(NOW), call)
    expect(screen.getByText("Couldn't load machines.")).toBeTruthy()
    expect(screen.queryByRole("listitem", { name: "devbox" })).toBeNull()
    restore()
    fireEvent.click(screen.getByRole("button", { name: "Try again" }))
    await advance(50)
    expect(rolesOf(machineCard("devbox"))).toEqual(["Device devbox", "Worker devbox"])
    expect(row("devbox").getByText("Online")).toBeTruthy()
  })
})

/** Renders the page while `call` fails; `restore` makes it work again. */
async function renderDevicesFailing(state: MockState, call: "listDevices" | "listWorkers") {
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
        [call]: () => (failing ? Promise.reject(new Error("boom")) : client.machines[call]()),
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
  const workers = () => within(machinesSection())
  const toggleOf = (name: string) => within(workerRow(name)).getByRole("switch", { name: "Integrator" })
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
    fireEvent.click(within(workerRow("devbox")).getByRole("switch"))
    await advance(0)
    await fail(new ApiError("unavailable", "down"))
    expect(within(workerRow("devbox")).getByRole("alert").textContent).toBe("无法更改合入者设置，请重试。")
  })

  it("drops a worker that is gone, with a notice that outlives its row", async () => {
    const { state, release } = await renderHeld()
    fireEvent.click(toggleOf("devbox"))
    await advance(0)
    state.workers = state.workers.filter((w) => w.workerId !== "worker-devbox")
    await release()
    // The machine's card keeps its device.
    expect(screen.queryByRole("listitem", { name: "Worker devbox" })).toBeNull()
    expect(rolesOf(machineCard("devbox"))).toEqual(["Device devbox"])
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
  const openDelete = async (kind: "device" | "worker", name: string) => {
    fireEvent.pointerDown(screen.getByRole("button", { name: `Actions for ${kind} ${name}` }), { button: 0, ctrlKey: false })
    await advance(0)
    fireEvent.click(screen.getByRole("menuitem", { name: "Delete…" }))
    await advance(0)
  }

  it("takes a deleted worker off the providers below at once, not at their next poll", async () => {
    await renderDevices(seedState(NOW))
    const providers = () => within(document.getElementById("providers")!)
    const anthropic = () => within(providers().getByText("Anthropic", { selector: "span.font-medium" }).closest("li")!)
    expect(anthropic().getAllByRole("listitem").map((li) => li.textContent)).toEqual(["cloud-runner", "devbox", "eric-mbp"])
    await openDelete("worker", "devbox")
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    await advance(50)
    expect(anthropic().getAllByRole("listitem").map((li) => li.textContent)).toEqual(["cloud-runner", "eric-mbp"])
  })

  it("confirms with what deleting a device does, then deletes it", async () => {
    const { state } = await renderDevices(seedState(NOW))
    await openDelete("device", "eric-win")
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
    // The card keeps the worker of the machine, and its name: the worker
    // reported the same hostname.
    expect(state.workers.some((w) => w.name === "eric-win")).toBe(true)
    expect(rolesOf(machineCard("ERIC-WIN"))).toEqual(["Worker eric-win"])
    expect(screen.queryByRole("listitem", { name: "Device eric-win" })).toBeNull()
  })

  it("warns when it is online, and says how deleting a worker differs", async () => {
    const { state } = await renderDevices(seedState(NOW))
    await openDelete("worker", "devbox")
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
    await openDelete("device", "eric-win")
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
    expect(rolesOf(machineCard("ERIC-WIN"))).toEqual(["Worker eric-win"])
  })

  it("takes the card away with the machine's last role", async () => {
    const { state } = await renderDevices(seedState(NOW))
    await openDelete("device", "palace-builder")
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    await advance(50)
    expect(state.devices.some((d) => d.name === "palace-builder")).toBe(false)
    expect(machineNames()).toEqual(["cloud-runner", "devbox", "ERIC-WIN", "erics-mbp.local"])
  })

  it("returns focus to the row's menu after Cancel, and to the card's other row once the row is gone", async () => {
    await renderDevices(seedState(NOW))
    const trigger = () => screen.getByRole("button", { name: "Actions for device eric-win" })
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
    // The card stays with the machine's worker: focus goes to its menu.
    expect(rolesOf(machineCard("ERIC-WIN"))).toEqual(["Worker eric-win"])
    expect(document.activeElement).toBe(within(workerRow("eric-win")).getByRole("button", { name: "Actions for worker eric-win" }))
  })

  it("returns focus to Add once the card is gone with the machine's last row", async () => {
    await renderDevices(seedState(NOW))
    await openDelete("device", "palace-builder")
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    await advance(300)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(machineNames()).not.toContain("palace-builder")
    expect(document.activeElement).toBe(addMenuTrigger())
  })

  it("returns focus to the device's menu once the worker of its card is deleted", async () => {
    await renderDevices(seedState(NOW))
    await openDelete("worker", "devbox")
    fireEvent.click(screen.getByRole("button", { name: "Delete" }))
    await advance(300)
    expect(rolesOf(machineCard("devbox"))).toEqual(["Device devbox"])
    expect(document.activeElement).toBe(screen.getByRole("button", { name: "Actions for device devbox" }))
  })

  it("reads in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderDevices(seedState(NOW))
    fireEvent.pointerDown(screen.getByRole("button", { name: "设备 devbox 的操作" }), { button: 0, ctrlKey: false })
    await advance(0)
    fireEvent.click(screen.getByRole("menuitem", { name: "删除…" }))
    await advance(0)
    const dialog = within(screen.getByRole("dialog", { name: "删除设备 devbox？" }))
    expect(dialog.getByText("devbox 当前在线：它会立即断开连接，在它上面运行的会话会失败。")).toBeTruthy()
  })
})
