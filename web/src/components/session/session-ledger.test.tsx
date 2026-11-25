// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { NOT_A_DECISION, readyLedger } from "@/api/mock/session-ledger"
import { seedState, type MockState } from "@/api/mock/state"
import { queryKeys } from "@/api/query-client"
import type {
  CircusClient,
  LedgerService,
  RawSessionEvent,
  SessionLedgerEntry,
  SessionService,
} from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import type * as DiffRunner from "@/lib/diff-runner"
import { MAX_DIFF_ROWS } from "@/lib/line-diff"
import { SessionPage } from "@/routes/session-page"
import { advance, renderWithBackend } from "@/test/render"

// The line diff as the drawer runs it; a test may make it give up.
const diffRunner = vi.hoisted(() => ({ givesUp: false }))
vi.mock("@/lib/diff-runner", async (original) => {
  const real = await original<typeof DiffRunner>()
  return {
    diffOffMainThread: (base: string, changeset: string) =>
      diffRunner.givesUp ? Promise.resolve(null) : real.diffOffMainThread(base, changeset),
  }
})

async function renderSession(
  path: string,
  { state = seedState(), wrapClient }: { state?: MockState; wrapClient?: (client: CircusClient) => CircusClient } = {},
) {
  const router = createMemoryRouter(
    [
      {
        path: "/p/:projectId/s/:sessionId",
        element: (
          <SidebarProvider>
            <SessionPage />
          </SidebarProvider>
        ),
      },
      { path: "/p/:projectId", element: <p>project page</p> },
      { path: "/p/:projectId/ledger/:decision", element: <p>ledger page</p> },
    ],
    { initialEntries: [path] },
  )
  const result = await renderWithBackend(<RouterProvider router={router} />, { state, wrapClient })
  await advance(50)
  return { ...result, router }
}

/** A client whose services are the mock's, with some methods replaced. */
const patch =
  ({
    sessions = () => ({}),
    ledger = () => ({}),
  }: {
    sessions?: (mock: SessionService) => Partial<SessionService>
    ledger?: (mock: LedgerService) => Partial<LedgerService>
  }) =>
  (client: CircusClient): CircusClient => ({
    ...client,
    sessions: { ...client.sessions, ...sessions(client.sessions) },
    ledger: { ...client.ledger, ...ledger(client.ledger) },
  })

const entryButton = () => document.querySelector<HTMLButtonElement>("[data-ledger-entry]")
const drawer = () => document.querySelector<HTMLElement>("[data-session-ledger]")!
const rows = () => [...drawer().querySelectorAll<HTMLElement>('[data-slot="ledger-entry"]')]
const row = (text: string) => rows().find((r) => r.textContent?.includes(text))!

async function openDrawer() {
  fireEvent.click(entryButton()!)
  await advance(20)
}

async function openMenuItem(name: string) {
  const trigger = screen.getByRole("button", { name: i18n.t("session.actions") })
  fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false, pointerType: "mouse" })
  await advance(10)
  fireEvent.click(screen.getByRole("menuitem", { name }))
  await advance(10)
}

/** A stream the test writes after the session's recorded events. */
function scripted(mock: SessionService) {
  const control = { push: (_e: RawSessionEvent) => {} }
  const watchSession: SessionService["watchSession"] = async function* (req) {
    const queue: RawSessionEvent[] = []
    let wake = () => {}
    control.push = (e) => {
      queue.push(e)
      wake()
    }
    const abort = new AbortController()
    req.signal.addEventListener("abort", () => abort.abort())
    // The recorded events first, as the mock serves them.
    const recorded = mock.watchSession({ ...req, signal: abort.signal })[Symbol.asyncIterator]()
    const first = await recorded.next()
    if (!first.done) yield first.value
    for (;;) {
      const next = queue.shift()
      if (next) yield next
      else await new Promise<void>((resolve) => (wake = resolve))
    }
  }
  return { control, watchSession }
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
})
afterEach(async () => {
  diffRunner.givesUp = false
  cleanup()
  vi.useRealTimers()
  await i18n.changeLanguage("en")
})

describe("the header entry", () => {
  it("counts the changes", async () => {
    await renderSession("/p/circus/s/web-vite")
    expect(entryButton()?.dataset.ledgerEntry).toBe("ready")
    expect(entryButton()?.getAttribute("aria-label")).toBe("Ledger: 5 changes")
    expect(entryButton()?.textContent).toBe("Ledger5")
  })

  it("is marked when the changes have problems", async () => {
    await renderSession("/p/palace/s/palace-new")
    expect(entryButton()?.dataset.ledgerEntry).toBe("problems")
    expect(entryButton()?.dataset.variant).toBe("destructive")
    expect(entryButton()?.getAttribute("aria-label")).toBe("Ledger: 3 changes, with problems")
  })

  it("is muted with why when the snapshot failed", async () => {
    await renderSession("/p/circus/s/fix-auth-refresh")
    expect(entryButton()?.dataset.ledgerEntry).toBe("failed")
    expect(entryButton()?.getAttribute("aria-label")).toBe(
      "Ledger: No Ledger in this session: the device has no permission to write the snapshot.",
    )
    expect(entryButton()?.className).toContain("text-muted-foreground")
  })

  it("is muted while the snapshot is prepared", async () => {
    const state = seedState()
    const ledger = readyLedger("web-vite", Date.now())
    ledger.status.state = "preparing"
    state.sessions.find((s) => s.sessionId === "web-vite")!.ledger = ledger
    await renderSession("/p/circus/s/web-vite", { state })
    expect(entryButton()?.dataset.ledgerEntry).toBe("preparing")
    expect(entryButton()?.getAttribute("aria-label")).toBe("Ledger: The Ledger snapshot is being prepared.")
    await openDrawer()
    expect(within(drawer()).getByText("Preparing the snapshot…")).toBeTruthy()
  })

  it("isn't there for a session without a snapshot", async () => {
    await renderSession("/p/toolbox/s/dotfiles-go")
    expect(screen.getByRole("heading", { level: 1 })).toBeTruthy()
    expect(entryButton()).toBeNull()
  })
})

