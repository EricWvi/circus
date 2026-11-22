// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen, within } from "@testing-library/react"
import { focusManager } from "@tanstack/react-query"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import type { CircusClient, DecisionVersion } from "@/api/types"
import { fakeCommitId } from "@/mock/ledger"
import { SidebarProvider } from "@/components/ui/sidebar"
import { advance, renderWithBackend } from "@/test/render"

import { LedgerPage } from "./ledger-page"

async function renderLedger(path: string, wrapClient?: (client: CircusClient) => CircusClient) {
  const page = (
    <SidebarProvider>
      <LedgerPage />
    </SidebarProvider>
  )
  const router = createMemoryRouter(
    [
      { path: "/p/:projectId/ledger", element: page },
      { path: "/p/:projectId/ledger/:decision", element: page },
      { path: "*", element: <h1>Elsewhere</h1> },
    ],
    { initialEntries: [path] },
  )
  const result = await renderWithBackend(<RouterProvider router={router} />, { wrapClient })
  await advance(50)
  return { ...result, router }
}

/** Where the router is now, with the query. */
const at = (router: { state: { location: { pathname: string; search: string } } }) =>
  router.state.location.pathname + router.state.location.search

/** A client whose ledger calls are replaced by `ledger`'s. */
const withLedger = (ledger: Partial<CircusClient["ledger"]>) => (client: CircusClient): CircusClient => ({
  ...client,
  ledger: { ...client.ledger, ...ledger },
})

const tree = () => screen.getByRole("tree", { name: "Decisions" })
const item = (name: string | RegExp) => within(tree()).getByRole("treeitem", { name })
const visibleItems = () => within(tree()).getAllByRole("treeitem").map((el) => el.textContent)

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-21T12:00:00Z"))
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe("the ledger page", () => {
  it("shows a skeleton while the ledger loads", async () => {
    await renderLedger("/p/circus/ledger", withLedger({ getLedger: () => new Promise(() => {}) }))
    expect(screen.getByRole("status", { name: "Loading" })).toBeTruthy()
    expect(screen.queryByRole("tree")).toBeNull()
  })

  it("explains an empty ledger", async () => {
    await renderLedger("/p/toolbox/ledger")
    expect(screen.getByText("No decisions yet")).toBeTruthy()
    expect(screen.getByText("Decisions appear here once they are merged.")).toBeTruthy()
    expect(screen.queryByRole("tree")).toBeNull()
  })

  it("says when the ledger can't be loaded, and tries again", async () => {
    let fail = true
    await renderLedger(
      "/p/circus/ledger",
      withLedger({
        getLedger: () =>
          fail
            ? Promise.reject(new ApiError("internal", "boom"))
            : Promise.resolve({ headCommit: "", entries: [], nextNumber: 0 }),
      }),
    )
    const alert = screen.getByRole("alert")
    expect(alert.textContent).toContain("Couldn't load the Ledger.")
    fail = false
    fireEvent.click(within(alert).getByRole("button", { name: "Try again" }))
    await advance(50)
    expect(screen.getByText("No decisions yet")).toBeTruthy()
  })

  it("leaves a project that isn't there", async () => {
    const { router } = await renderLedger("/p/nope/ledger")
    expect(at(router)).toBe("/")
  })

  it("lists directories, then decisions, with the count and next number", async () => {
    await renderLedger("/p/circus/ledger")
    expect(visibleItems().slice(0, 6)).toEqual([
      "architecture6",
      "storage3",
      "D3SQLite as the only database",
      "D4Ledger 以 Git 仓库存储",
      "D13Bound ledger sizes",
      "D1Go control plane, React Web UI",
    ])
    expect(item(/^architecture/).getAttribute("aria-level")).toBe("1")
    expect(item(/^D13/).getAttribute("aria-level")).toBe("3")
    expect(item(/^D0/).getAttribute("href")).toBe("/p/circus/ledger/D0")
    expect(screen.getByText("16 decisions · next D16")).toBeTruthy()
    expect(screen.getByText("Choose a decision to read it.")).toBeTruthy()
  })

  it("filters by number or title", async () => {
    await renderLedger("/p/circus/ledger")
    const filter = screen.getByRole("searchbox", { name: "Filter decisions" })
    fireEvent.change(filter, { target: { value: "d1" } })
    expect(visibleItems()).toEqual([
      "architecture",
      "storage",
      "D13Bound ledger sizes",
      "D1Go control plane, React Web UI",
      "web",
      "D10Render agent Markdown as untrusted input",
      "D11界面文本全部走 i18n",
      "D12Two-step layout on narrow screens",
      "D14Ledger 页面只读",
      "D15Hover previews for decision references",
    ])
    fireEvent.change(filter, { target: { value: "仓库" } })
    expect(visibleItems()).toEqual(["architecture", "storage", "D4Ledger 以 Git 仓库存储"])
    fireEvent.change(filter, { target: { value: "no such thing" } })
    // Outside the tree, which only holds tree items.
    expect(screen.getByText("No decisions match").closest("[role=tree]")).toBeNull()
  })

  it("moves through the tree with the keyboard", async () => {
    const { router } = await renderLedger("/p/circus/ledger")
    // One row is in the tab order.
    expect(within(tree()).getAllByRole("treeitem").filter((el) => el.tabIndex === 0)).toHaveLength(1)
    const architecture = item(/^architecture/)
    act(() => architecture.focus())
    fireEvent.keyDown(architecture, { key: "ArrowDown" })
    expect(document.activeElement).toBe(item(/^storage/))
    // Left closes the open directory; Left again goes up.
    fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" })
    expect(item(/^storage/).getAttribute("aria-expanded")).toBe("false")
    expect(visibleItems()).not.toContain("D13Bound ledger sizes")
    fireEvent.keyDown(document.activeElement!, { key: "ArrowLeft" })
    expect(document.activeElement).toBe(item(/^architecture/))
    // Right opens a closed directory, then goes into it.
    fireEvent.keyDown(document.activeElement!, { key: "ArrowDown" })
    fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" })
    expect(item(/^storage/).getAttribute("aria-expanded")).toBe("true")
    fireEvent.keyDown(document.activeElement!, { key: "ArrowRight" })
    expect(document.activeElement).toBe(item(/^D3/))
    expect((document.activeElement as HTMLElement).tabIndex).toBe(0)
    fireEvent.keyDown(document.activeElement!, { key: "End" })
    expect(document.activeElement).toBe(item(/^D0/))
    fireEvent.keyDown(document.activeElement!, { key: " " })
    await advance(50)
    expect(at(router)).toBe("/p/circus/ledger/D0")
    expect(item(/^D0/).getAttribute("aria-selected")).toBe("true")
  })

  it("opens a decision's directories when it is selected", async () => {
    const { router } = await renderLedger("/p/circus/ledger")
    fireEvent.click(item(/^architecture/))
    expect(visibleItems()).not.toContain("D3SQLite as the only database")
    await act(() => router.navigate("/p/circus/ledger/D3"))
    await advance(50)
    expect(item(/^D3/).getAttribute("aria-current")).toBe("page")
  })
})

