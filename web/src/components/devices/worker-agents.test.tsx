// @vitest-environment happy-dom
import { cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { AGENT_ANSWER_WAIT_MS, AGENTS_BUSY_FAST_MS, DEVICES_LIVE_INTERVAL_MS } from "@/api/hooks"
import { addAgentsDemo, addQueueDemo, mockAgent, seedState, type MockState } from "@/api/mock/state"
import type { CircusClient, Worker } from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { DevicesPage } from "@/routes/devices-page"
import { advance, renderWithBackend } from "@/test/render"

const NOW = Date.parse("2026-11-26T12:00:00Z")
const DAY = 86_400_000

type Machines = CircusClient["machines"]

/** Renders /devices; `machines` stands in for some of the machine calls (the real ones are its second argument). */
async function renderDevices(state: MockState, machines?: (real: Machines) => Partial<Machines>) {
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
    wrapClient: (client) =>
      machines ? { ...client, machines: { ...client.machines, ...machines(client.machines) } } : client,
  })
  await advance(50)
  return result
}

function agentsDemo() {
  const state = seedState(NOW)
  addAgentsDemo(state, NOW)
  return state
}

/** Counts the list reads, passing them on. */
function countingLists(counter: { lists: number }) {
  return (real: Machines): Partial<Machines> => ({
    listWorkers: () => {
      counter.lists++
      return real.listWorkers()
    },
  })
}

/** The Claude Code item of the worker with that name. */
function item(worker: string): HTMLElement {
  const list = screen.getByRole("list", { name: `Agents on ${worker}` })
  return within(list).getByRole("listitem")
}
const claude = (worker: string) => within(item(worker))
/** The worker's agents: the list, and the time of an offline worker's report under it. */
const agents = (worker: string) => within(screen.getByRole("list", { name: `Agents on ${worker}` }).parentElement!)
const buttons = (worker: string) => claude(worker).queryAllByRole("button").map((b) => b.textContent)
/** What the item's live region says about a wish the worker hasn't acted on. */
const announced = (worker: string) => item(worker).querySelector("[aria-live].sr-only")?.textContent
/** The visible wish line, hidden from assistive technology (the live region says it). */
const wishLine = (worker: string) =>
  [...item(worker).querySelectorAll("span[aria-hidden='true']")].map((e) => e.textContent).find((t) => t) ?? null
const spinners = (worker: string) => item(worker).querySelectorAll("[data-slot=spinner]").length
const state = (worker: string) => item(worker).querySelector("[aria-live]:not(.sr-only)")?.textContent

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(NOW)
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  await i18n.changeLanguage("en")
})