describe("the drawer", () => {
  it("shows the status and one row per change, with what changed", async () => {
    await renderSession("/p/circus/s/web-vite")
    await openDrawer()
    expect(within(drawer()).getByRole("heading", { name: "Ledger" })).toBeTruthy()
    expect(drawer().querySelector('[data-slot="ledger-status"]')?.textContent).toMatch(/^Ready·collected \d\d:\d\d$/)
    expect(rows()).toHaveLength(5)
    expect(row("D?a").textContent).toContain("Show a session's Ledger changes next to its conversation")
    expect(row("D?a").textContent).toContain("New")
    expect(row("D?a").textContent).toContain("new in web/")
    expect(row("D3").textContent).toContain("moved architecture/storage/ → storage/")
    expect(row("D4").textContent).toContain("retitled “Ledger 以 Git 仓库存储” → “The Ledger is stored as a Git repository”")
    expect(row("D10").textContent).toContain("Render agent Markdown as untrusted input")
    expect(row("D10").textContent).toContain("revised")
    // Several flags at once.
    expect(row("D5").textContent).toContain(
      "revised, moved architecture/ → web/, retitled “Connect over JSON for the browser” → “Connect over JSON in the browser”",
    )
    // A warning, in a neutral style.
    expect(within(row("D?a")).getByText("body has no link to the code it decides on").parentElement?.className).toContain(
      "text-muted-foreground",
    )
    expect(row("D?a").dataset.problem).toBeUndefined()
  })

  it("says when a merge or a discard is being applied, in English and Chinese", async () => {
    const state = seedState()
    const ledger = readyLedger("web-vite", Date.now())
    ledger.status.applying = "merge"
    state.sessions.find((s) => s.sessionId === "web-vite")!.ledger = ledger
    await renderSession("/p/circus/s/web-vite", { state })
    await openDrawer()
    const applying = () => drawer().querySelector('[data-slot="ledger-applying"]')?.textContent
    expect(applying()).toBe("applying the merge; the next turn waits for it")
    await i18n.changeLanguage("zh-CN")
    await advance(10)
    expect(applying()).toBe("正在应用合并，下一回合等它完成")
  })

  it("links existing decisions to the project's Ledger page, not new ones", async () => {
    const { router } = await renderSession("/p/circus/s/web-vite")
    await openDrawer()
    expect(within(row("D3")).getByRole("link", { name: "D3" }).getAttribute("href")).toBe("/p/circus/ledger/D3")
    expect(within(row("D?a")).queryByRole("link")).toBeNull()
    // Following a link doesn't open the row's diff.
    fireEvent.click(within(row("D3")).getByRole("link", { name: "D3" }))
    await advance(10)
    expect(router.state.location.pathname).toBe("/p/circus/ledger/D3")
  })

  it("describes each kind of change", async () => {
    const entry = (over: Partial<SessionLedgerEntry>): SessionLedgerEntry => ({
      path: "",
      basePath: "",
      number: "",
      title: "",
      baseTitle: "",
      added: false,
      contentChanged: false,
      moved: false,
      retitled: false,
      deleted: false,
      problems: [],
      ...over,
    })
    const entries = [
      entry({ path: "D?b-Root.md", number: "D?b", title: "Root", added: true }),
      entry({ path: "core/y/D7-x.md", basePath: "core/x/D7-x.md", number: "D7", title: "X", moved: true }),
      entry({ path: "D9-new.md", basePath: "D9-old.md", number: "D9", title: "new", baseTitle: "old", retitled: true }),
      entry({ path: "core/D8-a.md", basePath: "D8-a.md", number: "D8", title: "A", moved: true }),
      entry({ path: "docs/a.md", basePath: "a.md" }),
      entry({ path: "docs/gone.md", deleted: true }),
    ]
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        ledger: (mock) => ({
          getSessionLedger: async (req) => ({ ...(await mock.getSessionLedger(req)), entries }),
        }),
      }),
    })
    await openDrawer()
    expect(row("D?b").textContent).toContain("new in the root")
    expect(row("D7").textContent).toContain("moved core/x/ → core/y/")
    expect(row("D9").textContent).toContain("retitled “old” → “new”")
    expect(row("D8").textContent).toContain("moved the root → core/")
    expect(row("docs/a.md").textContent).toContain("changed, moved from a.md")
    expect(row("docs/gone.md").textContent).toContain("deleted")
  })

  it("shows problems in red, those without a path on top, and blocks the merge", async () => {
    await renderSession("/p/palace/s/palace-new")
    await openDrawer()
    const top = drawer().querySelector('[data-slot="ledger-problems"]')!
    expect(top.textContent).toBe("the snapshot holds more than 8192 entries; nothing was collected past them")
    expect(top.querySelector("li")?.className).toContain("text-destructive")
    // A file that isn't a decision is a problem.
    const file = row("ops/notes.txt")
    expect(file.textContent).toContain("File")
    expect(file.textContent).toContain("added")
    expect(file.dataset.problem).toBe("true")
    expect(within(file).getByText(NOT_A_DECISION).parentElement?.className).toContain("text-destructive")
    const bad = row("D?a")
    expect(bad.dataset.problem).toBe("true")
    expect(within(bad).getByText("front matter must hold number and title").parentElement?.className).toContain(
      "text-destructive",
    )
    expect(row("D1").dataset.problem).toBeUndefined()
    const merge = within(drawer()).getByRole("button", { name: /Merge Ledger changes/ })
    expect(merge).toHaveProperty("disabled", true)
    expect(drawer().querySelector('[data-slot="merge-blocked"]')?.textContent).toBe(
      "Ask the agent to fix the problems before merging.",
    )
  })

  it("expands a row into its diff, as plain text", async () => {
    await renderSession("/p/palace/s/palace-new")
    await openDrawer()
    const added = row("D?a")
    const toggle = within(added).getByRole("button", { name: /^Show the changes to / })
    expect(toggle.getAttribute("aria-expanded")).toBe("false")
    // A click anywhere on the row opens it.
    fireEvent.click(within(added).getByText("Health checks over HTTP"))
    await advance(20)
    expect(toggle.getAttribute("aria-expanded")).toBe("true")
    const diff = added.querySelector<HTMLElement>('[data-slot="ledger-diff"]')!
    // The agent's markup is shown, never run or rendered.
    expect(diff.textContent).toContain("<script>alert('not run')</script>")
    expect(diff.querySelector("script")).toBeNull()
    expect(diff.querySelectorAll('[data-line="added"]').length).toBeGreaterThan(3)
    expect(diff.querySelector('[data-line="removed"]')).toBeNull()

    fireEvent.click(toggle)
    await advance(10)
    expect(added.querySelector('[data-slot="ledger-diff"]')).toBeNull()
  })

  it("marks truncated sides and never renders markup from either", async () => {
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        ledger: () => ({
          getSessionLedgerDiff: async () => ({
            baseExists: true,
            base: "# Title\n\nold <b>bold</b>\n",
            baseTruncated: true,
            changesetExists: true,
            changeset: '# Title\n\nnew <img src="x" onerror="alert(1)"> [link](javascript:alert(1))\n',
            changesetTruncated: true,
          }),
        }),
      }),
    })
    await openDrawer()
    fireEvent.click(within(row("D10")).getByRole("button", { name: /^Show the changes to / }))
    await advance(20)
    const diff = row("D10").querySelector<HTMLElement>('[data-slot="ledger-diff"]')!
    expect([...diff.querySelectorAll("[data-diff-note]")].map((n) => n.textContent)).toEqual([
      "The earlier version is cut at 256 KiB.",
      "This version is cut at 256 KiB.",
    ])
    expect(diff.querySelector("img, b, a")).toBeNull()
    expect(within(diff).getByText('new <img src="x" onerror="alert(1)"> [link](javascript:alert(1))')).toBeTruthy()
    expect(within(diff).getByText("old <b>bold</b>")).toBeTruthy()
  })
})