describe("a decision", () => {
  it("shows its number, title, directory, update time and body", async () => {
    await renderLedger("/p/circus/ledger/D4")
    const article = screen.getByRole("article")
    expect(within(article).getByRole("heading", { level: 2 }).textContent).toBe("D4Ledger 以 Git 仓库存储")
    expect(article.textContent).toContain("architecture/storage")
    expect(article.textContent).toContain("Updated 10d ago")
    expect(article.textContent).toContain("每个项目的 Ledger 是控制面上的一个裸 Git 仓库")
    expect(within(article).getByRole("link", { name: "palace#D1" }).getAttribute("href")).toBe("/p/palace/ledger/D1")
  })

  it("says when there is no such decision", async () => {
    await renderLedger("/p/circus/ledger/D99")
    expect(screen.getByText("There is no D99 in this Ledger")).toBeTruthy()
    expect(screen.getByRole("link", { name: "All decisions" }).getAttribute("href")).toBe("/p/circus/ledger")
    cleanup()
    await renderLedger("/p/circus/ledger/readme")
    // Anything but a number is never echoed back.
    expect(screen.getByText("There is no such decision in this Ledger")).toBeTruthy()
  })

  it("moves other spellings of a number to its own path", async () => {
    for (const [path, to] of [
      ["/p/circus/ledger/D004?view=history", "/p/circus/ledger/D4?view=history"],
      ["/p/circus/ledger/d4", "/p/circus/ledger/D4"],
      ["/p/circus/ledger/D4%20", "/p/circus/ledger/D4"],
    ]) {
      const { router } = await renderLedger(path)
      expect(at(router)).toBe(to)
      cleanup()
    }
  })

  it("says when it can't be loaded", async () => {
    await renderLedger(
      "/p/circus/ledger/D6",
      withLedger({ getDecision: () => Promise.reject(new ApiError("internal", "boom")) }),
    )
    expect(screen.getByText("Couldn't load D6.")).toBeTruthy()
    expect(screen.getByRole("button", { name: "Try again" })).toBeTruthy()
  })

  it("previews a referenced decision on focus", async () => {
    await renderLedger("/p/circus/ledger/D6")
    const ref = within(screen.getByRole("article")).getByRole("link", { name: "D9" })
    act(() => ref.focus())
    // The card opens after a moment, then loads the decision.
    await advance(400)
    await advance(50)
    const card = document.querySelector('[data-slot="decision-preview"]')
    expect(card?.textContent).toContain("会话转录只追加")
    expect(card?.textContent).toContain("session")
    expect(card?.textContent).toContain("转录是只追加的事件序列")
  })

  it("only follows a reference on a touch screen", async () => {
    const { router } = await renderLedger("/p/circus/ledger/D6")
    const ref = within(screen.getByRole("article")).getByRole("link", { name: "D9" })
    fireEvent.pointerDown(ref, { pointerType: "touch" })
    act(() => ref.focus())
    await advance(450)
    expect(document.querySelector('[data-slot="decision-preview"]')).toBeNull()
    fireEvent.click(ref)
    await advance(50)
    expect(at(router)).toBe("/p/circus/ledger/D9")
  })
})

