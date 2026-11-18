// @vitest-environment happy-dom
import { cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { DEVICES_LIVE_INTERVAL_MS } from "@/api/hooks"
import { SEED_OWNER_ID, seedState, type MockState } from "@/api/mock/state"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { advance, renderWithBackend } from "@/test/render"

import { DAEMON_COMMAND, DevicesPage, ENROLL_COMMAND, JOIN_COMMAND } from "./devices-page"

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

/** The list item of the device with that name. */
const row = (name: string) => {
  const item = screen.getByText(name, { selector: "span" }).closest("li")
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
    expect(row("devbox").getByText("守护进程0.1.0")).toBeTruthy()
    expect(row("eric-win").getByText("前天在线")).toBeTruthy()
    expect(row("spare").getByText("从未连接")).toBeTruthy()
  })
})

describe("without devices", () => {
  it("says so and shows the commands that add one, ready to copy", async () => {
    const writeText = vi.fn(() => Promise.resolve())
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true })

    const state = seedState(NOW)
    state.devices = []
    await renderDevices(state)

    expect(screen.getByText("No devices yet")).toBeTruthy()
    const shown = Array.from(document.querySelectorAll("pre code")).map((c) => c.textContent)
    expect(shown).toEqual([ENROLL_COMMAND, JOIN_COMMAND, DAEMON_COMMAND])
    expect(ENROLL_COMMAND).toBe("circus server enroll-token create --role daemon")
    expect(JOIN_COMMAND).toBe("circus join")
    expect(DAEMON_COMMAND).toBe("circus daemon")

    const copyButtons = screen.getAllByRole("button", { name: "Copy command" })
    expect(copyButtons).toHaveLength(3)
    fireEvent.click(copyButtons[1])
    await advance(0)
    expect(writeText).toHaveBeenCalledWith("circus join")
    expect(screen.getByRole("button", { name: "Copied" })).toBeTruthy()
    await advance(2000)
    expect(screen.queryByRole("button", { name: "Copied" })).toBeNull()
  })
})

describe("when the list can't load", () => {
  it("offers a retry", async () => {
    const { restore } = await renderDevicesFailing(seedState(NOW))
    expect(screen.getByText("Couldn't load devices.")).toBeTruthy()
    restore()
    fireEvent.click(screen.getByRole("button", { name: "Try again" }))
    await advance(50)
    expect(screen.getByText("devbox", { selector: "span" })).toBeTruthy()
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