describe("merging", () => {
  it("merges the changeset the user saw and shows the numbers it got", async () => {
    const merges: { expectedChangesetId: string }[] = []
    const { backend } = await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        ledger: (mock) => ({
          mergeSessionLedger: (req) => {
            merges.push(req)
            return mock.mergeSessionLedger(req)
          },
        }),
      }),
    })
    const ledger = backend.client.ledger.getLedger({ projectId: "circus" })
    await advance(10)
    const next = (await ledger).nextNumber
    await openDrawer()
    fireEvent.click(within(drawer()).getByRole("button", { name: "Merge Ledger changes" }))
    await advance(50)
    expect(merges).toEqual([{ sessionId: "web-vite", expectedChangesetId: "cs-web-vite" }])
    const done = within(drawer()).getAllByRole("status").find((s) => s.textContent?.startsWith("Merged."))!
    expect(done.textContent).toBe(`Merged. New decisions: D${next}`)
    expect(within(done).getByRole("link", { name: `D${next}` }).getAttribute("href")).toBe(`/p/circus/ledger/D${next}`)
    // The snapshot and the header follow.
    expect(within(drawer()).getByText("No Ledger changes in this session.")).toBeTruthy()
    expect(drawer().querySelector('[data-slot="ledger-merged"]')?.textContent).toMatch(new RegExp(`^Merged \\d\\d:\\d\\d: D${next}$`))
    expect(entryButton()?.getAttribute("aria-label")).toBe("Ledger: 0 changes")
    // The recorded merge is a line in the conversation.
    expect(document.querySelector('[data-ledger-event="merged"]')?.textContent).toBe(`Ledger changes merged: D${next}`)
  })

  it("is disabled while a turn runs, saying when it can be merged", async () => {
    await renderSession("/p/circus/s/web-vite")
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "Keep going" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(150)
    await openDrawer()
    const merge = within(drawer()).getByRole("button", { name: "Merge Ledger changes" })
    expect(merge).toHaveProperty("disabled", true)
    expect(drawer().querySelector('[data-slot="merge-blocked"]')?.textContent).toBe("Merge after this turn ends")
    // Once the turn ends it can be merged.
    await advance(3_000)
    expect(within(drawer()).getByRole("button", { name: "Merge Ledger changes" })).toHaveProperty("disabled", false)
  })

  it("says in Chinese when the turn must end first", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderSession("/p/circus/s/web-vite")
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "继续" } })
    fireEvent.click(screen.getAllByRole("button").find((b) => b.getAttribute("type") === "submit")!)
    await advance(150)
    await openDrawer()
    expect(within(drawer()).getByRole("button", { name: "合入 Ledger 改动" })).toHaveProperty("disabled", true)
    expect(drawer().querySelector('[data-slot="merge-blocked"]')?.textContent).toBe("本轮结束后可合入")
  })

  it("waits for an integrator when the changes don't fast-forward", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderSession("/p/circus/s/quic-spike")
    await openDrawer()
    const waiting = drawer().querySelector<HTMLElement>('[data-merge-state="waiting"]')!
    expect(within(waiting).getByText("等待合入中").textContent).toBe("等待合入中")
    expect(within(drawer()).queryByRole("button", { name: "合入 Ledger 改动" })).toBeNull()
    expect(within(waiting).getAllByRole("button")).toHaveLength(1)
    expect(within(waiting).getByRole("button").getAttribute("aria-label")).toBe("关于等待合入")
  })

  it("says why a merge was refused, and reads the changes again", async () => {
    let reads = 0
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        ledger: (mock) => ({
          getSessionLedger: (req) => {
            reads++
            return mock.getSessionLedger(req)
          },
          mergeSessionLedger: async () => {
            throw new ApiError("failed_precondition", "changed", undefined, { preconditionType: "CHANGESET_CHANGED" })
          },
        }),
      }),
    })
    await openDrawer()
    const before = reads
    fireEvent.click(within(drawer()).getByRole("button", { name: "Merge Ledger changes" }))
    await advance(50)
    expect(within(drawer()).getByRole("alert").textContent).toBe(
      "The changes have changed since you looked. Check them again, then merge.",
    )
    expect(reads).toBeGreaterThan(before)
  })
})