describe("history and old versions", () => {
  it("lists what each version changed, and loads more", async () => {
    await renderLedger("/p/circus/ledger/D6?view=history")
    const history = screen.getByRole("region", { name: "History of D6" })
    const versions = within(history).getAllByRole("listitem")
    expect(versions.map((v) => v.textContent)).toEqual([
      expect.stringMatching(/session-a1b2Current.*Clarify how long a session waits for its worker.*Content changed.*code /),
      expect.stringMatching(/session-2b91.*Name the worker trade-offs after M3.*Retitled from “Worker design trade-offs”.*Content changed/),
      expect.stringMatching(/import.*Import existing decisions.*Added/),
    ])
    cleanup()

    await renderLedger("/p/circus/ledger/D3?view=history")
    expect(screen.getByRole("region", { name: "History of D3" }).textContent).toContain(
      "Moved from architecture to architecture/storage",
    )
    cleanup()

    await renderLedger("/p/circus/ledger/D0?view=history")
    expect(within(screen.getByRole("region", { name: "History of D0" })).getAllByRole("listitem")).toHaveLength(20)
    fireEvent.click(screen.getByRole("button", { name: "Load more" }))
    await advance(50)
    expect(within(screen.getByRole("region", { name: "History of D0" })).getAllByRole("listitem")).toHaveLength(23)
    expect(screen.queryByRole("button", { name: "Load more" })).toBeNull()
  })

  it("opens an old version, marked as such, with a way back", async () => {
    const { router } = await renderLedger("/p/circus/ledger/D6?view=history")
    const oldest = within(screen.getByRole("region", { name: "History of D6" })).getAllByRole("link").at(-1)!
    fireEvent.click(oldest)
    await advance(50)
    expect(at(router)).toMatch(/^\/p\/circus\/ledger\/D6\?at=[0-9a-f]{40}$/)
    const article = screen.getByRole("article")
    expect(within(article).getByRole("heading", { level: 2 }).textContent).toBe("D6Worker design trade-offs")
    expect(within(article).getByRole("note").textContent).toMatch(/^This is an old version of D6, from .*2026/)
    expect(article.textContent).not.toContain("最多等待 10 分钟")
    // The tree still shows where it is.
    expect(item(/^D6/).getAttribute("aria-current")).toBe("page")

    fireEvent.click(within(article).getByRole("link", { name: "View the current version" }))
    await advance(50)
    expect(at(router)).toBe("/p/circus/ledger/D6")
    expect(screen.queryByRole("note")).toBeNull()
    expect(screen.getByRole("article").textContent).toContain("最多等待 10 分钟")
  })

  it("says when a version doesn't exist, keeping the header, tabs and history", async () => {
    const asked: string[] = []
    const counting = (client: CircusClient): CircusClient => ({
      ...client,
      ledger: {
        ...client.ledger,
        getDecision: (req) => {
          asked.push(req.commit ?? "")
          return client.ledger.getDecision(req)
        },
      },
    })
    // Unknown, malformed, upper case, and the short id the page shows.
    for (const bad of ["0".repeat(40), "garbage", "86D55F1".padEnd(40, "A"), "86d55f1"]) {
      await renderLedger(`/p/circus/ledger/D6?at=${bad}`, counting)
      const article = screen.getByRole("article")
      expect(within(article).getByText("This version of D6 doesn't exist")).toBeTruthy()
      expect(within(article).getByRole("link", { name: "View the current version" }).getAttribute("href")).toBe("/p/circus/ledger/D6")
      expect(within(article).getByRole("heading", { level: 2 }).textContent).toBe("D6M3 worker 设计的七项取舍")
      expect(within(article).getByRole("link", { name: "History" })).toBeTruthy()
      expect(screen.queryByRole("button", { name: "Try again" })).toBeNull()
      cleanup()
    }
    // Only the full lowercase id was asked for.
    expect(asked).toEqual(["0".repeat(40)])

    // The server refusing the commit reads the same.
    await renderLedger(
      `/p/circus/ledger/D6?at=${"a".repeat(40)}`,
      withLedger({ getDecision: () => Promise.reject(new ApiError("invalid_argument", "bad", "commit")) }),
    )
    expect(screen.getByText("This version of D6 doesn't exist")).toBeTruthy()
    cleanup()

    // History doesn't need the version.
    await renderLedger("/p/circus/ledger/D6?view=history&at=garbage")
    expect(within(screen.getByRole("region", { name: "History of D6" })).getAllByRole("listitem")).toHaveLength(3)
  })

  it("links the newest version without a commit and marks the one being viewed", async () => {
    await renderLedger("/p/circus/ledger/D6?view=history")
    let links = within(screen.getByRole("region", { name: "History of D6" })).getAllByRole("link")
    expect(links[0].getAttribute("href")).toBe("/p/circus/ledger/D6")
    expect(links[0].getAttribute("aria-current")).toBe("true")
    const oldest = links[2].getAttribute("href")!.split("at=")[1]
    cleanup()

    await renderLedger(`/p/circus/ledger/D6?view=history&at=${oldest}`)
    links = within(screen.getByRole("region", { name: "History of D6" })).getAllByRole("link")
    expect(links.map((l) => l.getAttribute("aria-current"))).toEqual([null, null, "true"])
    expect(links[2].textContent).toContain("Viewing")
    expect(links[0].textContent).not.toContain("Viewing")
  })

  it("never refetches an old version", async () => {
    const asked: string[] = []
    const counting = (client: CircusClient): CircusClient => ({
      ...client,
      ledger: {
        ...client.ledger,
        getLedger: (req) => {
          asked.push("ledger")
          return client.ledger.getLedger(req)
        },
        getDecision: (req) => {
          asked.push(req.commit || "head")
          return client.ledger.getDecision(req)
        },
      },
    })
    const commit = fakeCommitId("circus:0")
    await renderLedger(`/p/circus/ledger/D6?at=${commit}`, counting)
    expect(asked).toEqual(["ledger", commit])
    // A day later the window regains focus: stale queries refetch, an old version is never stale.
    vi.setSystemTime(Date.now() + 24 * 60 * 60_000)
    act(() => {
      focusManager.setFocused(false)
      focusManager.setFocused(true)
    })
    await advance(50)
    expect(asked).toEqual(["ledger", commit, "ledger"])
    act(() => focusManager.setFocused(undefined))
  })

  it("says moved only when the directory changed", async () => {
    const version = (summary: string, path: string, flags: Partial<DecisionVersion>): DecisionVersion => ({
      commit: fakeCommitId(summary),
      committedAtUnixMs: 0,
      author: "s",
      summary,
      path,
      title: "t",
      added: false,
      contentChanged: false,
      moved: false,
      retitled: false,
      codeCommit: "",
      ...flags,
    })
    await renderLedger(
      "/p/circus/ledger/D6?view=history",
      withLedger({
        listDecisionHistory: () =>
          Promise.resolve({
            versions: [
              version("Rename", "session/D6-new.md", { moved: true, retitled: true }),
              version("Move", "session/D6-old.md", { moved: true }),
              version("Add", "arch/D6-old.md", { added: true }),
            ],
            nextPageToken: "",
          }),
      }),
    )
    const items = within(screen.getByRole("region", { name: "History of D6" })).getAllByRole("listitem")
    expect(items[0].textContent).not.toContain("Moved")
    expect(items[1].textContent).toContain("Moved from arch to session")
  })
})

describe("narrow screens", () => {
  /** Pretends the window is phone-sized for the page's media queries. */
  const narrow = () =>
    vi.spyOn(window, "matchMedia").mockImplementation(
      (query: string) => ({ matches: query.includes("max-width"), media: query, addEventListener() {}, removeEventListener() {} }) as unknown as MediaQueryList,
    )

  it("focus the decision after opening it, and the list's row after going back", async () => {
    narrow()
    const { router } = await renderLedger("/p/circus/ledger")
    fireEvent.click(item(/^D7/))
    await advance(50)
    expect(at(router)).toBe("/p/circus/ledger/D7")
    expect(document.activeElement).toBe(within(screen.getByRole("article")).getByRole("heading", { level: 2 }))

    fireEvent.click(screen.getByRole("link", { name: "All decisions" }))
    await advance(50)
    expect(at(router)).toBe("/p/circus/ledger")
    expect(item(/^D7/).getAttribute("aria-selected")).toBe("true")
    expect(item(/^D7/).getAttribute("aria-current")).toBeNull()
    expect(document.activeElement).toBe(item(/^D7/))
  })
})
