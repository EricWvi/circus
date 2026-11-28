// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen, within } from "@testing-library/react"
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

/** The worker's list of agents. */
const list = (worker: string) => screen.getByRole("list", { name: i18n.t("devices.agents.listLabel", { worker }) })
/** The Claude Code item of the worker with that name. */
const item = (worker: string) => list(worker).querySelector<HTMLElement>("li[data-agent=claude_code]")!
const claude = (worker: string) => within(item(worker))
/** The worker's agents: the list, and the refusals and the time of an offline worker's report under it. */
const agents = (worker: string) => within(list(worker).parentElement!)
/** The item's name: the agent, its state and its version. */
const label = (worker: string) => document.getElementById(item(worker).getAttribute("aria-labelledby")!)!.textContent
const state = (worker: string) => item(worker).getAttribute("data-agent-state")
/** The chip, when it is a button (it opens the agent's dialog, or its tooltip); null when it isn't. */
const chip = (worker: string) => item(worker).querySelector<HTMLButtonElement>("button:not([data-slot=button])")
/** What doesn't fit in the chip, as its description (its tooltip says the same). */
const details = (worker: string) => {
  // The first id is the chip's own; an open tooltip adds its own after it.
  const id = chip(worker)?.getAttribute("aria-describedby")?.split(" ")[0]
  return id ? document.getElementById(id)!.textContent : null
}
/** What the chip shows. */
const shown = (worker: string) => item(worker).querySelector("[data-agent-mark]")!.parentElement!.textContent
/** The actions beside the chip, which come out on hover or focus. */
const side = (worker: string) => claude(worker).queryAllByRole("button").filter((b) => b.dataset.slot === "button")
const sideNames = (worker: string) => side(worker).map((b) => b.textContent)
/** What the item's live region says about a wish the worker hasn't acted on. */
const announced = (worker: string) => item(worker).querySelectorAll(":scope > [aria-live]")[1]?.textContent
const spinners = (worker: string) => item(worker).querySelectorAll("[data-slot=spinner]").length
const dialog = () => within(screen.getByRole("dialog"))
/** Presses the chip, which opens its dialog. */
async function openChip(worker: string) {
  fireEvent.click(chip(worker)!, { detail: 1 })
  await advance(10)
  return dialog()
}
const footer = () => dialog().getAllByRole("button").map((b) => b.textContent)

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
  it("show every state the worker reports as a chip, with what can be done", async () => {
    await renderDevices(agentsDemo())
    // Installed: the version, and Uninstall beside it; nothing to open.
    expect(label("devbox")).toBe("Claude Code, installed, 2.1.3")
    expect(shown("devbox")).toBe("Claude Code2.1.3")
    expect(chip("devbox")).toBeNull()
    expect(sideNames("devbox")).toEqual(["Uninstall"])
    expect(side("devbox")[0].getAttribute("aria-label")).toBe("Uninstall Claude Code from devbox")

    // Not installed: dashed, the name only, and pressing it asks to install.
    expect(label("spare-box")).toBe("Claude Code, not installed")
    expect(shown("spare-box")).toBe("Claude Code")
    expect(chip("spare-box")!.getAttribute("aria-label")).toBe("Claude Code, not installed")
    expect(chip("spare-box")!.getAttribute("aria-haspopup")).toBe("dialog")
    expect(chip("spare-box")!.className).toContain("border-dashed")
    expect(sideNames("spare-box")).toEqual([])

    expect(label("ci-runner-1")).toBe("Claude Code, installing, 2.1.3")
    expect(shown("ci-runner-1")).toBe("Claude Code41%")
    expect(details("ci-runner-1")).toBe("31.2 MB of 74.5 MB (41%)")
    expect(spinners("ci-runner-1")).toBe(1)
    const known = claude("ci-runner-1").getByRole("progressbar", { name: "Downloading Claude Code" })
    expect(known.getAttribute("aria-valuenow")).toBe("41")
    expect(known.getAttribute("aria-valuetext")).toBe("31.2 MB of 74.5 MB (41%)")
    expect(sideNames("ci-runner-1")).toEqual([])

    // A download of unknown size: the bytes so far.
    expect(shown("ci-runner-2")).toBe("Claude Code12.8 MB")
    const unknown = claude("ci-runner-2").getByRole("progressbar")
    expect(unknown.getAttribute("aria-valuenow")).toBeNull()
    expect(unknown.getAttribute("aria-valuetext")).toBe("12.8 MB downloaded")

    expect(label("build-mac")).toBe("Claude Code, outdated, 2.0.9")
    expect(details("build-mac")).toBe(
      "2.0.9 is installed, but this worker runs 2.1.3. Sessions don't run on it until it's updated.",
    )
    expect(sideNames("build-mac")).toEqual(["Update", "Uninstall"])
    // Update stays out, and a warning mark says outdated without colour;
    // only Uninstall folds away until hover or focus.
    const fold = (b: HTMLElement) => b.closest(".group-hover\\/chip\\:grid-cols-\\[minmax\\(0\\,1fr\\)\\]")
    expect(side("build-mac").map((b) => fold(b) !== null)).toEqual([false, true])
    expect(item("build-mac").querySelector("svg.lucide-triangle-alert")?.getAttribute("aria-hidden")).toBe("true")
    expect(item("devbox").querySelector("svg.lucide-triangle-alert")).toBeNull()
    expect(side("devbox").map((b) => fold(b) !== null)).toEqual([true])

    // Failed: a dashed chip in the destructive colour; the error is in its dialog.
    expect(label("lab-pi")).toBe("Claude Code, install failed")
    expect(shown("lab-pi")).toBe("Claude CodeInstall failed")
    expect(chip("lab-pi")!.className).toContain("border-destructive")
    expect(claude("lab-pi").queryByText(/download failed/)).toBeNull()
    expect(sideNames("lab-pi")).toEqual([])

    expect(label("old-laptop")).toBe("Claude Code, uninstalling, 2.1.3")
    expect(spinners("old-laptop")).toBe(1)
    expect(sideNames("old-laptop")).toEqual([])

    // Never connected: nothing reported, and an install would be refused.
    expect(state("fresh-worker")).toBe("unknown")
    expect(screen.getByRole("button", { name: "Claude Code, unknown" }).getAttribute("aria-haspopup")).toBeNull()
    expect(details("fresh-worker")).toBe("The worker hasn't reported this agent yet.")
    expect(sideNames("fresh-worker")).toEqual([])

    // Online and reported without it: its build doesn't support it.
    expect(state("legacy-runner")).toBe("unsupported")
    expect(chip("legacy-runner")!.getAttribute("aria-label")).toBe("Claude Code, not supported")
    expect(details("legacy-runner")).toBe("This worker's version doesn't support Claude Code.")
  })

  it("list Codex and opencode after Claude Code, as coming, with nothing to press", async () => {
    await renderDevices(agentsDemo())
    for (const worker of ["devbox", "spare-box", "eric-win"]) {
      const items = within(list(worker)).getAllByRole("listitem")
      expect(items.map((li) => li.getAttribute("data-agent"))).toEqual(["claude_code", "codex", "opencode"])
      expect(items.map((li) => li.querySelector("[data-agent-mark]")?.getAttribute("data-agent-mark"))).toEqual([
        "claude_code",
        "codex",
        "opencode",
      ])
      for (const later of items.slice(1)) {
        expect(within(later).queryByRole("button")).toBeNull()
        expect(later.querySelector(".sr-only")!.textContent).toMatch(/^(Codex|opencode), coming soon$/)
      }
    }
    // Out of the tab order too: there is nothing to do with them.
    for (const later of within(list("spare-box")).getAllByRole("listitem").slice(1)) {
      expect(later.querySelector("button, a, input, [tabindex]")).toBeNull()
    }
  })

  it("show an offline worker's last report and its time once, and the wish it will act on when it connects", async () => {
    const demo = agentsDemo()
    // Between two minutes: the time still reads in full hours and days.
    vi.setSystemTime(NOW + 30_000)
    await renderDevices(demo)
    expect(label("travel-mbp")).toBe("Claude Code, installed, 2.1.3")
    expect(announced("travel-mbp")).toBe("Will uninstall when the worker connects.")
    expect(details("travel-mbp")).toBe("Will uninstall when the worker connects.")
    // Waiting for the worker to connect: a clock, not a spinner.
    expect(spinners("travel-mbp")).toBe(0)
    expect(item("travel-mbp").querySelector("svg.lucide-clock")).toBeTruthy()
    expect(agents("travel-mbp").getAllByText("As reported 3h ago")).toHaveLength(1)
    expect(sideNames("travel-mbp")).toEqual(["Keep installed"])

    expect(label("nas")).toBe("Claude Code, not installed")
    expect(announced("nas")).toBe("Will install when the worker connects.")
    expect(agents("nas").getByText("As reported 5d ago")).toBeTruthy()
    // Taking the wish back is beside the chip; the chip opens nothing.
    expect(chip("nas")!.getAttribute("aria-haspopup")).toBeNull()
    expect(sideNames("nas")).toEqual(["Cancel install"])

    // A failed install still wanted is installed again when the worker connects.
    expect(label("kiosk")).toBe("Claude Code, install failed")
    expect(announced("kiosk")).toBe("Will install when the worker connects.")
    expect(details("kiosk")).toBe("Will install when the worker connects.")
    const failed = await openChip("kiosk")
    expect(failed.getByRole("heading").textContent).toBe("Claude Code couldn't be installed on kiosk")
    expect(failed.getByText("no space left on device")).toBeTruthy()
    expect(failed.getByText("Will install when the worker connects.")).toBeTruthy()
    expect(footer()).toEqual(["Don't install", "Close"])
    fireEvent.click(failed.getByRole("button", { name: "Close" }))
    await advance(500)

    // Went offline during a download: what it last said, and nothing to do about it.
    expect(details("field-laptop")).toBe(
      "20.1 MB of 74.5 MB (26%) The worker went offline during this. It reports again when it connects.",
    )
    expect(sideNames("field-laptop")).toEqual([])
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
    expect(state("eric-win")).toBe("installing")
    expect(details("eric-win")).toContain("The worker went offline during this. It reports again when it connects.")
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
    expect(label("devbox")).toBe("Claude Code, unknown")
    expect(announced("devbox")).toBe("")
    const install = await openChip("devbox")
    expect(install.getByText("The worker hasn't reported this agent yet.")).toBeTruthy()
    expect(footer()).toEqual(["Cancel", "Install"])
    const before = counter.lists
    await advance(60_000)
    expect(counter.lists - before).toBe(60_000 / DEVICES_LIVE_INTERVAL_MS)
  })

  it("install once confirmed, following the download every second, then go back to the usual pace", async () => {
    const counter = { lists: 0 }
    const demo = agentsDemo()
    await renderDevices(demo, countingLists(counter))
    // The seeded downloads and the uninstall run their course first.
    await advance(20_000)
    const install = await openChip("spare-box")
    expect(install.getByRole("heading").textContent).toBe("Install Claude Code on spare-box?")
    expect(install.getByText("spare-box downloads Claude Code and installs it. Sessions can run on this worker once it's installed.")).toBeTruthy()
    // Online: nothing about waiting for it to connect.
    expect(install.queryByText(/is offline/)).toBeNull()
    fireEvent.click(install.getByRole("button", { name: "Install" }))
    await advance(10)
    expect(label("spare-box")).toBe("Claude Code, installing, 2.1.3")
    expect(details("spare-box")).toBe("0 kB of 74.5 MB (0%)")
    // The worker reports once a second; the list follows at that pace.
    await advance(1_000)
    expect(shown("spare-box")).toBe("Claude Code12%")
    expect(details("spare-box")).toBe("9 MB of 74.5 MB (12%)")
    await advance(1_000)
    expect(details("spare-box")).toBe("18 MB of 74.5 MB (24%)")
    await advance(8_000)
    expect(label("spare-box")).toBe("Claude Code, installed, 2.1.3")
    expect(sideNames("spare-box")).toEqual(["Uninstall"])
    const before = counter.lists
    await advance(3 * DEVICES_LIVE_INTERVAL_MS)
    expect(counter.lists - before).toBe(3)
  })

  it("install nothing when the install is cancelled", async () => {
    const requests: boolean[] = []
    await renderDevices(agentsDemo(), (real) => ({
      setWorkerAgent: (req) => {
        requests.push(req.install)
        return real.setWorkerAgent(req)
      },
    }))
    const install = await openChip("spare-box")
    fireEvent.click(install.getByRole("button", { name: "Cancel" }))
    await advance(500)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(requests).toEqual([])
    expect(label("spare-box")).toBe("Claude Code, not installed")
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
    expect(details("devbox")).toBe("Waiting for the worker to start the install…")
    expect(spinners("devbox")).toBe(1)
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
        const answer: Worker = { ...worker, updateVersion: "", agents: [{ ...worker.agents[0], wanted: true }] }
        setTimeout(() => void real.setWorkerAgent(req), 3_000)
        return answer
      },
    }))
    const failed = await openChip("devbox")
    expect(failed.getByText("boom")).toBeTruthy()
    expect(footer()).toEqual(["Don't install", "Close", "Retry"])
    fireEvent.click(failed.getByRole("button", { name: "Retry" }))
    await advance(10)
    expect(announced("devbox")).toBe("Waiting for the worker to start the install…")
    expect(spinners("devbox")).toBe(1)
    const before = counter.lists
    await advance(2_000)
    expect(counter.lists - before).toBeGreaterThanOrEqual(2)
    expect(state("devbox")).toBe("failed")
    // Its dialog says so too, and offers no second retry meanwhile.
    await openChip("devbox")
    expect(dialog().getByText("Waiting for the worker to start the install…")).toBeTruthy()
    expect(footer()).toEqual(["Don't install", "Close"])
    fireEvent.click(dialog().getByRole("button", { name: "Close" }))
    await advance(1_500)
    expect(state("devbox")).toBe("installing")
  })

  it("update an outdated install beside the chip, and retry a failed one from its dialog", async () => {
    await renderDevices(agentsDemo())
    fireEvent.click(claude("build-mac").getByRole("button", { name: "Update Claude Code on build-mac" }))
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()
    const failed = await openChip("lab-pi")
    expect(failed.getByRole("heading").textContent).toBe("Claude Code couldn't be installed on lab-pi")
    expect(failed.getByText(/^download failed: GET https:/)).toBeTruthy()
    fireEvent.click(failed.getByRole("button", { name: "Retry" }))
    await advance(10)
    expect(state("build-mac")).toBe("installing")
    expect(state("lab-pi")).toBe("installing")
    await advance(10_000)
    expect(label("build-mac")).toBe("Claude Code, installed, 2.1.3")
    expect(label("lab-pi")).toBe("Claude Code, installed, 2.1.3")
  })

  it("give up on a failed install without a confirmation, showing it as not installed from then on", async () => {
    const demo = agentsDemo()
    const { unmount } = await renderDevices(demo)
    fireEvent.click((await openChip("lab-pi")).getByRole("button", { name: "Don't install" }))
    await advance(500)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(demo.workers.find((w) => w.name === "lab-pi")!.agents[0].wanted).toBe(false)
    expect(label("lab-pi")).toBe("Claude Code, not installed")
    // Pressing it now asks to install.
    expect((await openChip("lab-pi")).getByRole("heading").textContent).toBe("Install Claude Code on lab-pi?")
    // Long after, with the change forgotten: the worker still reports the
    // old failure, and the chip still shows it as not installed.
    unmount()
    await advance(10 * 60_000)
    await renderDevices(demo)
    expect(demo.workers.find((w) => w.name === "lab-pi")!.agents[0].state).toBe("failed")
    expect(label("lab-pi")).toBe("Claude Code, not installed")
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
        return { ...worker, updateVersion: "", agents: [{ ...worker.agents[0] }] }
      },
    }))
    fireEvent.click((await openChip("devbox")).getByRole("button", { name: "Retry" }))
    await advance(10)
    expect(announced("devbox")).toBe("Waiting for the worker to start the install…")
    await advance(AGENT_ANSWER_WAIT_MS)
    expect(announced("devbox")).toBe("")
    const failed = await openChip("devbox")
    expect(failed.getByText("boom again")).toBeTruthy()
    expect(footer()).toEqual(["Don't install", "Close", "Retry"])
    fireEvent.click(failed.getByRole("button", { name: "Close" }))
    // And the page is back to the usual pace.
    await advance(DEVICES_LIVE_INTERVAL_MS)
    const before = counter.lists
    await advance(3 * DEVICES_LIVE_INTERVAL_MS)
    expect(counter.lists - before).toBe(3)
  })

  it("ignore the second click of a double click, which would land on what the first one's answer put there", async () => {
    const requests: boolean[] = []
    await renderDevices(agentsDemo(), (real) => ({
      setWorkerAgent: (req) => {
        requests.push(req.install)
        return real.setWorkerAgent(req)
      },
    }))
    fireEvent.click(claude("nas").getByRole("button", { name: "Cancel installing Claude Code on nas" }), { detail: 1 })
    await advance(10)
    // The chip that asks to install is where Cancel install was.
    expect(label("nas")).toBe("Claude Code, not installed")
    fireEvent.click(chip("nas")!, { detail: 2 })
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(requests).toEqual([false])
    // A click of its own is taken.
    fireEvent.click((await openChip("nas")).getByRole("button", { name: "Install" }))
    await advance(10)
    expect(requests).toEqual([false, true])
  })

  it("say a worker that connected before and reported no entry doesn't support the agent, even offline", async () => {
    const demo = seedState(NOW)
    demo.workers.find((w) => w.name === "eric-win")!.agents = []
    await renderDevices(demo)
    expect(state("eric-win")).toBe("unsupported")
    expect(chip("eric-win")!.getAttribute("aria-label")).toBe("Claude Code, not supported")
    expect(sideNames("eric-win")).toEqual([])
    fireEvent.click(chip("eric-win")!)
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()
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
    expect(state("build-mac")).toBe("outdated")
    expect(details("build-mac")).toBe("Will install when the worker connects.")
    expect(announced("build-mac")).toBe("Will install when the worker connects.")
    expect(sideNames("build-mac")).toEqual(["Uninstall"])
  })

  it("uninstall only once confirmed, saying what is removed and what stays", async () => {
    const demo = agentsDemo()
    await renderDevices(demo)
    fireEvent.click(claude("build-mac").getByRole("button", { name: "Uninstall Claude Code from build-mac" }))
    await advance(10)
    let confirm = within(screen.getByRole("dialog", { name: "Uninstall Claude Code from build-mac?" }))
    expect(
      confirm.getByText(/removes Claude Code's files from that machine\. Your own Claude login and settings there stay\./),
    ).toBeTruthy()
    fireEvent.click(confirm.getByRole("button", { name: "Cancel" }))
    await advance(500)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(demo.workers.find((w) => w.name === "build-mac")!.agents[0].wanted).toBe(true)

    fireEvent.click(claude("build-mac").getByRole("button", { name: "Uninstall Claude Code from build-mac" }))
    await advance(10)
    confirm = within(screen.getByRole("dialog", { name: "Uninstall Claude Code from build-mac?" }))
    fireEvent.click(confirm.getByRole("button", { name: "Uninstall" }))
    await advance(10)
    expect(state("build-mac")).toBe("uninstalling")
    await advance(3_000)
    expect(label("build-mac")).toBe("Claude Code, not installed")
    expect(sideNames("build-mac")).toEqual([])
    expect(chip("build-mac")!.getAttribute("aria-haspopup")).toBe("dialog")
  })

  it("keep the uninstall beside the chip in the tab order, folded until hovered or focused, and out where nothing hovers", async () => {
    await renderDevices(agentsDemo())
    const uninstall = claude("devbox").getByRole("button", { name: "Uninstall Claude Code from devbox" })
    // Folded, not hidden: Tab reaches it, which unfolds it.
    expect(uninstall.closest("[hidden], [aria-hidden=true], [inert]")).toBeNull()
    expect(uninstall.getAttribute("tabindex")).toBeNull()
    uninstall.focus()
    expect(document.activeElement).toBe(uninstall)
    const fold = uninstall.parentElement!.parentElement!.parentElement!
    for (const name of [
      "grid-cols-[minmax(0,0fr)]",
      "group-hover/chip:grid-cols-[minmax(0,1fr)]",
      "group-focus-within/chip:grid-cols-[minmax(0,1fr)]",
      "[@media(hover:none)]:grid-cols-[minmax(0,1fr)]",
      "motion-reduce:transition-none",
    ]) {
      expect(fold.classList.contains(name)).toBe(true)
    }
    expect(fold.parentElement!.classList.contains("group/chip")).toBe(true)
  })

  it("let the user change an offline worker's wish either way", async () => {
    await renderDevices(agentsDemo())
    fireEvent.click(claude("nas").getByRole("button", { name: "Cancel installing Claude Code on nas" }))
    await advance(10)
    expect(announced("nas")).toBe("")
    expect(sideNames("nas")).toEqual([])
    const install = await openChip("nas")
    expect(install.getByText("nas is offline: it installs Claude Code once it connects.")).toBeTruthy()
    fireEvent.click(install.getByRole("button", { name: "Install" }))
    await advance(10)
    expect(announced("nas")).toBe("Will install when the worker connects.")

    // An uninstall confirmed on the wrong machine can be taken back.
    fireEvent.click(claude("travel-mbp").getByRole("button", { name: "Keep Claude Code installed on travel-mbp" }))
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(announced("travel-mbp")).toBe("")
    expect(sideNames("travel-mbp")).toEqual(["Uninstall"])
  })

  it("send one request for two clicks in a row", async () => {
    let requests = 0
    await renderDevices(agentsDemo(), (real) => ({
      setWorkerAgent: (req) => {
        requests++
        return real.setWorkerAgent(req)
      },
    }))
    // Both clicks before React renders again: the dialog (which fades out
    // in a browser) still takes the second.
    for (const [worker, name] of [
      ["spare-box", "Install"],
      ["lab-pi", "Retry"],
    ]) {
      const button = (await openChip(worker)).getByRole("button", { name })
      act(() => {
        button.click()
        button.click()
      })
      await advance(500)
    }
    const update = claude("build-mac").getByRole("button", { name: "Update Claude Code on build-mac" })
    fireEvent.click(update)
    fireEvent.click(update)
    await advance(10)
    expect(requests).toBe(3)
  })

  it("open no dialog while another request of the agent is under way", async () => {
    await renderDevices(agentsDemo(), () => ({ setWorkerAgent: () => new Promise<never>(() => {}) }))
    fireEvent.click(claude("build-mac").getByRole("button", { name: "Update Claude Code on build-mac" }))
    await advance(10)
    fireEvent.click(claude("build-mac").getByRole("button", { name: "Uninstall Claude Code from build-mac" }))
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()

    fireEvent.click((await openChip("spare-box")).getByRole("button", { name: "Install" }))
    await advance(500)
    expect(chip("spare-box")!.getAttribute("aria-disabled")).toBe("true")
    expect(spinners("spare-box")).toBe(1)
    fireEvent.click(chip("spare-box")!, { detail: 1 })
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()
  })

  it("keep keyboard focus on the pressed button while its request is under way, then on the chip", async () => {
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
    // Cancel install is gone: the chip, which now asks to install, has focus.
    expect(document.activeElement).toBe(chip("nas"))
    expect(chip("nas")!.getAttribute("aria-label")).toBe("Claude Code, not installed")
  })

  it("return focus to the chip that opened the dialog, or keep it in the item when the chip changed", async () => {
    await renderDevices(agentsDemo())
    // Opened and closed with the keyboard: back on the chip.
    chip("spare-box")!.focus()
    fireEvent.keyDown(chip("spare-box")!, { key: "Enter" })
    fireEvent.click(chip("spare-box")!, { detail: 0 })
    await advance(10)
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" })
    await advance(500)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(document.activeElement).toBe(chip("spare-box"))

    // Install: the chip becomes the download's, which takes focus.
    fireEvent.keyDown(chip("spare-box")!, { key: "Enter" })
    fireEvent.click(chip("spare-box")!, { detail: 0 })
    await advance(10)
    fireEvent.click(dialog().getByRole("button", { name: "Install" }))
    await advance(500)
    expect(label("spare-box")).toBe("Claude Code, installing, 2.1.3")
    expect(document.activeElement).toBe(chip("spare-box"))

    // Uninstall, through the dialog: the item keeps focus.
    const uninstall = claude("build-mac").getByRole("button", { name: "Uninstall Claude Code from build-mac" })
    uninstall.focus()
    fireEvent.click(uninstall)
    await advance(10)
    fireEvent.click(dialog().getByRole("button", { name: "Uninstall" }))
    await advance(500)
    expect(item("build-mac").contains(document.activeElement)).toBe(true)
  })

  it("explain an install refused because the worker never reported", async () => {
    await renderDevices(agentsDemo(), () => ({
      setWorkerAgent: () => Promise.reject(new ApiError("failed_precondition", "never reported")),
    }))
    fireEvent.click((await openChip("spare-box")).getByRole("button", { name: "Install" }))
    await advance(10)
    expect(agents("spare-box").getByRole("alert").textContent).toBe(
      "This worker hasn't reported its agents yet. Try again once it has connected.",
    )
  })

  it("explain an install refused because the worker doesn't support the agent", async () => {
    await renderDevices(agentsDemo(), () => ({
      setWorkerAgent: () => Promise.reject(new ApiError("invalid_argument", "unsupported", "agent")),
    }))
    fireEvent.click((await openChip("spare-box")).getByRole("button", { name: "Install" }))
    await advance(10)
    expect(agents("spare-box").getByRole("alert").textContent).toBe("This worker doesn't support Claude Code.")
  })

  it("explain an uninstall refused because a session holds the worker", async () => {
    const demo = seedState(NOW)
    addQueueDemo(demo, NOW)
    await renderDevices(demo)
    fireEvent.click(claude("devbox").getByRole("button", { name: "Uninstall Claude Code from devbox" }))
    await advance(10)
    fireEvent.click(dialog().getByRole("button", { name: "Uninstall" }))
    await advance(10)
    expect(agents("devbox").getByRole("alert").textContent).toBe(
      "A session that runs Claude Code is on this worker. Uninstall it once that session no longer runs there.",
    )
    expect(label("devbox")).toBe("Claude Code, installed, 2.1.3")
  })

  it("report a worker that is gone with a notice, not under its agents", async () => {
    await renderDevices(agentsDemo(), () => ({
      setWorkerAgent: () => Promise.reject(new ApiError("not_found", "gone")),
    }))
    fireEvent.click((await openChip("spare-box")).getByRole("button", { name: "Install" }))
    await advance(10)
    expect(agents("spare-box").queryByRole("alert")).toBeNull()
    expect(screen.getByText("Worker spare-box no longer exists")).toBeTruthy()
    expect(screen.getByText("Its agents weren't changed.")).toBeTruthy()
  })

  it.each([
    ["an install", "spare-box", "Install"],
    ["a give-up", "lab-pi", "Don't install"],
  ])("drop the failure of %s whose answer was lost once the list shows it applied", async (_, worker, name) => {
    await renderDevices(agentsDemo(), (real) => ({
      // Applied, but the answer never arrives.
      setWorkerAgent: async (req) => {
        await real.setWorkerAgent(req)
        throw new ApiError("unavailable", "lost")
      },
    }))
    fireEvent.click((await openChip(worker)).getByRole("button", { name }))
    await advance(10)
    // The failure shows only until a list read says otherwise.
    await advance(1_000)
    expect(agents(worker).queryByRole("alert")).toBeNull()
  })

  it("mark the request busy, hold the other buttons, and say why it failed", async () => {
    let fail: (error: unknown) => void = () => {}
    await renderDevices(agentsDemo(), () => ({
      setWorkerAgent: () => new Promise((_, reject) => (fail = reject)),
    }))
    fireEvent.click(claude("build-mac").getByRole("button", { name: "Update Claude Code on build-mac" }))
    await advance(10)
    const update = claude("build-mac").getByRole("button", { name: "Update Claude Code on build-mac" })
    const uninstall = claude("build-mac").getByRole("button", { name: "Uninstall Claude Code from build-mac" })
    expect(update.getAttribute("aria-busy")).toBe("true")
    expect(uninstall.getAttribute("aria-busy")).toBe("false")
    expect(spinners("build-mac")).toBe(1)
    // Held with aria-disabled, never `disabled`: a browser moves focus off a
    // button that becomes disabled (happy-dom doesn't, so this checks the cause).
    for (const b of [update, uninstall]) {
      expect(b.hasAttribute("disabled")).toBe(false)
      expect(b.getAttribute("aria-disabled")).toBe("true")
    }
    fail(new ApiError("unavailable", "down"))
    await advance(10)
    expect(agents("build-mac").getByRole("alert").textContent).toBe(i18n.t("errors.unreachable"))
    expect(update.getAttribute("aria-disabled")).toBe("false")
    expect(spinners("build-mac")).toBe(0)
  })

  it("open the failed dialog on Retry, or on Close without it, never on Don't install", async () => {
    await renderDevices(agentsDemo())
    await openChip("lab-pi")
    expect(footer()).toEqual(["Don't install", "Close", "Retry"])
    expect(document.activeElement).toBe(dialog().getByRole("button", { name: "Retry" }))
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" })
    await advance(500)
    // Offline, the retry waits for the worker: no Retry to offer.
    await openChip("kiosk")
    expect(footer()).toEqual(["Don't install", "Close"])
    expect(document.activeElement).toBe(dialog().getByRole("button", { name: "Close" }))
  })

  it("say a failure reported without an error was reported without one", async () => {
    const demo = seedState(NOW)
    demo.workers.find((w) => w.name === "devbox")!.agents = [mockAgent(NOW, { state: "failed", installedVersion: "", error: "" })]
    await renderDevices(demo)
    const failed = await openChip("devbox")
    expect(failed.getByText("The worker reported the failure without saying why.")).toBeTruthy()
    expect(failed.queryByText("The worker reported this error:")).toBeNull()
  })

  it("follow the worker while a dialog is open, and give focus to the chip it became when it closes", async () => {
    const demo = agentsDemo()
    await renderDevices(demo)
    const opener = chip("spare-box")!
    opener.focus()
    fireEvent.keyDown(opener, { key: "Enter" })
    fireEvent.click(opener, { detail: 0 })
    await advance(10)
    expect(dialog().getByRole("button", { name: "Install" })).toBeTruthy()
    // Another tab installs it meanwhile: the worker reports a download.
    demo.workers.find((w) => w.name === "spare-box")!.agents = [
      mockAgent(Date.now(), { state: "installing", installedVersion: "", downloadedBytes: 1_000_000, totalBytes: 10_000_000 }),
    ]
    await advance(DEVICES_LIVE_INTERVAL_MS)
    expect(label("spare-box")).toBe("Claude Code, installing, 2.1.3")
    expect(opener.isConnected).toBe(false)
    // Install no longer applies: the dialog only closes.
    expect(footer()).toEqual(["Close"])
    fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" })
    await advance(500)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(document.activeElement).toBe(chip("spare-box"))
  })

  it("read in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    vi.setSystemTime(NOW + 30_000)
    await renderDevices(agentsDemo())
    expect(screen.getByRole("list", { name: "devbox 上的 agent" })).toBe(list("devbox"))
    expect(label("devbox")).toBe("Claude Code，已安装，2.1.3")
    expect(label("lab-pi")).toBe("Claude Code，安装失败")
    expect(shown("lab-pi")).toBe("Claude Code安装失败")
    expect(details("ci-runner-1")).toBe("31.2 MB / 74.5 MB（41%）")
    expect(details("ci-runner-2")).toBe("已下载 12.8 MB")
    expect(details("build-mac")).toBe("已安装 2.0.9，但这个工作节点运行的是 2.1.3。更新之前，会话不会在它上面运行。")
    expect(details("nas")).toBe("工作节点连接后会安装。")
    expect(agents("nas").getByText("5 天前报告的状态")).toBeTruthy()
    expect(claude("travel-mbp").getByRole("button", { name: "在 travel-mbp 上保留 Claude Code" }).textContent).toBe("保留安装")
    expect(chip("legacy-runner")!.getAttribute("aria-label")).toBe("Claude Code，不支持")
    expect(details("legacy-runner")).toBe("这个工作节点的版本不支持 Claude Code。")
    expect(within(list("devbox")).getByText("Codex，即将支持")).toBeTruthy()
    fireEvent.click(chip("spare-box")!, { detail: 1 })
    await advance(10)
    expect(dialog().getByRole("heading").textContent).toBe("在 spare-box 上安装 Claude Code？")
    expect(footer()).toEqual(["取消", "安装"])
  })
})