describe("reading the changes one at a time", () => {
  /**
   * The server's single slot per user for session ledger reads: a read
   * while another is under way is refused as busy.
   */
  const oneSlot = (log: { busy: number; snapshots: number; diffs: number }) =>
    patch({
      ledger: (mock) => {
        let inFlight = 0
        const guard =
          <A, R>(count: () => void, read: (req: A) => Promise<R>) =>
          async (req: A) => {
            if (inFlight > 0) {
              log.busy++
              throw new ApiError("resource_exhausted", "ledger is busy", undefined, { reason: "LEDGER_BUSY" })
            }
            inFlight++
            count()
            try {
              return await read(req)
            } finally {
              inFlight--
            }
          }
        return {
          getSessionLedger: guard(() => log.snapshots++, mock.getSessionLedger),
          getSessionLedgerDiff: guard(() => log.diffs++, mock.getSessionLedgerDiff),
        }
      },
    })

  it("merges with diffs open without a busy refusal, reading the changes once they changed", async () => {
    const log = { busy: 0, snapshots: 0, diffs: 0 }
    await renderSession("/p/circus/s/web-vite", { wrapClient: oneSlot(log) })
    await openDrawer()
    for (const r of rows()) fireEvent.click(within(r).getByRole("button", { name: /^Show the changes to / }))
    await advance(200)
    expect(rows().every((r) => r.querySelector('[data-slot="ledger-diff"]'))).toBe(true)
    const before = { ...log }
    expect(before.diffs).toBe(rows().length)

    fireEvent.click(within(drawer()).getByRole("button", { name: "Merge Ledger changes" }))
    await advance(500)
    expect(within(drawer()).getByText("No Ledger changes in this session.")).toBeTruthy()
    expect(log.busy).toBe(0)
    // The merge's ledger event reads the changes; the diffs went with them.
    expect(log.snapshots - before.snapshots).toBe(1)
    expect(log.diffs).toBe(before.diffs)
  })

  it("waits out a busy refusal, showing the diff once a retry gets it", async () => {
    let refusals = 1
    await renderSession("/p/palace/s/palace-new", {
      wrapClient: patch({
        ledger: (mock) => ({
          getSessionLedgerDiff: async (req) => {
            if (refusals-- > 0) throw new ApiError("resource_exhausted", "ledger is busy", undefined, { reason: "LEDGER_BUSY" })
            return mock.getSessionLedgerDiff(req)
          },
        }),
      }),
    })
    await openDrawer()
    const added = row("D?a")
    fireEvent.click(within(added).getByRole("button", { name: /^Show the changes to / }))
    await advance(50)
    expect(added.textContent).not.toContain("Couldn't load the changes to this file.")
    await advance(1_000)
    expect(added.querySelector('[data-slot="ledger-diff"]')?.textContent).toContain("Health checks over HTTP")
  })
})

describe("a diff and the snapshot's version", () => {
  it("reads an open diff again for a new version of the snapshot, showing the old one until the new one is in", async () => {
    let stream: ReturnType<typeof scripted> | undefined
    let diffReads = 0
    let holdDiff: Promise<void> | null = null
    const { state } = await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        sessions: (mock) => {
          stream = scripted(mock)
          return { watchSession: stream.watchSession }
        },
        ledger: (mock) => ({
          getSessionLedgerDiff: async (req) => {
            diffReads++
            if (holdDiff) await holdDiff
            return mock.getSessionLedgerDiff(req)
          },
        }),
      }),
    })
    await openDrawer()
    const first = rows()[0]
    fireEvent.click(within(first).getByRole("button", { name: /^Show the changes to / }))
    await advance(100)
    const diff = () => rows()[0].querySelector<HTMLElement>('[data-slot="ledger-diff"]')
    expect(diff()?.textContent).not.toContain("A line the agent added later")
    expect(diffReads).toBe(1)

    // The agent changed the file; a collection gives the snapshot a new version.
    const ledger = state.sessions.find((s) => s.sessionId === "web-vite")!.ledger!
    const change = ledger.open!.changes[0]
    change.content = `${change.content ?? ""}\nA line the agent added later\n`
    ledger.status.updatedAtUnixMs += 1_000
    let release = () => {}
    holdDiff = new Promise<void>((resolve) => (release = resolve))
    act(() =>
      stream!.control.push({
        seq: 0,
        tsUnixMs: ledger.status.updatedAtUnixMs,
        kind: "ledger",
        payloadJson: '{"event":"collected","changeset_id":"cs-web-vite"}',
        ephemeral: true,
        truncated: false,
      }),
    )
    await advance(100)
    expect(diffReads).toBe(2)
    // The old diff stays while the new one is read.
    expect(diff()).not.toBeNull()
    expect(diff()?.textContent).not.toContain("A line the agent added later")
    holdDiff = null
    act(() => release())
    await advance(100)
    expect(diff()?.textContent).toContain("A line the agent added later")
    expect(diffReads).toBe(2)
  })

  it("shows a refusal other than busy as an error without retrying it", async () => {
    let diffReads = 0
    await renderSession("/p/palace/s/palace-new", {
      wrapClient: patch({
        ledger: () => ({
          getSessionLedgerDiff: async () => {
            diffReads++
            throw new ApiError("resource_exhausted", "message too large")
          },
        }),
      }),
    })
    await openDrawer()
    const added = row("D?a")
    fireEvent.click(within(added).getByRole("button", { name: /^Show the changes to / }))
    await advance(100)
    expect(added.textContent).toContain("Couldn't load the changes to this file.")
    expect(diffReads).toBe(1)
  })
})

