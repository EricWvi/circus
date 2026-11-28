// @vitest-environment happy-dom
import { cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { DEVICES_LIVE_INTERVAL_MS, MACHINE_UPDATE_INTERVAL_MS } from "@/api/hooks"
import { defaultUpdateTiming, type MockUpdateTiming } from "@/api/mock/machine-updates"
import { seedState, type MockState } from "@/api/mock/state"
import { queryKeys } from "@/api/query-client"
import type { CircusClient, Device, Worker } from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { en } from "@/i18n/locales/en"
import { zhCN } from "@/i18n/locales/zh-CN"
import { DevicesPage } from "@/routes/devices-page"
import { deviceRow, machineCard, workerRow } from "@/test/machines"
import { advance, renderWithBackend } from "@/test/render"

const NOW = Date.parse("2026-11-28T12:00:00Z")
const COMPLETE_MS = defaultUpdateTiming.completeMs

type Machines = CircusClient["machines"]

/** Renders /devices; `machines` stands in for some machine calls. */
async function renderDevices({
  state = seedState(NOW),
  machines,
  updatesDemo = true,
  updateTiming = defaultUpdateTiming,
}: {
  state?: MockState
  machines?: (real: Machines) => Partial<Machines>
  updatesDemo?: boolean
  updateTiming?: MockUpdateTiming
} = {}) {
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
  const result = await renderWithBackend(<RouterProvider router={router} />, {
    state,
    options: { updatesDemo, updateTiming },
    wrapClient: (client) =>
      machines ? { ...client, machines: { ...client.machines, ...machines(client.machines) } } : client,
  })
  await advance(50)
  return result
}

/** The card of the machine with that name: eric-mbp's is erics-mbp.local, eric-win's ERIC-WIN. */
const card = (name: string) => machineCard(name)

/** A card's machine update component: mounted on every card, showing something or not. */
function slot(c: HTMLElement): HTMLElement {
  const s = c.querySelector<HTMLElement>("[data-slot=machine-update]")
  if (!s) throw new Error("no machine update on the card")
  return s
}

/** What a card's update shows: its update button, running status, or failure and Retry. */
function shown(c: HTMLElement): (string | null)[] {
  const content = [...slot(c).children].filter((e) => !e.hasAttribute("aria-live"))
  return content.flatMap((e) =>
    e.tagName === "BUTTON" || e.hasAttribute("tabindex") ? [e.textContent] : [...e.children].map((x) => x.textContent),
  )
}

/** What a card's live region says. */
const announced = (c: HTMLElement) => slot(c).querySelector("[aria-live=polite]")!.textContent

const updateButton = (c: HTMLElement) => within(c).getByRole("button", { name: /^Update .* to v/ })

/** Records the identity of each UpdateMachine request, passing it on. */
function recording(seen: string[]) {
  return (real: Machines): Partial<Machines> => ({
    updateMachine: (req) => {
      seen.push("deviceId" in req ? `device ${req.deviceId}` : `worker ${req.workerId}`)
      return real.updateMachine(req)
    },
  })
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

describe("machine updates", () => {
  it("offer one update on a machine's card, and run it on the whole machine", async () => {
    const seen: string[] = []
    const { state } = await renderDevices({ machines: recording(seen) })
    // devbox's device runs as the service (its worker in the foreground):
    // one button for the machine, beside its name, not on a role's row.
    expect(shown(card("devbox"))).toEqual(["Update to v0.2.0"])
    expect(within(card("devbox")).getAllByRole("button", { name: /^Update / })).toHaveLength(1)
    expect(card("devbox").querySelectorAll("[data-slot=machine-update]")).toHaveLength(1)
    expect(card("devbox").querySelector("[data-role-row] [data-slot=machine-update]")).toBeNull()
    // Every card has the component, and no live region speaks on load.
    for (const name of ["devbox", "palace-builder", "erics-mbp.local", "cloud-runner"]) {
      expect(announced(card(name))).toBe("")
    }

    const button = updateButton(card("devbox"))
    expect(button.getAttribute("aria-label")).toBe("Update devbox to v0.2.0")
    button.focus()
    fireEvent.click(button)
    await advance(10)
    // Asked through the role that offers it.
    expect(seen).toEqual(["device device-devbox"])
    expect(shown(card("devbox"))).toEqual(["Updating to v0.2.0…"])
    // Focus moved from the button to the status that replaced it.
    expect(document.activeElement?.textContent).toBe("Updating to v0.2.0…")
    expect(card("devbox").contains(document.activeElement)).toBe(true)
    // The focused status says it runs: the live region does not repeat it.
    expect(announced(card("devbox"))).toBe("")

    // The machine comes back with the new version: the next reads say so.
    await advance(COMPLETE_MS + MACHINE_UPDATE_INTERVAL_MS)
    expect(state.devices.find((d) => d.name === "devbox")!.version).toBe("v0.2.0")
    expect(shown(card("devbox"))).toEqual([])
    expect(within(deviceRow("devbox")).getByText("v0.2.0")).toBeTruthy()
    expect(within(workerRow("devbox")).getByText("v0.2.0")).toBeTruthy()
    expect(announced(card("devbox"))).toBe("devbox is updated to v0.2.0")
    // Nothing of the update remains: focus goes to the card's first menu button, never to a hidden element.
    expect(document.activeElement).toBe(within(card("devbox")).getByRole("button", { name: "Actions for device devbox" }))
  })

  it("ask through the worker when only it offers the update, and offer it on a worker's own machine", async () => {
    const state = seedState(NOW)
    // devbox the other way round: its daemon in the foreground, its worker the service.
    state.devices.find((d) => d.name === "devbox")!.foreground = true
    state.workers.find((w) => w.name === "devbox")!.foreground = false
    const seen: string[] = []
    await renderDevices({ state, updatesDemo: false, machines: recording(seen) })
    expect(shown(card("devbox"))).toEqual(["Update to v0.2.0"])
    fireEvent.click(updateButton(card("devbox")))
    await advance(10)
    expect(shown(card("cloud-runner"))).toEqual(["Update to v0.2.0"])
    fireEvent.click(updateButton(card("cloud-runner")))
    await advance(10)
    expect(seen).toEqual(["worker worker-devbox", "worker worker-cloud-runner"])
    expect(shown(card("devbox"))).toEqual(["Updating to v0.2.0…"])
    expect(shown(card("cloud-runner"))).toEqual(["Updating to v0.2.0…"])
  })

  it("show a running update from the server, as after a reload, without announcing it", async () => {
    await renderDevices()
    expect(shown(card("palace-builder"))).toEqual(["Updating to v0.2.0…"])
    expect(within(card("palace-builder")).getByText("Updating to v0.2.0…").querySelector("[data-slot=spinner]")).toBeTruthy()
    expect(announced(card("palace-builder"))).toBe("")
  })

  it("show a command's failure once, in full in a popover, and retry it", async () => {
    await renderDevices()
    const mbp = () => card("erics-mbp.local")
    // The last cause; the popover has the whole output.
    expect(shown(mbp())).toEqual(["Update failed: i/o timeout", "Retry"])
    // Existing failures are not announced.
    expect(within(mbp()).queryByRole("alert")).toBeNull()
    fireEvent.click(within(mbp()).getByRole("button", { name: /^Update failed: i\/o timeout\. Show why updating erics-mbp\.local failed$/ }))
    await advance(10)
    const dialog = screen.getByRole("dialog", { name: "Updating to v0.2.0 failed" })
    const output = dialog.querySelector("pre")!
    expect(output.textContent).toMatch(/^Checking the latest circus release…\n/)
    expect(output.textContent).toMatch(/dial tcp 140\.82\.112\.4:443: i\/o timeout$/)
    // A bounded, scrollable area that the keyboard can reach.
    expect(output.className).toMatch(/max-h-/)
    expect(output.className).toMatch(/overflow-auto/)
    expect(output.getAttribute("tabindex")).toBe("0")
    fireEvent.keyDown(dialog, { key: "Escape" })
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()

    const retry = within(mbp()).getByRole("button", { name: "Retry updating erics-mbp.local to v0.2.0" })
    retry.focus()
    fireEvent.click(retry)
    await advance(10)
    expect(shown(mbp())).toEqual(["Updating to v0.2.0…"])
    expect(mbp().contains(document.activeElement)).toBe(true)
    expect(announced(mbp())).toBe("")
  })

  it("never take back focus the user moved elsewhere, to the page or another control", async () => {
    await renderDevices({ updatesDemo: false })
    const elsewhereMenu = () => within(card("ERIC-WIN")).getByRole("button", { name: "Actions for device eric-win" })
    for (const [name, elsewhere] of [
      ["devbox", () => (document.activeElement as HTMLElement).blur()],
      ["cloud-runner", () => elsewhereMenu().focus()],
    ] as const) {
      const button = updateButton(card(name))
      button.focus()
      fireEvent.click(button)
      await advance(10)
      expect(document.activeElement?.textContent).toBe("Updating to v0.2.0…")
      elsewhere()
      await advance(10)
      const before = document.activeElement
      await advance(COMPLETE_MS + MACHINE_UPDATE_INTERVAL_MS)
      expect(shown(card(name))).toEqual([])
      expect(document.activeElement).toBe(before)
    }
    expect(document.activeElement).toBe(elsewhereMenu())
  })

  it("move focus to the card's first menu when a refused press removes the button", async () => {
    let refused = false
    const noOffer = <T extends Device | Worker>(list: T[]) =>
      refused ? list.map((m) => (m.name === "devbox" ? { ...m, updateVersion: "" } : m)) : list
    await renderDevices({
      machines: (real) => ({
        updateMachine: () => {
          refused = true
          return Promise.reject(new ApiError("failed_precondition", "already at the latest release v0.2.0"))
        },
        listDevices: async () => noOffer(await real.listDevices()),
        listWorkers: async () => noOffer(await real.listWorkers()),
      }),
    })
    const button = updateButton(card("devbox"))
    button.focus()
    fireEvent.click(button)
    await advance(10)
    expect(shown(card("devbox"))).toEqual([])
    expect(document.activeElement).toBe(within(card("devbox")).getByRole("button", { name: "Actions for device devbox" }))
  })

  it("announce the running update when focus did not move to it, and how each attempt pressed ends", async () => {
    await renderDevices({ updatesDemo: false, updateTiming: { completeMs: 60_000, deadlineMs: 20_000 } })
    const mbp = () => card("erics-mbp.local")
    // Pressed without focus (a mouse in some browsers): the region says it runs.
    fireEvent.click(updateButton(mbp()))
    await advance(10)
    expect(announced(mbp())).toBe("Updating erics-mbp.local to v0.2.0…")
    await advance(20_000 + MACHINE_UPDATE_INTERVAL_MS)
    expect(announced(mbp())).toBe("Updating erics-mbp.local failed")
    // Another machine's card says nothing of it.
    expect(announced(card("devbox"))).toBe("")
    // A retry is a new attempt: it speaks again.
    fireEvent.click(within(mbp()).getByRole("button", { name: "Retry updating erics-mbp.local to v0.2.0" }))
    await advance(10)
    expect(announced(mbp())).toBe("Updating erics-mbp.local to v0.2.0…")
  })

  it("say a timed-out update and an unreachable machine in the user's language", async () => {
    await renderDevices()
    // cloud-runner timed out; Retry is offered.
    expect(shown(card("cloud-runner"))).toEqual(["Update timed out", "Retry"])
    fireEvent.click(within(card("cloud-runner")).getByRole("button", { name: /^Update timed out\. Show why/ }))
    await advance(10)
    let dialog = screen.getByRole("dialog")
    expect(dialog.textContent).toContain("No role of the machine came back with v0.2.0 within 5 minutes.")
    expect(dialog.querySelector("pre")).toBeNull()
    fireEvent.keyDown(dialog, { key: "Escape" })
    await advance(10)

    // eric-win's command couldn't be sent; it is offline, so no Retry.
    expect(shown(card("ERIC-WIN"))).toEqual(["Update failed: couldn't reach the machine"])
    fireEvent.click(within(card("ERIC-WIN")).getByRole("button", { name: /^Update failed: couldn't reach the machine\. Show why/ }))
    await advance(10)
    dialog = screen.getByRole("dialog")
    expect(dialog.textContent).toContain("The command couldn't be sent to the machine:")
    expect(dialog.querySelector("pre")!.textContent).toBe("the device's link closed before the command was sent")
  })

  it("say only that the update failed when the command left no output", async () => {
    const empty = <T extends Device | Worker>(list: T[]) =>
      list.map((m) => (m.name === "eric-mbp" && m.update ? { ...m, update: { ...m.update, error: "" } } : m))
    await renderDevices({
      machines: (real) => ({
        listDevices: async () => empty(await real.listDevices()),
        listWorkers: async () => empty(await real.listWorkers()),
      }),
    })
    expect(shown(card("erics-mbp.local"))).toEqual(["Update failed", "Retry"])
  })

  it("show a failure without Retry while no update can be started", async () => {
    const state = seedState(NOW)
    for (const m of [...state.devices, ...state.workers]) if (m.name === "eric-mbp") m.online = false
    await renderDevices({ state })
    expect(shown(card("erics-mbp.local"))).toEqual(["Update failed: i/o timeout"])
  })

  it("fail a running update at its deadline, offer Retry, and poll slowly again", async () => {
    const counts = { devices: 0, workers: 0 }
    await renderDevices({
      updatesDemo: false,
      updateTiming: { completeMs: 60_000, deadlineMs: 20_000 },
      machines: (real) => ({
        listDevices: () => {
          counts.devices++
          return real.listDevices()
        },
        listWorkers: () => {
          counts.workers++
          return real.listWorkers()
        },
      }),
    })
    const mbp = () => card("erics-mbp.local")
    fireEvent.click(updateButton(mbp()))
    await advance(10)
    expect(shown(mbp())).toEqual(["Updating to v0.2.0…"])

    await advance(20_000 + MACHINE_UPDATE_INTERVAL_MS)
    expect(shown(mbp())).toEqual(["Update timed out", "Retry"])
    expect(announced(mbp())).toBe("Updating erics-mbp.local failed")

    // Nothing runs any more: back to every ten seconds.
    await advance(DEVICES_LIVE_INTERVAL_MS)
    const before = { ...counts }
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect([counts.devices - before.devices, counts.workers - before.workers]).toEqual([1, 1])
  })

  it.each([
    ["through its device", "erics-mbp.local"],
    ["through its worker", "cloud-runner"],
  ] as const)("show the running update from the answer alone, asked %s", async (_, name) => {
    let hold = false
    const never = new Promise<never>(() => {})
    const { queryClient } = await renderDevices({
      updatesDemo: false,
      machines: (real) => ({
        listDevices: () => (hold ? never : real.listDevices()),
        listWorkers: () => (hold ? never : real.listWorkers()),
      }),
    })
    hold = true
    fireEvent.click(updateButton(card(name)))
    await advance(10)
    expect(shown(card(name))).toEqual(["Updating to v0.2.0…"])
    // Every role of the machine has it, and no other.
    const roles = [
      ...(queryClient.getQueryData<Device[]>(queryKeys.devices) ?? []),
      ...(queryClient.getQueryData<Worker[]>(queryKeys.workers) ?? []),
    ]
    expect(roles.filter((r) => r.update).map((r) => r.name)).toEqual(
      name === "cloud-runner" ? ["cloud-runner"] : ["eric-mbp", "eric-mbp"],
    )
    // Another machine is untouched.
    expect(shown(card(name === "cloud-runner" ? "erics-mbp.local" : "cloud-runner"))).toEqual(["Update to v0.2.0"])
  })

  it.each([
    ["the device", false],
    ["the worker", true],
  ] as const)("put the answer on every role with the machine's key, whatever their names, asked through %s", async (_, viaWorker) => {
    const state = seedState(NOW)
    // erics-mbp.local's device is named laptop; its worker eric-mbp.
    state.devices.find((d) => d.name === "eric-mbp")!.name = "laptop"
    if (viaWorker) state.devices.find((d) => d.name === "laptop")!.foreground = true
    const seen: string[] = []
    let hold = false
    const never = new Promise<never>(() => {})
    const { queryClient } = await renderDevices({
      state,
      updatesDemo: false,
      machines: (real) => ({
        ...recording(seen)(real),
        listDevices: () => (hold ? never : real.listDevices()),
        listWorkers: () => (hold ? never : real.listWorkers()),
      }),
    })
    // Refetches held back: only the answer can change the lists.
    hold = true
    fireEvent.click(updateButton(card("erics-mbp.local")))
    await advance(10)
    expect(seen).toEqual([viaWorker ? "worker worker-eric-mbp" : "device device-eric-mbp"])
    expect(shown(card("erics-mbp.local"))).toEqual(["Updating to v0.2.0…"])
    const devices = queryClient.getQueryData<Device[]>(queryKeys.devices)!
    const workers = queryClient.getQueryData<Worker[]>(queryKeys.workers)!
    const running = (r: Device | Worker) => r.update?.state === "running" && r.updateVersion === ""
    expect(devices.filter(running).map((d) => d.name)).toEqual(["laptop"])
    expect(workers.filter(running).map((w) => w.name)).toEqual(["eric-mbp"])
  })

  it("send one request for a double click and for clicks while it is pending", async () => {
    let calls = 0
    let answer: ((u: Awaited<ReturnType<Machines["updateMachine"]>>) => void) | undefined
    await renderDevices({
      updatesDemo: false,
      machines: (real) => ({
        updateMachine: (req) => {
          calls++
          return new Promise((resolve) => {
            answer = resolve
            void real.updateMachine(req)
          })
        },
      }),
    })
    const button = updateButton(card("erics-mbp.local"))
    fireEvent.click(button, { detail: 1 })
    fireEvent.click(button, { detail: 2 })
    await advance(10)
    fireEvent.click(button, { detail: 1 })
    await advance(10)
    expect(calls).toBe(1)
    expect(button.getAttribute("aria-busy")).toBe("true")
    answer!({ targetVersion: "v0.2.0", state: "running", error: "", startedUnixMs: NOW, deadlineUnixMs: NOW + 300_000 })
    await advance(10)
    expect(shown(card("erics-mbp.local"))).toEqual(["Updating to v0.2.0…"])
    expect(calls).toBe(1)
  })

  it("poll both lists every two seconds while an update runs, and every ten otherwise", async () => {
    const counts = { devices: 0, workers: 0 }
    await renderDevices({
      updatesDemo: false,
      machines: (real) => ({
        listDevices: () => {
          counts.devices++
          return real.listDevices()
        },
        listWorkers: () => {
          counts.workers++
          return real.listWorkers()
        },
      }),
    })
    // Idle: every ten seconds.
    let before = { ...counts }
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect([counts.devices - before.devices, counts.workers - before.workers]).toEqual([1, 1])

    fireEvent.click(updateButton(card("cloud-runner")))
    await advance(10)
    before = { ...counts }
    await advance(5 * MACHINE_UPDATE_INTERVAL_MS)
    expect(counts.devices - before.devices).toBeGreaterThanOrEqual(4)
    expect(counts.workers - before.workers).toBeGreaterThanOrEqual(4)

    // Done: back to every ten seconds.
    await advance(COMPLETE_MS)
    expect(shown(card("cloud-runner"))).toEqual([])
    await advance(DEVICES_LIVE_INTERVAL_MS)
    before = { ...counts }
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect([counts.devices - before.devices, counts.workers - before.workers]).toEqual([1, 1])
  })

  it("say why when no update is offered any more, with the server's reason", async () => {
    await renderDevices({
      updatesDemo: false,
      machines: () => ({
        updateMachine: () => Promise.reject(new ApiError("failed_precondition", "already at the latest release v0.2.0")),
      }),
    })
    fireEvent.click(updateButton(card("erics-mbp.local")))
    await advance(10)
    expect(screen.getByText("No update for erics-mbp.local")).toBeTruthy()
    expect(screen.getByText("already at the latest release v0.2.0")).toBeTruthy()
  })

  it("say when the role it was asked through is gone", async () => {
    await renderDevices({
      updatesDemo: false,
      machines: () => ({ updateMachine: () => Promise.reject(new ApiError("not_found", "gone")) }),
    })
    fireEvent.click(updateButton(card("cloud-runner")))
    await advance(10)
    expect(screen.getByText("Worker cloud-runner no longer exists")).toBeTruthy()
    fireEvent.click(updateButton(card("erics-mbp.local")))
    await advance(10)
    expect(screen.getByText("Device eric-mbp no longer exists")).toBeTruthy()
  })

  it("read in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderDevices()
    expect(within(card("devbox")).getByRole("button", { name: "将 devbox 更新到 v0.2.0" }).textContent).toBe("更新到 v0.2.0")
    expect(shown(card("palace-builder"))).toEqual(["正在更新到 v0.2.0…"])
    expect(shown(card("erics-mbp.local"))).toEqual(["更新失败：i/o timeout", "重试"])
    expect(shown(card("ERIC-WIN"))).toEqual(["更新失败：无法连接这台机器"])
    expect(within(card("erics-mbp.local")).getByRole("button", { name: "重新将 erics-mbp.local 更新到 v0.2.0" })).toBeTruthy()
    expect(shown(card("cloud-runner"))).toEqual(["更新超时", "重试"])
  })

  it("have every text in both catalogs", () => {
    expect(Object.keys(zhCN.devices.update).sort()).toEqual(Object.keys(en.devices.update).sort())
  })
})
