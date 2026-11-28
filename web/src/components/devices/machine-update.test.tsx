// @vitest-environment happy-dom
import { cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { DEVICES_LIVE_INTERVAL_MS, MACHINE_UPDATE_INTERVAL_MS } from "@/api/hooks"
import { defaultUpdateTiming, type MockUpdateTiming } from "@/api/mock/machine-updates"
import { seedState, type MockState } from "@/api/mock/state"
import type { CircusClient, Device, Worker } from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { en } from "@/i18n/locales/en"
import { zhCN } from "@/i18n/locales/zh-CN"
import { DevicesPage } from "@/routes/devices-page"
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

/** The card (list item) of the device or worker with that name. */
function card(section: "Devices" | "Workers", name: string): HTMLElement {
  const region = within(screen.getByRole("region", { name: section }))
  return region.getByText(name, { selector: "span.font-medium" }).closest("li")!
}
const device = (name: string) => card("Devices", name)
const worker = (name: string) => card("Workers", name)

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

const updateButton = (c: HTMLElement) => within(c).getByRole("button", { name: /^Update the machine of / })

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
  it("offer an update only on a card whose role runs as the service, and run it on the whole machine", async () => {
    const { state } = await renderDevices()
    expect(shown(device("devbox"))).toEqual(["Update to v0.2.0"])
    // devbox's worker runs in the foreground: no button on its card.
    expect(shown(worker("devbox"))).toEqual([])
    // Every card has the component, and no live region speaks on load.
    for (const c of [device("devbox"), worker("devbox"), device("palace-builder"), worker("eric-mbp")]) {
      expect(announced(c)).toBe("")
    }

    const button = updateButton(device("devbox"))
    button.focus()
    fireEvent.click(button)
    await advance(10)
    // The machine's update shows on both cards.
    expect(shown(device("devbox"))).toEqual(["Updating to v0.2.0…"])
    expect(shown(worker("devbox"))).toEqual(["Updating to v0.2.0…"])
    // Focus moved from the button to the status that replaced it.
    expect(document.activeElement?.textContent).toBe("Updating to v0.2.0…")
    expect(device("devbox").contains(document.activeElement)).toBe(true)
    // The focused status says it runs: the live region does not repeat it.
    expect(announced(device("devbox"))).toBe("")
    expect(announced(worker("devbox"))).toBe("")

    // The machine comes back with the new version: the next reads say so.
    await advance(COMPLETE_MS + MACHINE_UPDATE_INTERVAL_MS)
    expect(state.devices.find((d) => d.name === "devbox")!.version).toBe("v0.2.0")
    expect(shown(device("devbox"))).toEqual([])
    expect(shown(worker("devbox"))).toEqual([])
    expect(within(device("devbox")).getByText("daemon v0.2.0")).toBeTruthy()
    expect(within(worker("devbox")).getByText("worker v0.2.0")).toBeTruthy()
    expect(announced(device("devbox"))).toBe("The machine of devbox is updated to v0.2.0")
    expect(announced(worker("devbox"))).toBe("")
    // Nothing of the update remains: focus goes to the card's menu button, never to a hidden element.
    expect(document.activeElement).toBe(within(device("devbox")).getByRole("button", { name: "Actions for devbox" }))
  })

  it("show a running update from the server, as after a reload, without announcing it", async () => {
    await renderDevices()
    expect(shown(device("palace-builder"))).toEqual(["Updating to v0.2.0…"])
    expect(within(device("palace-builder")).getByText("Updating to v0.2.0…").querySelector("[data-slot=spinner]")).toBeTruthy()
    expect(announced(device("palace-builder"))).toBe("")
  })

  it("show a command's failure on both cards, in full in a popover, and retry it from either", async () => {
    await renderDevices()
    // The last cause; the popover has the whole output.
    for (const c of [device("eric-mbp"), worker("eric-mbp")]) {
      expect(shown(c)).toEqual(["Update failed: i/o timeout", "Retry"])
      // Existing failures are not announced.
      expect(within(c).queryByRole("alert")).toBeNull()
    }
    fireEvent.click(within(worker("eric-mbp")).getByRole("button", { name: /^Update failed: i\/o timeout\. Show why/ }))
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

    const retry = within(worker("eric-mbp")).getByRole("button", { name: "Retry updating the machine of eric-mbp to v0.2.0" })
    retry.focus()
    fireEvent.click(retry)
    await advance(10)
    expect(shown(device("eric-mbp"))).toEqual(["Updating to v0.2.0…"])
    expect(shown(worker("eric-mbp"))).toEqual(["Updating to v0.2.0…"])
    expect(worker("eric-mbp").contains(document.activeElement)).toBe(true)
    expect(announced(worker("eric-mbp"))).toBe("")
    expect(announced(device("eric-mbp"))).toBe("")
  })

  it("never take back focus the user moved elsewhere, to the page or another control", async () => {
    await renderDevices({ updatesDemo: false })
    for (const [name, elsewhere] of [
      ["devbox", () => (document.activeElement as HTMLElement).blur()],
      ["cloud-runner", () => within(device("eric-win")).getByRole("button", { name: "Actions for eric-win" }).focus()],
    ] as const) {
      const c = name === "devbox" ? device(name) : worker(name)
      const button = updateButton(c)
      button.focus()
      fireEvent.click(button)
      await advance(10)
      expect(document.activeElement?.textContent).toBe("Updating to v0.2.0…")
      elsewhere()
      await advance(10)
      const before = document.activeElement
      await advance(COMPLETE_MS + MACHINE_UPDATE_INTERVAL_MS)
      expect(shown(c)).toEqual([])
      expect(document.activeElement).toBe(before)
    }
    expect(document.activeElement).toBe(within(device("eric-win")).getByRole("button", { name: "Actions for eric-win" }))
  })

  it("move focus to the card's menu when a refused press removes the button", async () => {
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
    const button = updateButton(device("devbox"))
    button.focus()
    fireEvent.click(button)
    await advance(10)
    expect(shown(device("devbox"))).toEqual([])
    expect(document.activeElement).toBe(within(device("devbox")).getByRole("button", { name: "Actions for devbox" }))
  })

  it("announce on the card last pressed only, the running update too when focus did not move to it", async () => {
    await renderDevices({ updatesDemo: false, updateTiming: { completeMs: 60_000, deadlineMs: 20_000 } })
    // Pressed without focus (a mouse in some browsers): the region says it runs.
    fireEvent.click(updateButton(device("eric-mbp")))
    await advance(10)
    expect(announced(device("eric-mbp"))).toBe("Updating the machine of eric-mbp to v0.2.0…")
    await advance(20_000 + MACHINE_UPDATE_INTERVAL_MS)
    expect(announced(device("eric-mbp"))).toBe("Updating the machine of eric-mbp failed")
    // That attempt ended: a retry pressed on the other card speaks there only.
    fireEvent.click(within(worker("eric-mbp")).getByRole("button", { name: "Retry updating the machine of eric-mbp to v0.2.0" }))
    await advance(10)
    expect(announced(worker("eric-mbp"))).toBe("Updating the machine of eric-mbp to v0.2.0…")
    expect(announced(device("eric-mbp"))).toBe("Updating the machine of eric-mbp failed")
  })

  it("say a timed-out update and an unreachable machine in the user's language", async () => {
    await renderDevices()
    // cloud-runner timed out; Retry is offered.
    expect(shown(worker("cloud-runner"))).toEqual(["Update timed out", "Retry"])
    fireEvent.click(within(worker("cloud-runner")).getByRole("button", { name: /^Update timed out\. Show why/ }))
    await advance(10)
    let dialog = screen.getByRole("dialog")
    expect(dialog.textContent).toContain("No role of the machine came back with v0.2.0 within 5 minutes.")
    expect(dialog.querySelector("pre")).toBeNull()
    fireEvent.keyDown(dialog, { key: "Escape" })
    await advance(10)

    // eric-win's command couldn't be sent; it is offline, so no Retry.
    expect(shown(device("eric-win"))).toEqual(["Update failed: couldn't reach the machine"])
    fireEvent.click(within(device("eric-win")).getByRole("button", { name: /^Update failed: couldn't reach the machine\. Show why/ }))
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
    expect(shown(device("eric-mbp"))).toEqual(["Update failed", "Retry"])
  })

  it("show a failure without Retry while no update can be started", async () => {
    const state = seedState(NOW)
    for (const m of [...state.devices, ...state.workers]) if (m.name === "eric-mbp") m.online = false
    await renderDevices({ state })
    expect(shown(device("eric-mbp"))).toEqual(["Update failed: i/o timeout"])
  })

  it("fail a running update at its deadline on both cards, offer Retry, and poll slowly again", async () => {
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
    fireEvent.click(updateButton(device("eric-mbp")))
    await advance(10)
    expect(shown(worker("eric-mbp"))).toEqual(["Updating to v0.2.0…"])

    await advance(20_000 + MACHINE_UPDATE_INTERVAL_MS)
    for (const c of [device("eric-mbp"), worker("eric-mbp")]) expect(shown(c)).toEqual(["Update timed out", "Retry"])
    expect(announced(device("eric-mbp"))).toBe("Updating the machine of eric-mbp failed")
    expect(announced(worker("eric-mbp"))).toBe("")

    // Nothing runs any more: back to every ten seconds.
    await advance(DEVICES_LIVE_INTERVAL_MS)
    const before = { ...counts }
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect([counts.devices - before.devices, counts.workers - before.workers]).toEqual([1, 1])
  })

  it.each([
    ["device", device],
    ["worker", worker],
  ] as const)("show the running update on both cards from the answer alone, pressing the %s card", async (_, pressed) => {
    let hold = false
    const never = new Promise<never>(() => {})
    await renderDevices({
      updatesDemo: false,
      machines: (real) => ({
        listDevices: () => (hold ? never : real.listDevices()),
        listWorkers: () => (hold ? never : real.listWorkers()),
      }),
    })
    hold = true
    fireEvent.click(updateButton(pressed("eric-mbp")))
    await advance(10)
    expect(shown(device("eric-mbp"))).toEqual(["Updating to v0.2.0…"])
    expect(shown(worker("eric-mbp"))).toEqual(["Updating to v0.2.0…"])
    // Another machine is untouched.
    expect(shown(worker("cloud-runner"))).toEqual(["Update to v0.2.0"])
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
    const button = updateButton(device("eric-mbp"))
    fireEvent.click(button, { detail: 1 })
    fireEvent.click(button, { detail: 2 })
    await advance(10)
    fireEvent.click(button, { detail: 1 })
    await advance(10)
    expect(calls).toBe(1)
    expect(button.getAttribute("aria-busy")).toBe("true")
    answer!({ targetVersion: "v0.2.0", state: "running", error: "", startedUnixMs: NOW, deadlineUnixMs: NOW + 300_000 })
    await advance(10)
    expect(shown(device("eric-mbp"))).toEqual(["Updating to v0.2.0…"])
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

    fireEvent.click(updateButton(worker("cloud-runner")))
    await advance(10)
    before = { ...counts }
    await advance(5 * MACHINE_UPDATE_INTERVAL_MS)
    expect(counts.devices - before.devices).toBeGreaterThanOrEqual(4)
    expect(counts.workers - before.workers).toBeGreaterThanOrEqual(4)

    // Done: back to every ten seconds.
    await advance(COMPLETE_MS)
    expect(shown(worker("cloud-runner"))).toEqual([])
    await advance(DEVICES_LIVE_INTERVAL_MS)
    before = { ...counts }
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect([counts.devices - before.devices, counts.workers - before.workers]).toEqual([1, 1])
  })

  it("say why when no update is offered any more, with the server's reason", async () => {
    await renderDevices({
      machines: () => ({
        updateMachine: () => Promise.reject(new ApiError("failed_precondition", "already at the latest release v0.2.0")),
      }),
    })
    fireEvent.click(updateButton(device("devbox")))
    await advance(10)
    expect(screen.getByText("No update for devbox")).toBeTruthy()
    expect(screen.getByText("already at the latest release v0.2.0")).toBeTruthy()
  })

  it("say when the machine is gone", async () => {
    await renderDevices({
      updatesDemo: false,
      machines: () => ({ updateMachine: () => Promise.reject(new ApiError("not_found", "gone")) }),
    })
    fireEvent.click(updateButton(worker("eric-mbp")))
    await advance(10)
    expect(screen.getByText("Worker eric-mbp no longer exists")).toBeTruthy()
  })

  it("read in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderDevices()
    const region = within(screen.getByRole("region", { name: "设备" }))
    const row = (name: string) => region.getByText(name, { selector: "span.font-medium" }).closest("li")!
    expect(within(row("devbox")).getByRole("button", { name: "将 devbox 所在的机器更新到 v0.2.0" }).textContent).toBe("更新到 v0.2.0")
    expect(shown(row("palace-builder"))).toEqual(["正在更新到 v0.2.0…"])
    expect(shown(row("eric-mbp"))).toEqual(["更新失败：i/o timeout", "重试"])
    expect(shown(row("eric-win"))).toEqual(["更新失败：无法连接这台机器"])
    expect(within(row("eric-mbp")).getByRole("button", { name: "重新将 eric-mbp 所在的机器更新到 v0.2.0" })).toBeTruthy()
    const workers = within(screen.getByRole("region", { name: "工作节点" }))
    expect(shown(workers.getByText("cloud-runner", { selector: "span.font-medium" }).closest("li")!)).toEqual(["更新超时", "重试"])
  })

  it("have every text in both catalogs", () => {
    expect(Object.keys(zhCN.devices.update).sort()).toEqual(Object.keys(en.devices.update).sort())
  })
})