describe("live updates", () => {
  it("reads the changes and the session again on a ledger event, live or recorded", async () => {
    let ledgerReads = 0
    let sessionReads = 0
    let stream: ReturnType<typeof scripted> | undefined
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        sessions: (mock) => {
          stream = scripted(mock)
          return {
            watchSession: stream.watchSession,
            getSession: (req) => {
              sessionReads++
              return mock.getSession(req)
            },
          }
        },
        ledger: (mock) => ({
          getSessionLedger: (req) => {
            ledgerReads++
            return mock.getSessionLedger(req)
          },
        }),
      }),
    })
    await openDrawer()
    const [ledgerBefore, sessionBefore] = [ledgerReads, sessionReads]
    act(() =>
      stream!.control.push({
        seq: 0,
        tsUnixMs: Date.now(),
        kind: "ledger",
        payloadJson: '{"event":"collected","changeset_id":"cs-web-vite"}',
        ephemeral: true,
        truncated: false,
      }),
    )
    await advance(20)
    expect(ledgerReads).toBe(ledgerBefore + 1)
    expect(sessionReads).toBe(sessionBefore + 1)

    act(() =>
      stream!.control.push({
        seq: 1000,
        tsUnixMs: Date.now(),
        kind: "ledger",
        payloadJson: '{"event":"merged","changeset_id":"cs-web-vite","numbers":{"D?b":93,"D?a":92}}',
        ephemeral: false,
        truncated: false,
      }),
    )
    await advance(20)
    expect(ledgerReads).toBe(ledgerBefore + 2)
    const line = document.querySelector<HTMLElement>('[data-ledger-event="merged"]')!
    expect(line.textContent).toBe("Ledger changes merged: D92, D93")
    // (Behind the open drawer, the line is hidden from the accessibility tree.)
    expect([...line.querySelectorAll("a")].map((a) => a.getAttribute("href"))).toEqual([
      "/p/circus/ledger/D92",
      "/p/circus/ledger/D93",
    ])

    act(() =>
      stream!.control.push({
        seq: 1001,
        tsUnixMs: Date.now(),
        kind: "ledger",
        payloadJson: '{"event":"discarded","changeset_id":"cs-2","reason":"session deleted"}',
        ephemeral: false,
        truncated: false,
      }),
    )
    await advance(20)
    expect(document.querySelector('[data-ledger-event="discarded"]')?.textContent).toBe(
      "Ledger changes discarded: session deleted",
    )
  })

  it("reads the changes again when the session shows the snapshot changed", async () => {
    let ledgerReads = 0
    const { state } = await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        ledger: (mock) => ({
          getSessionLedger: (req) => {
            ledgerReads++
            return mock.getSessionLedger(req)
          },
        }),
      }),
    })
    await openDrawer()
    const before = ledgerReads
    // Changed without an event: the session's poll shows it.
    state.sessions.find((s) => s.sessionId === "web-vite")!.ledger!.status.updatedAtUnixMs += 1_000
    await advance(5_100)
    expect(ledgerReads).toBe(before + 1)
  })
})

describe("deleting the session", () => {
  it("warns up front that unmerged changes are discarded, and deletes with them discarded", async () => {
    const deletes: Parameters<SessionService["deleteSession"]>[0][] = []
    const { router } = await renderSession("/p/circus/s/quic-spike", {
      wrapClient: patch({
        sessions: (mock) => ({
          deleteSession: (req) => {
            deletes.push(req)
            return mock.deleteSession(req)
          },
        }),
      }),
    })
    await openMenuItem("Delete session")
    const dialog = screen.getByRole("dialog")
    expect(within(dialog).getByRole("heading").textContent).toBe("Discard unmerged Ledger changes?")
    fireEvent.click(within(dialog).getByRole("button", { name: "Discard changes and delete" }))
    await advance(50)
    expect(deletes).toEqual([{ sessionId: "quic-spike", discardLedgerChanges: true }])
    expect(router.state.location.pathname).toBe("/p/circus")
  })

  it("asks again when the server refuses for unmerged changes the page didn't know of", async () => {
    const deletes: Parameters<SessionService["deleteSession"]>[0][] = []
    const { router } = await renderSession("/p/circus/s/quic-spike", {
      wrapClient: patch({
        sessions: (mock) => ({
          // The page's copy of the session doesn't show the changes.
          getSession: async (req) => {
            const { ledger: _, ...session } = await mock.getSession(req)
            return session
          },
          deleteSession: (req) => {
            deletes.push(req)
            return mock.deleteSession(req)
          },
        }),
      }),
    })
    await openMenuItem("Delete session")
    let dialog = screen.getByRole("dialog")
    expect(within(dialog).getByRole("heading").textContent).toBe("Delete this session?")
    fireEvent.click(within(dialog).getByRole("button", { name: "Delete session" }))
    await advance(50)
    dialog = screen.getByRole("dialog")
    expect(within(dialog).getByRole("heading").textContent).toBe("Discard unmerged Ledger changes?")
    expect(within(dialog).queryByRole("alert")).toBeNull()
    expect(router.state.location.pathname).toBe("/p/circus/s/quic-spike")

    fireEvent.click(within(dialog).getByRole("button", { name: "Discard changes and delete" }))
    await advance(50)
    expect(deletes).toEqual([
      { sessionId: "quic-spike", discardLedgerChanges: false },
      { sessionId: "quic-spike", discardLedgerChanges: true },
    ])
    expect(router.state.location.pathname).toBe("/p/circus")
  })

  it("warns in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderSession("/p/circus/s/quic-spike")
    await openMenuItem("删除会话")
    const dialog = screen.getByRole("dialog")
    expect(within(dialog).getByRole("heading").textContent).toBe("丢弃未合入的 Ledger 改动？")
    expect(within(dialog).getByRole("button", { name: "丢弃改动并删除" })).toBeTruthy()
  })

  it("can't be chosen while the session isn't stopped", async () => {
    await renderSession("/p/circus/s/web-vite")
    const trigger = screen.getByRole("button", { name: "Session actions" })
    fireEvent.pointerDown(trigger, { button: 0, ctrlKey: false, pointerType: "mouse" })
    await advance(10)
    expect(screen.getByRole("menuitem", { name: "Delete session" }).getAttribute("aria-disabled")).toBe("true")
  })

  it("says why a delete failed otherwise", async () => {
    await renderSession("/p/circus/s/quic-spike", {
      wrapClient: patch({
        sessions: () => ({
          deleteSession: async () => {
            throw new ApiError("failed_precondition", "lease held")
          },
        }),
      }),
    })
    await openMenuItem("Delete session")
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Discard changes and delete" }))
    await advance(50)
    expect(within(screen.getByRole("dialog")).getByRole("alert").textContent).toBe(
      "The session can't be deleted right now: it is still stopping or busy. Try again in a moment.",
    )
  })
})