describe("a worker's agents", () => {
  it("show every state the worker reports, with what can be done", async () => {
    await renderDevices(agentsDemo())
    expect(state("devbox")).toBe("Installed")
    expect(claude("devbox").getByText("2.1.3")).toBeTruthy()
    expect(buttons("devbox")).toEqual(["Uninstall"])
    expect(claude("devbox").getByRole("button").getAttribute("data-variant")).toBe("ghost")

    expect(state("spare-box")).toBe("Not installed")
    expect(buttons("spare-box")).toEqual(["Install"])
    expect(claude("spare-box").getByRole("button").getAttribute("data-variant")).toBe("outline")

    expect(state("ci-runner-1")).toBe("Installing")
    expect(claude("ci-runner-1").getByText("2.1.3")).toBeTruthy()
    expect(spinners("ci-runner-1")).toBe(1)
    const known = claude("ci-runner-1").getByRole("progressbar", { name: "Downloading Claude Code" })
    expect(known.getAttribute("aria-valuenow")).toBe("41")
    expect(known.getAttribute("aria-valuetext")).toBe("31.2 MB of 74.5 MB (41%)")
    expect(claude("ci-runner-1").getByText("31.2 MB of 74.5 MB (41%)")).toBeTruthy()
    expect(buttons("ci-runner-1")).toEqual([])

    const unknown = claude("ci-runner-2").getByRole("progressbar")
    expect(unknown.getAttribute("aria-valuenow")).toBeNull()
    expect(unknown.getAttribute("aria-valuetext")).toBe("12.8 MB downloaded")

    expect(state("build-mac")).toBe("Outdated")
    expect(
      claude("build-mac").getByText(
        "2.0.9 is installed, but this worker runs 2.1.3. Sessions don't run on it until it's updated.",
      ),
    ).toBeTruthy()
    expect(buttons("build-mac")).toEqual(["Update", "Uninstall"])

    expect(state("lab-pi")).toBe("Failed")
    expect(claude("lab-pi").getByText(/^download failed: GET https:/)).toBeTruthy()
    expect(buttons("lab-pi")).toEqual(["Retry", "Don't install"])
    expect(claude("lab-pi").getAllByRole("button").map((b) => b.getAttribute("data-variant"))).toEqual(["outline", "ghost"])

    expect(state("old-laptop")).toBe("Uninstalling")
    expect(buttons("old-laptop")).toEqual([])

    // Never connected: nothing reported, and an install would be refused.
    expect(claude("fresh-worker").getByText("Unknown")).toBeTruthy()
    expect(claude("fresh-worker").getByText("The worker hasn't reported this agent yet.")).toBeTruthy()
    expect(buttons("fresh-worker")).toEqual([])
    expect(item("fresh-worker").getAttribute("data-agent-state")).toBe("unknown")

    // Online and reported without it: its build doesn't support it.
    expect(claude("legacy-runner").getByText("Not supported")).toBeTruthy()
    expect(claude("legacy-runner").getByText("This worker's version doesn't support Claude Code.")).toBeTruthy()
    expect(buttons("legacy-runner")).toEqual([])
    expect(item("legacy-runner").getAttribute("data-agent-state")).toBe("unsupported")
  })

  it("show an offline worker's last report and its time once, and the wish it will act on when it connects", async () => {
    const demo = agentsDemo()
    // Between two minutes: the time still reads in full hours and days.
    vi.setSystemTime(NOW + 30_000)
    await renderDevices(demo)
    expect(state("travel-mbp")).toBe("Installed")
    expect(announced("travel-mbp")).toBe("Will uninstall when the worker connects.")
    expect(wishLine("travel-mbp")).toBe("Will uninstall when the worker connects.")
    expect(agents("travel-mbp").getAllByText("As reported 3h ago")).toHaveLength(1)
    expect(buttons("travel-mbp")).toEqual(["Keep installed"])

    expect(state("nas")).toBe("Not installed")
    expect(announced("nas")).toBe("Will install when the worker connects.")
    expect(agents("nas").getByText("As reported 5d ago")).toBeTruthy()
    expect(buttons("nas")).toEqual(["Cancel install"])

    // A failed install still wanted is installed again when the worker connects.
    expect(state("kiosk")).toBe("Failed")
    expect(claude("kiosk").getByText("no space left on device")).toBeTruthy()
    expect(announced("kiosk")).toBe("Will install when the worker connects.")
    expect(buttons("kiosk")).toEqual(["Don't install"])

    expect(claude("field-laptop").getByText("The worker went offline during this. It reports again when it connects.")).toBeTruthy()
    // An online worker's report is current: no time, and no wish.
    expect(agents("devbox").queryByText(/As reported/)).toBeNull()
    expect(announced("devbox")).toBe("")
  })

  it("say an offline worker was busy with the agent when it went away, without showing it as under way", async () => {
    const demo = seedState(NOW)
    demo.workers.find((w) => w.name === "eric-win")!.agents = [
      mockAgent(NOW - DAY, { state: "installing", installedVersion: "", downloadedBytes: 1_000_000, totalBytes: 10_000_000 }),
    ]
    const counter = { lists: 0 }
    await renderDevices(demo, countingLists(counter))
    expect(state("eric-win")).toBe("Installing")
    expect(claude("eric-win").getByText("The worker went offline during this. It reports again when it connects.")).toBeTruthy()
    expect(spinners("eric-win")).toBe(0)
    expect(claude("eric-win").getByRole("progressbar").getAttribute("aria-valuenow")).toBe("10")
    // An old report is no reason to poll every second.
    const before = counter.lists
    await advance(60_000)
    expect(counter.lists - before).toBe(60_000 / DEVICES_LIVE_INTERVAL_MS)
  })

  it("offer the install for a state this client doesn't know, without waiting for the worker", async () => {
    const demo = seedState(NOW)
    demo.workers.find((w) => w.name === "devbox")!.agents = [mockAgent(NOW, { state: "unknown" })]
    const counter = { lists: 0 }
    await renderDevices(demo, countingLists(counter))
    expect(state("devbox")).toBe("Unknown")
    expect(buttons("devbox")).toEqual(["Install"])
    expect(announced("devbox")).toBe("")
    const before = counter.lists
    await advance(60_000)
    expect(counter.lists - before).toBe(60_000 / DEVICES_LIVE_INTERVAL_MS)
  })

  it("install, following the download every second, then go back to the usual pace", async () => {
    const counter = { lists: 0 }
    await renderDevices(agentsDemo(), countingLists(counter))
    // The seeded downloads and the uninstall run their course first.
    await advance(20_000)
    fireEvent.click(claude("spare-box").getByRole("button", { name: "Install Claude Code on spare-box" }))
    await advance(10)
    expect(state("spare-box")).toBe("Installing")
    expect(claude("spare-box").getByText("0 kB of 74.5 MB (0%)")).toBeTruthy()
    // The worker reports once a second; the list follows at that pace.
    await advance(1_000)
    expect(claude("spare-box").getByText("9 MB of 74.5 MB (12%)")).toBeTruthy()
    await advance(1_000)
    expect(claude("spare-box").getByText("18 MB of 74.5 MB (24%)")).toBeTruthy()
    await advance(8_000)
    expect(state("spare-box")).toBe("Installed")
    expect(buttons("spare-box")).toEqual(["Uninstall"])
    const before = counter.lists
    await advance(3 * DEVICES_LIVE_INTERVAL_MS)
    expect(counter.lists - before).toBe(3)
  })

  it("follow a worker that doesn't act on a wish every second for a minute, then every few seconds", async () => {
    const demo = seedState(NOW)
    // Online and wanted, never acted on: the mock's workers act only on new requests.
    demo.workers.find((w) => w.name === "devbox")!.agents = [
      mockAgent(NOW, { state: "not_installed", installedVersion: "" }),
    ]
    const counter = { lists: 0 }
    await renderDevices(demo, countingLists(counter))
    expect(announced("devbox")).toBe("Waiting for the worker to start the install…")
    let before = counter.lists
    await advance(AGENTS_BUSY_FAST_MS)
    expect(counter.lists - before).toBeGreaterThanOrEqual(55)
    before = counter.lists
    await advance(60_000)
    expect(counter.lists - before).toBeLessThanOrEqual(13)
    expect(counter.lists - before).toBeGreaterThanOrEqual(11)
  })

  it("show a retry waiting for the worker, following it every second until it reports", async () => {
    const demo = seedState(NOW)
    demo.workers.find((w) => w.name === "devbox")!.agents = [
      mockAgent(NOW, { state: "failed", installedVersion: "", error: "boom" }),
    ]
    const counter = { lists: 0 }
    await renderDevices(demo, (real) => ({
      ...countingLists(counter)(real),
      // The wish is stored and answered with the report as it was; the
      // worker acts on it three seconds later.
      setWorkerAgent: async (req) => {
        const worker = demo.workers.find((w) => w.workerId === req.workerId)!
        const answer: Worker = { ...worker, agents: [{ ...worker.agents[0], wanted: true }] }
        setTimeout(() => void real.setWorkerAgent(req), 3_000)
        return answer
      },
    }))
    fireEvent.click(claude("devbox").getByRole("button", { name: "Install Claude Code on devbox again" }))
    await advance(10)
    expect(announced("devbox")).toBe("Waiting for the worker to start the install…")
    expect(buttons("devbox")).toEqual(["Don't install"])
    const before = counter.lists
    await advance(2_000)
    expect(counter.lists - before).toBeGreaterThanOrEqual(2)
    expect(state("devbox")).toBe("Failed")
    await advance(1_500)
    expect(state("devbox")).toBe("Installing")
    expect(claude("devbox").queryByText("boom")).toBeNull()
  })

  it("update an outdated install and retry a failed one", async () => {
    await renderDevices(agentsDemo())
    fireEvent.click(claude("build-mac").getByRole("button", { name: "Update Claude Code on build-mac" }))
    fireEvent.click(claude("lab-pi").getByRole("button", { name: "Install Claude Code on lab-pi again" }))
    await advance(10)
    expect(state("build-mac")).toBe("Installing")
    expect(state("lab-pi")).toBe("Installing")
    expect(claude("lab-pi").queryByText(/download failed/)).toBeNull()
    await advance(10_000)
    expect(state("build-mac")).toBe("Installed")
    expect(state("lab-pi")).toBe("Installed")
  })

  it("give up on a failed install without a confirmation, showing it as not installed from then on", async () => {
    const demo = agentsDemo()
    const { unmount } = await renderDevices(demo)
    fireEvent.click(claude("lab-pi").getByRole("button", { name: "Don't install Claude Code on lab-pi" }))
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(demo.workers.find((w) => w.name === "lab-pi")!.agents[0].wanted).toBe(false)
    expect(state("lab-pi")).toBe("Not installed")
    expect(claude("lab-pi").queryByText(/download failed/)).toBeNull()
    expect(buttons("lab-pi")).toEqual(["Install"])
    // Long after, with the change forgotten: the worker still reports the
    // old failure, and the row still shows it as not installed.
    unmount()
    await advance(10 * 60_000)
    await renderDevices(demo)
    expect(demo.workers.find((w) => w.name === "lab-pi")!.agents[0].state).toBe("failed")
    expect(state("lab-pi")).toBe("Not installed")
    expect(claude("lab-pi").queryByText(/download failed/)).toBeNull()
  })

  it("take a retry answered with the worker's new failure as that failure once the wait is over", async () => {
    const demo = seedState(NOW)
    demo.workers.find((w) => w.name === "devbox")!.agents = [
      mockAgent(NOW, { state: "failed", installedVersion: "", error: "boom" }),
    ]
    const counter = { lists: 0 }
    await renderDevices(demo, (real) => ({
      ...countingLists(counter)(real),
      // The worker failed again before the answer was made: the answer
      // carries the new report, and the worker reports nothing more.
      setWorkerAgent: async (req) => {
        const worker = demo.workers.find((w) => w.workerId === req.workerId)!
        Object.assign(worker.agents[0], { wanted: true, error: "boom again", reportedAtUnixMs: Date.now() })
        return { ...worker, agents: [{ ...worker.agents[0] }] }
      },
    }))
    fireEvent.click(claude("devbox").getByRole("button", { name: "Install Claude Code on devbox again" }))
    await advance(10)
    expect(announced("devbox")).toBe("Waiting for the worker to start the install…")
    await advance(AGENT_ANSWER_WAIT_MS)
    expect(announced("devbox")).toBe("")
    expect(claude("devbox").getByText("boom again")).toBeTruthy()
    expect(buttons("devbox")).toEqual(["Retry", "Don't install"])
    // And the page is back to the usual pace.
    await advance(DEVICES_LIVE_INTERVAL_MS)
    const before = counter.lists
    await advance(3 * DEVICES_LIVE_INTERVAL_MS)
    expect(counter.lists - before).toBe(3)
  })

  it("ignore the second click of a double click, which would land on the action the first one's answer put there", async () => {
    const requests: boolean[] = []
    await renderDevices(agentsDemo(), (real) => ({
      setWorkerAgent: (req) => {
        requests.push(req.install)
        return real.setWorkerAgent(req)
      },
    }))
    const place = () => claude("nas").getByRole("button")
    fireEvent.click(place(), { detail: 1 })
    await advance(10)
    expect(place().textContent).toBe("Install")
    fireEvent.click(place(), { detail: 2 })
    await advance(10)
    expect(requests).toEqual([false])
    expect(place().textContent).toBe("Install")
    // A click of its own is taken.
    fireEvent.click(place(), { detail: 1 })
    await advance(10)
    expect(requests).toEqual([false, true])
  })

  it("say a worker that connected before and reported no entry doesn't support the agent, even offline", async () => {
    const demo = seedState(NOW)
    demo.workers.find((w) => w.name === "eric-win")!.agents = []
    await renderDevices(demo)
    expect(claude("eric-win").getByText("Not supported")).toBeTruthy()
    expect(item("eric-win").getAttribute("data-agent-state")).toBe("unsupported")
    expect(buttons("eric-win")).toEqual([])
  })

  it("move the bar of a download of unknown size only while the worker is online", async () => {
    const demo = agentsDemo()
    demo.workers.find((w) => w.name === "eric-win")!.agents = [
      mockAgent(NOW - DAY, { state: "installing", installedVersion: "", downloadedBytes: 1_000_000 }),
    ]
    await renderDevices(demo)
    const bar = (worker: string) => claude(worker).getByRole("progressbar").firstElementChild!
    expect(bar("ci-runner-2").classList.contains("animate-pulse")).toBe(true)
    expect(bar("eric-win").classList.contains("animate-pulse")).toBe(false)
  })

  it("follow every second again for a minute after a new request, while still busy", async () => {
    const demo = agentsDemo()
    // devbox never acts on its wish: busy all along.
    demo.workers.find((w) => w.name === "devbox")!.agents = [mockAgent(NOW, { state: "not_installed", installedVersion: "" })]
    const counter = { lists: 0 }
    await renderDevices(demo, countingLists(counter))
    await advance(AGENTS_BUSY_FAST_MS + 20_000)
    let before = counter.lists
    await advance(10_000)
    expect(counter.lists - before).toBeLessThanOrEqual(3)
    // A request on another worker.
    fireEvent.click(claude("nas").getByRole("button", { name: "Cancel installing Claude Code on nas" }))
    await advance(10)
    before = counter.lists
    await advance(10_000)
    expect(counter.lists - before).toBeGreaterThanOrEqual(9)
  })

  it("follow every second again when the page gets busy anew after a quiet spell", async () => {
    const demo = seedState(NOW)
    const devbox = demo.workers.find((w) => w.name === "devbox")!
    devbox.agents = [mockAgent(NOW, { state: "not_installed", installedVersion: "" })]
    const counter = { lists: 0 }
    await renderDevices(demo, countingLists(counter))
    await advance(AGENTS_BUSY_FAST_MS + 20_000)
    // The worker installs it (reported by itself), then a reconnect brings a new download.
    devbox.agents = [mockAgent(Date.now())]
    await advance(3 * DEVICES_LIVE_INTERVAL_MS)
    devbox.agents = [mockAgent(Date.now(), { state: "installing", installedVersion: "", totalBytes: 10 })]
    await advance(DEVICES_LIVE_INTERVAL_MS)
    const before = counter.lists
    await advance(10_000)
    expect(counter.lists - before).toBeGreaterThanOrEqual(9)
  })

  it("hide the outdated hint while an update waits for the worker", async () => {
    const demo = agentsDemo()
    Object.assign(demo.workers.find((w) => w.name === "build-mac")!, { online: false, lastSeenUnixMs: NOW - DAY })
    demo.workers.find((w) => w.name === "build-mac")!.agents[0].wantedVersion = "2.1.3"
    await renderDevices(demo)
    expect(state("build-mac")).toBe("Outdated")
    expect(claude("build-mac").queryByText(/is installed, but this worker runs/)).toBeNull()
    expect(announced("build-mac")).toBe("Will install when the worker connects.")
    expect(buttons("build-mac")).toEqual(["Uninstall"])
  })

  it("uninstall only once confirmed, saying what is removed and what stays", async () => {
    const demo = agentsDemo()
    await renderDevices(demo)
    fireEvent.click(claude("build-mac").getByRole("button", { name: "Uninstall Claude Code from build-mac" }))
    await advance(10)
    let dialog = screen.getByRole("dialog", { name: "Uninstall Claude Code from build-mac?" })
    expect(
      within(dialog).getByText(/removes Claude Code's files from that machine\. Your own Claude login and settings there stay\./),
    ).toBeTruthy()
    fireEvent.click(within(dialog).getByRole("button", { name: "Cancel" }))
    await advance(500)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(demo.workers.find((w) => w.name === "build-mac")!.agents[0].wanted).toBe(true)

    fireEvent.click(claude("build-mac").getByRole("button", { name: "Uninstall Claude Code from build-mac" }))
    await advance(10)
    dialog = screen.getByRole("dialog", { name: "Uninstall Claude Code from build-mac?" })
    fireEvent.click(within(dialog).getByRole("button", { name: "Uninstall" }))
    await advance(10)
    expect(state("build-mac")).toBe("Uninstalling")
    await advance(3_000)
    expect(state("build-mac")).toBe("Not installed")
    expect(buttons("build-mac")).toEqual(["Install"])
  })

  it("let the user change an offline worker's wish either way", async () => {
    await renderDevices(agentsDemo())
    fireEvent.click(claude("nas").getByRole("button", { name: "Cancel installing Claude Code on nas" }))
    await advance(10)
    expect(announced("nas")).toBe("")
    expect(buttons("nas")).toEqual(["Install"])
    fireEvent.click(claude("nas").getByRole("button", { name: "Install Claude Code on nas" }))
    await advance(10)
    expect(announced("nas")).toBe("Will install when the worker connects.")

    // An uninstall confirmed on the wrong machine can be taken back.
    fireEvent.click(claude("travel-mbp").getByRole("button", { name: "Keep Claude Code installed on travel-mbp" }))
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(announced("travel-mbp")).toBe("")
    expect(buttons("travel-mbp")).toEqual(["Uninstall"])
  })

  it("send one request for two clicks in a row", async () => {
    let requests = 0
    await renderDevices(agentsDemo(), (real) => ({
      setWorkerAgent: (req) => {
        requests++
        return real.setWorkerAgent(req)
      },
    }))
    const install = claude("spare-box").getByRole("button", { name: "Install Claude Code on spare-box" })
    fireEvent.click(install)
    fireEvent.click(install)
    await advance(10)
    expect(requests).toBe(1)
  })

  it("open no confirmation while another request of the agent is under way", async () => {
    await renderDevices(agentsDemo(), () => ({ setWorkerAgent: () => new Promise<never>(() => {}) }))
    fireEvent.click(claude("build-mac").getByRole("button", { name: "Update Claude Code on build-mac" }))
    await advance(10)
    fireEvent.click(claude("build-mac").getByRole("button", { name: "Uninstall Claude Code from build-mac" }))
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()
  })

  it("keep keyboard focus on the pressed button while its request is under way", async () => {
    let answer: () => void = () => {}
    await renderDevices(agentsDemo(), (real) => ({
      setWorkerAgent: async (req) => {
        await new Promise<void>((resolve) => (answer = resolve))
        return real.setWorkerAgent(req)
      },
    }))
    const button = claude("nas").getByRole("button", { name: "Cancel installing Claude Code on nas" })
    button.focus()
    fireEvent.click(button)
    await advance(10)
    expect(button.getAttribute("aria-busy")).toBe("true")
    expect(button.hasAttribute("disabled")).toBe(false)
    expect(document.activeElement).toBe(button)
    answer()
    await advance(10)
    expect(document.activeElement).toBe(claude("nas").getByRole("button", { name: "Install Claude Code on nas" }))
  })

  it("keep keyboard focus in the agent's item when its buttons change or go", async () => {
    await renderDevices(agentsDemo())
    // Install: the worker starts at once, and the item has no button left.
    let button = claude("spare-box").getByRole("button", { name: "Install Claude Code on spare-box" })
    button.focus()
    fireEvent.click(button)
    await advance(10)
    expect(document.activeElement).toBe(item("spare-box"))

    // Cancel install on an offline worker: the same place now holds Install.
    button = claude("nas").getByRole("button", { name: "Cancel installing Claude Code on nas" })
    button.focus()
    fireEvent.click(button)
    await advance(10)
    expect(document.activeElement).toBe(claude("nas").getByRole("button", { name: "Install Claude Code on nas" }))

    // Uninstall, through the dialog.
    button = claude("build-mac").getByRole("button", { name: "Uninstall Claude Code from build-mac" })
    button.focus()
    fireEvent.click(button)
    await advance(10)
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Uninstall" }))
    await advance(500)
    expect(item("build-mac").contains(document.activeElement)).toBe(true)
  })

  it("explain an install refused because the worker never reported", async () => {
    await renderDevices(agentsDemo(), () => ({
      setWorkerAgent: () => Promise.reject(new ApiError("failed_precondition", "never reported")),
    }))
    fireEvent.click(claude("spare-box").getByRole("button", { name: "Install Claude Code on spare-box" }))
    await advance(10)
    expect(claude("spare-box").getByRole("alert").textContent).toBe(
      "This worker hasn't reported its agents yet. Try again once it has connected.",
    )
  })

  it("explain an install refused because the worker doesn't support the agent", async () => {
    await renderDevices(agentsDemo(), () => ({
      setWorkerAgent: () => Promise.reject(new ApiError("invalid_argument", "unsupported", "agent")),
    }))
    fireEvent.click(claude("spare-box").getByRole("button", { name: "Install Claude Code on spare-box" }))
    await advance(10)
    expect(claude("spare-box").getByRole("alert").textContent).toBe("This worker doesn't support Claude Code.")
  })

  it("explain an uninstall refused because a session holds the worker", async () => {
    const demo = seedState(NOW)
    addQueueDemo(demo, NOW)
    await renderDevices(demo)
    fireEvent.click(claude("devbox").getByRole("button", { name: "Uninstall Claude Code from devbox" }))
    await advance(10)
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Uninstall" }))
    await advance(10)
    expect(claude("devbox").getByRole("alert").textContent).toBe(
      "A session that runs Claude Code is on this worker. Uninstall it once that session no longer runs there.",
    )
    expect(state("devbox")).toBe("Installed")
  })

  it("report a worker that is gone with a notice, not in its item", async () => {
    await renderDevices(agentsDemo(), () => ({
      setWorkerAgent: () => Promise.reject(new ApiError("not_found", "gone")),
    }))
    fireEvent.click(claude("spare-box").getByRole("button", { name: "Install Claude Code on spare-box" }))
    await advance(10)
    expect(claude("spare-box").queryByRole("alert")).toBeNull()
    expect(screen.getByText("Worker spare-box no longer exists")).toBeTruthy()
    expect(screen.getByText("Its agents weren't changed.")).toBeTruthy()
  })

  it.each([
    ["an install", "spare-box", "Install Claude Code on spare-box"],
    ["a give-up", "lab-pi", "Don't install Claude Code on lab-pi"],
  ])("drop the failure of %s whose answer was lost once the list shows it applied", async (_, worker, name) => {
    await renderDevices(agentsDemo(), (real) => ({
      // Applied, but the answer never arrives.
      setWorkerAgent: async (req) => {
        await real.setWorkerAgent(req)
        throw new ApiError("unavailable", "lost")
      },
    }))
    fireEvent.click(claude(worker).getByRole("button", { name }))
    await advance(10)
    // The failure shows only until a list read says otherwise.
    await advance(1_000)
    expect(claude(worker).queryByRole("alert")).toBeNull()
  })

  it("mark the request busy, hold the other buttons, and say why it failed", async () => {
    let fail: (error: unknown) => void = () => {}
    await renderDevices(agentsDemo(), () => ({
      setWorkerAgent: () => new Promise((_, reject) => (fail = reject)),
    }))
    fireEvent.click(claude("lab-pi").getByRole("button", { name: "Install Claude Code on lab-pi again" }))
    await advance(10)
    const retry = claude("lab-pi").getByRole("button", { name: "Install Claude Code on lab-pi again" })
    const dismiss = claude("lab-pi").getByRole("button", { name: "Don't install Claude Code on lab-pi" })
    expect(retry.getAttribute("aria-busy")).toBe("true")
    expect(dismiss.getAttribute("aria-busy")).toBe("false")
    expect(retry.querySelector("[data-slot=spinner]")).toBeTruthy()
    expect(dismiss.querySelector("[data-slot=spinner]")).toBeNull()
    // Held with aria-disabled, never `disabled`: a browser moves focus off a
    // button that becomes disabled (happy-dom doesn't, so this checks the cause).
    for (const b of [retry, dismiss]) {
      expect(b.hasAttribute("disabled")).toBe(false)
      expect(b.getAttribute("aria-disabled")).toBe("true")
    }
    fireEvent.click(dismiss)
    await advance(10)
    expect(dismiss.getAttribute("aria-busy")).toBe("false")
    fail(new ApiError("unavailable", "down"))
    await advance(10)
    expect(claude("lab-pi").getByRole("alert").textContent).toBe(i18n.t("errors.unreachable"))
    expect(retry.getAttribute("aria-disabled")).toBe("false")
  })

  it("read in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    vi.setSystemTime(NOW + 30_000)
    await renderDevices(agentsDemo())
    const list = (worker: string) => screen.getByRole("list", { name: `${worker} 上的 agent` })
    const zh = (worker: string) => within(within(list(worker)).getByRole("listitem"))
    expect(zh("devbox").getByText("已安装")).toBeTruthy()
    expect(zh("ci-runner-1").getByText("31.2 MB / 74.5 MB（41%）")).toBeTruthy()
    expect(zh("ci-runner-2").getByText("已下载 12.8 MB")).toBeTruthy()
    expect(zh("build-mac").getByText("已安装 2.0.9，但这个 worker 运行的是 2.1.3。更新之前，会话不会在它上面运行。")).toBeTruthy()
    expect(zh("nas").getAllByText("worker 连接后会安装。")).toHaveLength(2)
    expect(within(list("nas").parentElement!).getByText("5 天前报告的状态")).toBeTruthy()
    expect(zh("travel-mbp").getByRole("button", { name: "在 travel-mbp 上保留 Claude Code" }).textContent).toBe("保留安装")
    expect(zh("lab-pi").getByRole("button", { name: "不在 lab-pi 上安装 Claude Code" }).textContent).toBe("不安装")
    expect(zh("legacy-runner").getByText("这个 worker 的版本不支持 Claude Code。")).toBeTruthy()
  })
})