describe("large diffs", () => {
  /** `bytes` of `width`-character lines, each different from every line of the other side. */
  const filled = (bytes: number, width: number, prefix: string) => {
    const line = (i: number) => `${prefix}${i}`.padEnd(width - 1, "x")
    const out: string[] = []
    for (let i = 0, n = 0; n < bytes; i++) {
      out.push(line(i))
      n += width
    }
    return out.join("\n")
  }
  const withDiff = (base: string, changeset: string) =>
    patch({
      ledger: () => ({
        getSessionLedgerDiff: async () => ({
          baseExists: base !== "",
          base,
          baseTruncated: false,
          changesetExists: true,
          changeset,
          changesetTruncated: false,
        }),
      }),
    })
  const expand = async (text: string) => {
    fireEvent.click(within(row(text)).getByRole("button", { name: /^(Show|Hide) the changes to / }))
    await advance(20)
  }

  it("shows a bounded number of rows for two 256 KiB files that differ everywhere", async () => {
    const size = 256 * 1024
    await renderSession("/p/circus/s/web-vite", { wrapClient: withDiff(filled(size, 8, "a"), filled(size, 8, "b")) })
    await openDrawer()
    await expand("D10")
    const diff = row("D10").querySelector<HTMLElement>('[data-slot="ledger-diff"]')!
    expect(diff.dataset.outcome).toBe("too_large")
    expect(within(diff).getByText("Too long to compare line by line; both versions are shown, up to 1000 lines each.")).toBeTruthy()
    const shown = diff.querySelectorAll('[role="row"]')
    expect(shown.length).toBe(MAX_DIFF_ROWS + 2)
    const more = [...diff.querySelectorAll<HTMLElement>('[data-line="more"]')].map((r) => r.textContent)
    expect(more).toHaveLength(2)
    expect(more[0]).toMatch(/^⋯ \d+ more lines not shown$/)
  })

  it("caps a diff of a large new file, and keeps it when the row is opened again", async () => {
    const lines = Array.from({ length: 5_000 }, (_, i) => `line ${i}`).join("\n")
    await renderSession("/p/circus/s/web-vite", { wrapClient: withDiff("", lines) })
    await openDrawer()
    await expand("D10")
    let diff = row("D10").querySelector<HTMLElement>('[data-slot="ledger-diff"]')!
    expect(diff.dataset.outcome).toBe("diff")
    expect(diff.querySelectorAll('[data-line="added"]')).toHaveLength(MAX_DIFF_ROWS)
    expect(diff.querySelector('[data-line="more"]')?.textContent).toBe("⋯ 3000 more lines not shown")

    // Closed and opened again: shown at once, without comparing again.
    fireEvent.click(within(row("D10")).getByRole("button", { name: /^Hide the changes to / }))
    await advance(10)
    fireEvent.click(within(row("D10")).getByRole("button", { name: /^Show the changes to / }))
    diff = row("D10").querySelector<HTMLElement>('[data-slot="ledger-diff"]')!
    expect(diff.querySelectorAll('[data-line="added"]')).toHaveLength(MAX_DIFF_ROWS)
  })

  it("names the diff table by its path, with an id that doesn't hold the path", async () => {
    await renderSession("/p/circus/s/web-vite")
    await openDrawer()
    await expand("D10")
    const toggle = within(row("D10")).getByRole("button", { name: /^Hide the changes to / })
    const path = toggle.getAttribute("aria-label")!.replace("Hide the changes to ", "")
    expect(path).toBe("web/D10-Render-agent-Markdown-as-untrusted-input.md")
    const id = toggle.getAttribute("aria-controls")!
    expect(id).not.toContain("/")
    const region = document.getElementById(id)!
    expect(within(region).getByRole("table").getAttribute("aria-label")).toBe(`Changes to ${path}`)
  })
})

describe("merge messages and blocks", () => {
  it("disables the button while a merge is under way", async () => {
    let release = () => {}
    let calls = 0
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        ledger: (mock) => ({
          mergeSessionLedger: async (req) => {
            calls++
            await new Promise<void>((resolve) => (release = resolve))
            return mock.mergeSessionLedger(req)
          },
        }),
      }),
    })
    await openDrawer()
    fireEvent.click(within(drawer()).getByRole("button", { name: "Merge Ledger changes" }))
    await advance(10)
    const pending = within(drawer()).getByRole("button", { name: "Merging…" })
    expect(pending).toHaveProperty("disabled", true)
    fireEvent.click(pending)
    act(() => release())
    await advance(50)
    expect(calls).toBe(1)
    expect(within(drawer()).getByText(/^Merged\. New decisions:/)).toBeTruthy()
  })

  it("forgets a refusal once the changes change", async () => {
    let stream: ReturnType<typeof scripted> | undefined
    const { state } = await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        sessions: (mock) => {
          stream = scripted(mock)
          return { watchSession: stream.watchSession }
        },
        ledger: () => ({
          mergeSessionLedger: async () => {
            throw new ApiError("failed_precondition", "running", undefined, { preconditionType: "TURN_RUNNING" })
          },
        }),
      }),
    })
    await openDrawer()
    fireEvent.click(within(drawer()).getByRole("button", { name: "Merge Ledger changes" }))
    await advance(50)
    expect(within(drawer()).getByRole("alert").textContent).toBe("A turn is running. Merge after it ends.")
    // Reading the changes again after the refusal keeps it.
    await advance(5_100)
    expect(within(drawer()).queryByRole("alert")).not.toBeNull()

    // The turn ended and the snapshot was collected.
    const ledger = state.sessions.find((s) => s.sessionId === "web-vite")!.ledger!
    ledger.status.updatedAtUnixMs += 1_000
    act(() =>
      stream!.control.push({
        seq: 0,
        tsUnixMs: ledger.status.updatedAtUnixMs,
        kind: "ledger",
        payloadJson: '{"event":"collected","changeset_id":"cs-web-vite"}',
        ephemeral: true,
        truncated: false,
      }),
    )
    await advance(50)
    expect(within(drawer()).queryByRole("alert")).toBeNull()
  })

  it.each(["the answer first", "the ledger event first"] as const)(
    "keeps the numbers of a merge whatever comes first (%s)",
    async (order) => {
      let stream: ReturnType<typeof scripted> | undefined
      const mergedEvent = (seq: number): RawSessionEvent => ({
        seq,
        tsUnixMs: Date.now(),
        kind: "ledger",
        payloadJson: '{"event":"merged","changeset_id":"cs-web-vite","numbers":{"D?a":99}}',
        ephemeral: false,
        truncated: false,
      })
      await renderSession("/p/circus/s/web-vite", {
        wrapClient: patch({
          // The mock's own stream stays quiet after the history: the test sends the ledger event.
          sessions: (mock) => {
            stream = scripted(mock)
            return { watchSession: stream.watchSession }
          },
          ledger: (mock) => ({
            mergeSessionLedger: async (req) => {
              const answer = await mock.mergeSessionLedger(req)
              if (order === "the ledger event first") {
                act(() => stream!.control.push(mergedEvent(1000)))
                await new Promise((resolve) => setTimeout(resolve, 300))
              }
              return answer
            },
          }),
        }),
      })
      await openDrawer()
      fireEvent.click(within(drawer()).getByRole("button", { name: "Merge Ledger changes" }))
      const merged = () => within(drawer()).queryByText(/^Merged\. New decisions:/)
      await advance(order === "the ledger event first" ? 400 : 50)
      expect(merged()).toBeTruthy()
      // Not offered again while the snapshot read still shows the merged changes.
      expect(within(drawer()).queryByRole("button", { name: "Merge Ledger changes" })).toBeNull()
      if (order === "the answer first") {
        await advance(300)
        act(() => stream!.control.push(mergedEvent(1000)))
      }
      await advance(100)
      expect(merged()).toBeTruthy()
      // The session's poll and the follow-up reads change nothing about it.
      await advance(5_100)
      expect(merged()).toBeTruthy()
      expect(within(drawer()).getByText("No Ledger changes in this session.")).toBeTruthy()
    },
  )

  it("doesn't offer the merge again while the changes read still show the merged changeset", async () => {
    // Once frozen, reads answer with the changes as they were before the merge.
    let last: Awaited<ReturnType<LedgerService["getSessionLedger"]>> | undefined
    let frozen = false
    let reads = 0
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        // The stream stays quiet after the history, so no ledger event reads the changes.
        sessions: (mock) => ({ watchSession: scripted(mock).watchSession }),
        ledger: (mock) => ({
          getSessionLedger: async (req) => {
            reads++
            if (!frozen || !last) last = await mock.getSessionLedger(req)
            return last
          },
        }),
      }),
    })
    await openDrawer()
    const merge = () => within(drawer()).queryByRole("button", { name: "Merge Ledger changes" })
    expect(merge()).toHaveProperty("disabled", false)
    frozen = true
    const before = reads
    fireEvent.click(merge()!)
    await advance(500)
    // The session's newer version read the changes again, still as before the merge.
    expect(reads).toBeGreaterThan(before)
    expect(rows().length).toBeGreaterThan(0)
    expect(within(drawer()).getByText(/^Merged\. New decisions:/)).toBeTruthy()
    expect(merge()).toBeNull()
  })

  it("forgets the numbers of a merge once a new changeset opens", async () => {
    await renderSession("/p/circus/s/web-vite")
    await openDrawer()
    fireEvent.click(within(drawer()).getByRole("button", { name: "Merge Ledger changes" }))
    await advance(50)
    expect(within(drawer()).getByText(/^Merged\. New decisions:/)).toBeTruthy()
    // A later turn records a new decision: a new changeset.
    fireEvent.change(screen.getByRole("textbox", { hidden: true }), { target: { value: "Record a decision" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit", hidden: true }))
    await advance(6_000)
    expect(rows()).toHaveLength(1)
    expect(within(drawer()).queryByText(/^Merged\. New decisions:/)).toBeNull()
    expect(within(drawer()).getByRole("button", { name: "Merge Ledger changes" })).toHaveProperty("disabled", false)
  })

  it("is disabled while the device is offline with changes not collected yet, and says so", async () => {
    const state = seedState()
    state.devices.find((d) => d.name === "devbox")!.online = false
    await renderSession("/p/circus/s/web-vite", { state })
    // A turn ended while the device was offline; the session's poll shows it.
    state.sessions.find((s) => s.sessionId === "web-vite")!.ledger!.status.collectionDue = true
    await advance(5_100)
    await openDrawer()
    expect(drawer().querySelector('[data-slot="ledger-collection-due"]')?.textContent).toBe(
      "Changes on the device are not collected yet: the device is offline.",
    )
    expect(within(drawer()).getByRole("button", { name: "Merge Ledger changes" })).toHaveProperty("disabled", true)
    expect(drawer().querySelector('[data-slot="merge-blocked"]')?.textContent).toBe(
      "The device is offline and has changes that aren't collected yet. Merge once it is back.",
    )
  })

  it("says changes aren't collected yet while the device is online, and lets the merge collect them", async () => {
    const { state } = await renderSession("/p/circus/s/web-vite")
    state.sessions.find((s) => s.sessionId === "web-vite")!.ledger!.status.collectionDue = true
    await advance(5_100)
    await openDrawer()
    expect(drawer().querySelector('[data-slot="ledger-collection-due"]')?.textContent).toBe(
      "Changes on the device are not collected yet.",
    )
    expect(within(drawer()).getByRole("button", { name: "Merge Ledger changes" })).toHaveProperty("disabled", false)
  })
})

describe("the drawer's focus and reads", () => {
  it("gives focus back to the Ledger button when it closes", async () => {
    await renderSession("/p/circus/s/web-vite")
    const button = entryButton()!
    button.focus()
    fireEvent.keyDown(button, { key: "Enter" })
    fireEvent.click(button)
    await advance(20)
    expect(drawer()).toBeTruthy()
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" })
    await advance(50)
    expect(document.querySelector("[data-session-ledger]")).toBeNull()
    expect(document.activeElement).toBe(entryButton())
  })

  it("reads the changes once for a ledger event that changed them", async () => {
    let reads = 0
    let stream: ReturnType<typeof scripted> | undefined
    const { state } = await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        sessions: (mock) => {
          stream = scripted(mock)
          return { watchSession: stream.watchSession }
        },
        ledger: (mock) => ({
          getSessionLedger: (req) => {
            reads++
            return mock.getSessionLedger(req)
          },
        }),
      }),
    })
    await openDrawer()
    const before = reads
    const ledger = state.sessions.find((s) => s.sessionId === "web-vite")!.ledger!
    ledger.status.updatedAtUnixMs += 1_000
    act(() =>
      stream!.control.push({
        seq: 0,
        tsUnixMs: ledger.status.updatedAtUnixMs,
        kind: "ledger",
        payloadJson: '{"event":"collected","changeset_id":"cs-web-vite"}',
        ephemeral: true,
        truncated: false,
      }),
    )
    await advance(100)
    expect(reads).toBe(before + 1)
  })

  it("labels the muted entry in the language's own punctuation", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderSession("/p/circus/s/fix-auth-refresh")
    expect(entryButton()?.getAttribute("aria-label")).toBe("Ledger：这个会话没有 Ledger：设备没有写入快照的权限。")
  })
})

describe("deleting the session, continued", () => {
  it("can't be called off while the delete is under way, and goes to the project once done", async () => {
    let release = () => {}
    const { router, state, queryClient } = await renderSession("/p/circus/s/quic-spike", {
      wrapClient: patch({
        sessions: (mock) => ({
          deleteSession: async (req) => {
            await new Promise<void>((resolve) => (release = resolve))
            return mock.deleteSession(req)
          },
        }),
      }),
    })
    // The lists the sidebar shows, and the session's Ledger, are cached.
    await queryClient.prefetchQuery({ queryKey: queryKeys.sessions(), queryFn: () => listSessions(state) })
    await openDrawer()
    fireEvent.keyDown(document.activeElement ?? document.body, { key: "Escape" })
    await advance(50)
    expect(queryClient.getQueryData(queryKeys.sessionLedger("quic-spike"))).toBeDefined()

    await openMenuItem("Delete session")
    const dialog = screen.getByRole("dialog")
    fireEvent.click(within(dialog).getByRole("button", { name: "Discard changes and delete" }))
    await advance(10)
    const cancel = within(dialog).getByRole("button", { name: "Cancel" })
    expect(cancel).toHaveProperty("disabled", true)
    fireEvent.click(cancel)
    fireEvent.keyDown(dialog, { key: "Escape" })
    await advance(10)
    expect(screen.getByRole("dialog")).toBe(dialog)

    act(() => release())
    await advance(50)
    expect(router.state.location.pathname).toBe("/p/circus")
    expect(state.sessions.some((s) => s.sessionId === "quic-spike")).toBe(false)
    // Gone from the sidebar's lists and the caches.
    const listed = queryClient.getQueryData<{ sessionId: string }[]>(queryKeys.sessions()) ?? []
    expect(listed.some((s) => s.sessionId === "quic-spike")).toBe(false)
    expect(queryClient.getQueryData(queryKeys.sessionLedger("quic-spike"))).toBeUndefined()
    expect(queryClient.getQueryData(queryKeys.session("quic-spike"))).toBeUndefined()
  })
})

/** The sessions list as the sidebar reads it. */
function listSessions(state: MockState) {
  return Promise.resolve(state.sessions.map((s) => ({ sessionId: s.sessionId, projectId: s.projectId })))
}

describe("diffs that can't be shown line by line", () => {
  it("says when the two versions are too different to compare, and shows both", async () => {
    diffRunner.givesUp = true
    await renderSession("/p/circus/s/web-vite")
    await openDrawer()
    fireEvent.click(within(row("D10")).getByRole("button", { name: /^Show the changes to / }))
    await advance(20)
    const diff = row("D10").querySelector<HTMLElement>('[data-slot="ledger-diff"]')!
    expect(diff.dataset.outcome).toBe("too_different")
    expect([...diff.querySelectorAll("[data-diff-note]")].map((n) => n.textContent)).toEqual([
      "Too different to compare line by line; both versions are shown, up to 1000 lines each.",
    ])
    expect(diff.querySelectorAll('[data-line="removed"]').length).toBeGreaterThan(0)
    expect(diff.querySelectorAll('[data-line="added"]').length).toBeGreaterThan(0)
    expect(diff.querySelector('[data-line="context"]')).toBeNull()
  })

  it("shows nothing for a file the changes no longer hold", async () => {
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        ledger: () => ({
          getSessionLedgerDiff: async () => {
            throw new ApiError("not_found", "not changed")
          },
        }),
      }),
    })
    await openDrawer()
    const toggle = within(row("D10")).getByRole("button", { name: /^Show the changes to / })
    fireEvent.click(toggle)
    await advance(20)
    const region = document.getElementById(toggle.getAttribute("aria-controls")!)!
    expect(region.textContent).toBe("")
    expect(within(region).queryByRole("status")).toBeNull()
  })
})

describe("following the session's snapshot", () => {
  it("reads the changes again when a read that began before a change brought the old ones", async () => {
    let reads = 0
    let hold: Promise<void> | null = null
    let release = () => {}
    const { state, queryClient } = await renderSession("/p/circus/s/web-vite", {
      wrapClient: patch({
        ledger: (mock) => ({
          getSessionLedger: async (req) => {
            reads++
            // Read when asked; answered once let go.
            const answer = await mock.getSessionLedger(req)
            if (hold) await hold
            return answer
          },
        }),
      }),
    })
    await openDrawer()
    expect(rows()).toHaveLength(5)
    const before = reads

    // A read starts (a refocus, say) and is slow to answer.
    hold = new Promise<void>((resolve) => (release = resolve))
    act(() => void queryClient.invalidateQueries({ queryKey: queryKeys.sessionLedger("web-vite") }))
    await advance(10)
    expect(reads).toBe(before + 1)

    // Meanwhile the changes change without an event; the session's poll sees it.
    const ledger = state.sessions.find((s) => s.sessionId === "web-vite")!.ledger!
    ledger.open!.changes.pop()
    ledger.status.updatedAtUnixMs += 1_000
    await advance(5_100)
    expect(entryButton()?.getAttribute("aria-label")).toBe("Ledger: 4 changes")
    expect(reads).toBe(before + 1)

    // The slow read brings the old changes: they are read again.
    hold = null
    act(() => release())
    await advance(50)
    expect(reads).toBe(before + 2)
    expect(rows()).toHaveLength(4)
  })
})

describe("leaving during a delete", () => {
  it("stays where the user went when the delete finishes", async () => {
    let release = () => {}
    const { router, state } = await renderSession("/p/circus/s/quic-spike", {
      wrapClient: patch({
        sessions: (mock) => ({
          deleteSession: async (req) => {
            await new Promise<void>((resolve) => (release = resolve))
            return mock.deleteSession(req)
          },
        }),
      }),
    })
    await openMenuItem("Delete session")
    fireEvent.click(within(screen.getByRole("dialog")).getByRole("button", { name: "Discard changes and delete" }))
    await advance(10)
    // The browser's Back, or a link from elsewhere.
    await act(() => router.navigate("/p/circus/ledger/D3"))
    await advance(10)
    act(() => release())
    await advance(50)
    expect(state.sessions.some((s) => s.sessionId === "quic-spike")).toBe(false)
    expect(router.state.location.pathname).toBe("/p/circus/ledger/D3")
  })
})
