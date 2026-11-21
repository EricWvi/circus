// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen, within } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { MockState } from "@/api/mock/state"
import { ApiError } from "@/api/errors"
import type { CircusClient, RawSessionEvent, SessionDetails, SessionService, WorkspaceStatus } from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { SessionPage } from "@/routes/session-page"
import { advance, renderWithBackend } from "@/test/render"
import { withDefaultProject } from "@/test/workspace-state"

const FOLDER = "~/circus/projects/fresh"
const READY = "/home/eric/circus/projects/fresh"

/** Starts a session in the project's first default location and opens its page. */
async function openNewSession(
  state: MockState,
  { wrapClient, locationIndex = 0 }: { wrapClient?: (client: CircusClient) => CircusClient; locationIndex?: number } = {},
) {
  const page = (
    <SidebarProvider>
      <SessionPage />
    </SidebarProvider>
  )
  const router = createMemoryRouter(
    [
      { path: "/p/:projectId/s/:sessionId", element: page },
      { path: "*", element: null },
    ],
    { initialEntries: ["/"] },
  )
  const result = await renderWithBackend(<RouterProvider router={router} />, { state, wrapClient })
  const created = result.backend.client.sessions.createSession({
    projectId: "fresh",
    locationIndex,
    clientRequestId: crypto.randomUUID(),
  })
  await advance(10)
  const session = await created
  await act(() => router.navigate(`/p/fresh/s/${session.sessionId}`))
  await advance(50)
  return { ...result, router, session }
}

/** A client whose sessions service is the mock's, with some methods replaced. */
const withSessions =
  (patch: (sessions: SessionService) => Partial<SessionService>) =>
  (client: CircusClient): CircusClient => ({ ...client, sessions: { ...client.sessions, ...patch(client.sessions) } })

/** The conversation, apart from the banner and the page header. */
const log = () => within(screen.getByRole("log"))
const badge = () => document.querySelector<HTMLElement>("[data-workspace-badge]")
const announced = () => document.querySelector("[data-workspace-announcement]")?.textContent ?? ""
const banner = () => document.querySelector<HTMLElement>("[data-workspace-state]")
const composerNote = "Claude starts once the folder is ready. Your messages are kept until then."

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  await i18n.changeLanguage("en")
})

/**
 * A session stream the test writes, as the control plane would send it:
 * each connection starts with a keepalive and then gets what `push` sends;
 * `drop` ends the current connection as the server's hourly end does.
 */
function scriptedStream() {
  let push: (e: RawSessionEvent) => void = () => {}
  let drop: () => void = () => {}
  let connections = 0
  const watchSession: SessionService["watchSession"] = async function* () {
    connections++
    yield { seq: 0, tsUnixMs: Date.now(), kind: "unspecified", payloadJson: "", ephemeral: true, truncated: false }
    const queue: (RawSessionEvent | "drop")[] = []
    let wake = () => {}
    push = (e) => {
      queue.push(e)
      wake()
    }
    drop = () => {
      queue.push("drop")
      wake()
    }
    for (;;) {
      const next = queue.shift()
      if (next === "drop") throw new ApiError("unavailable", "stream ended")
      if (next) yield next
      else await new Promise<void>((resolve) => (wake = resolve))
    }
  }
  return {
    watchSession,
    push: (e: RawSessionEvent) => act(() => push(e)),
    drop: () => act(() => drop()),
    connections: () => connections,
  }
}

/** A KIND_WORKSPACE event for "fresh" on devbox; seq 0 is live only. */
const wsEvent = (seq: number, ts: number, payload: object): RawSessionEvent => ({
  seq,
  tsUnixMs: ts,
  kind: "workspace",
  payloadJson: JSON.stringify({ device: "devbox", path: FOLDER, repo_url: "", ...payload }),
  ephemeral: seq === 0,
  truncated: false,
})

/**
 * Opens a new session in "fresh" whose stream is `stream`, and whose
 * GetSession returns the mock's session with the workspace `fetched()`
 * says (the mock's own when undefined).
 */
async function openScripted(
  stream: ReturnType<typeof scriptedStream>,
  fetched: () => Partial<WorkspaceStatus> | undefined,
  { onRead = () => {} }: { onRead?: () => void } = {},
) {
  return openNewSession(withDefaultProject(""), {
    wrapClient: withSessions((mock) => ({
      watchSession: stream.watchSession,
      getSession: async (req) => {
        onRead()
        const s = await mock.getSession(req)
        const patch = fetched()
        return (patch ? { ...s, workspace: { ...s.workspace!, ...patch } } : s) satisfies SessionDetails
      },
    })),
  })
}

describe("the workspace banner", () => {
  it("follows a clone from pending to ready, with the time it takes, and keeps only the outcome", async () => {
    await openNewSession(withDefaultProject("https://example.com/fresh.git"))
    expect(banner()?.dataset.workspaceState).toBe("pending")
    expect(banner()?.textContent).toBe("Preparing the folder will start shortly")
    expect(screen.getByText(composerNote)).toBeTruthy()
    expect(screen.getByRole("textbox")).toHaveProperty("disabled", false)
    // The header says what the session waits for, not "Queued".
    expect(badge()?.textContent).toBe("Preparing folder")
    expect(screen.queryByText("Idle")).toBeNull()

    // Preparing: the elapsed time ticks; the conversation says nothing yet.
    await advance(1_000)
    expect(banner()?.textContent).toBe(`Cloning https://example.com/fresh.git into ${FOLDER}… (0s)`)
    await advance(2_000)
    expect(banner()?.textContent).toBe(`Cloning https://example.com/fresh.git into ${FOLDER}… (2s)`)
    expect(log().queryByText(/Cloning/)).toBeNull()
    expect(screen.getByText("No messages yet")).toBeTruthy()

    await advance(1_000)
    expect(banner()).toBeNull()
    expect(badge()).toBeNull()
    expect(screen.queryByText(composerNote)).toBeNull()
    expect(log().getByText(`Cloned https://example.com/fresh.git into ${READY}`)).toBeTruthy()
    expect(log().queryByText(/Cloning/)).toBeNull()
  })

  it("says what it creates without a repository", async () => {
    await openNewSession(withDefaultProject(""))
    await advance(1_000)
    expect(banner()?.textContent).toBe(`Creating ${FOLDER}… (0s)`)
    await advance(2_500)
    expect(log().getByText(`Created ${READY}`)).toBeTruthy()
  })

  it("keeps a message until the folder is ready, then Claude answers it", async () => {
    await openNewSession(withDefaultProject(""))
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "Set up the repo" } })
    fireEvent.click(screen.getByRole("button", { name: "Submit" }))
    await advance(500)
    expect(log().getByText("Set up the repo")).toBeTruthy()
    expect(screen.getByText("Sent")).toBeTruthy()
    // Not "waiting for a worker": the folder is what it waits for.
    expect(screen.queryByText("Waiting for a worker to pick this up…")).toBeNull()
    expect(badge()?.textContent).toBe("Preparing folder")
    await advance(5_000)
    expect(screen.queryByText("Sent")).toBeNull()
    expect(log().getByText(`Created ${READY}`)).toBeTruthy()
  })

  it("updates live while it waits for the device, without lines in the conversation", async () => {
    await openNewSession(withDefaultProject("https://example.com/offline.git"))
    await advance(1_000)
    expect(banner()?.textContent).toBe("Waiting for devbox to come online to prepare the folder")
    expect(screen.getByText("No messages yet")).toBeTruthy()
    await advance(6_000)
    expect(banner()?.textContent).toBe("Preparing the folder will start shortly")
    await advance(1_000)
    expect(banner()?.dataset.workspaceState).toBe("preparing")
    expect(screen.getByText("No messages yet")).toBeTruthy()
  })

  it("waits for an offline device, saying so once", async () => {
    const state = withDefaultProject("")
    state.devices.find((d) => d.name === "devbox")!.online = false
    await openNewSession(state)
    await advance(1_000)
    expect(banner()?.textContent).toBe("Waiting for devbox to come online to prepare the folder")
    // Not the notice that Claude can still reply: no turn runs.
    expect(screen.queryByText(/Claude can still reply/)).toBeNull()
    // The badge says the same, in a word or two (only its icon on narrow screens).
    expect(badge()?.dataset.workspaceBadge).toBe("waitingDevice")
    expect(badge()?.textContent).toBe("Waiting for device")
    expect(badge()?.getAttribute("title")).toBe("Waiting for device")
    expect(within(badge()!).getByText("Waiting for device").className).toContain("max-sm:sr-only")
  })

  it("asks to update an outdated device", async () => {
    await openNewSession(withDefaultProject("https://example.com/outdated.git"))
    await advance(1_000)
    expect(banner()?.textContent).toBe("Update circus on devbox to prepare the folder")
    expect(badge()?.textContent).toBe("Needs update")
  })

  it("explains a failure, shows the device's words on request, and retries", async () => {
    const retries: string[] = []
    await openNewSession(withDefaultProject("https://example.com/clone-fails.git"), {
      wrapClient: withSessions((mock) => ({
        retryWorkspace: (req) => {
          retries.push(req.sessionId)
          return mock.retryWorkspace(req)
        },
      })),
    })
    await advance(3_500)
    const failed = banner()!
    expect(failed.dataset.workspaceState).toBe("failed")
    expect(within(failed).getByText(`Couldn't prepare ${FOLDER}: git clone failed`)).toBeTruthy()
    expect(screen.getByText(composerNote)).toBeTruthy()
    expect(badge()?.textContent).toBe("Folder failed")
    expect(badge()?.className).toContain("text-destructive")
    // The banner says it; the conversation doesn't, yet.
    expect(log().queryByText(/Couldn't prepare/)).toBeNull()

    const details = within(failed).getByRole("button", { name: "Details" })
    expect(details.getAttribute("aria-expanded")).toBe("false")
    fireEvent.click(details)
    await advance(0)
    expect(within(failed).getByText(/Repository not found/).className).toContain("font-mono")

    fireEvent.click(within(failed).getByRole("button", { name: "Retry" }))
    await advance(10)
    expect(retries).toHaveLength(1)
    expect(banner()?.textContent).toBe("Preparing the folder will start shortly")
    // Superseded by the retry: now the failure is history.
    expect(log().getByText(`Couldn't prepare ${FOLDER}: git clone failed`)).toBeTruthy()
    await advance(3_500)
    expect(banner()).toBeNull()
    expect(log().getByText(`Couldn't prepare ${FOLDER}: git clone failed`)).toBeTruthy()
    expect(log().getByText(`Cloned https://example.com/clone-fails.git into ${READY}`)).toBeTruthy()
  })

  it("puts the remote of a mismatch in the reason, without details", async () => {
    await openNewSession(withDefaultProject("https://example.com/mismatch.git"))
    await advance(3_500)
    expect(banner()?.textContent).toContain(
      `Couldn't prepare ${FOLDER}: the folder is a repository of a different remote (git@github.com:someone-else/fork.git)`,
    )
    expect(within(banner()!).queryByRole("button", { name: "Details" })).toBeNull()
  })

  it("shows nothing for a folder that was ready when the session started", async () => {
    const { backend, router, session: first } = await openNewSession(withDefaultProject("https://example.com/fresh.git"))
    await advance(4_000)
    // A second session in the same place.
    const created = backend.client.sessions.createSession({ projectId: "fresh", locationIndex: 0, clientRequestId: "k2" })
    await advance(10)
    const second = await created
    expect(second.workspace?.state).toBe("ready")
    expect(second.sessionId).not.toBe(first.sessionId)
    await act(() => router.navigate(`/p/fresh/s/${second.sessionId}`))
    await advance(100)
    expect(banner()).toBeNull()
    expect(badge()).toBeNull()
    expect(screen.queryByText(composerNote)).toBeNull()
    expect(screen.getByText("No messages yet")).toBeTruthy()
  })

  it("shows nothing for a stopped session whose folder failed", async () => {
    const { state, session } = await openNewSession(withDefaultProject("https://example.com/not-empty.git"))
    await advance(3_500)
    expect(banner()?.dataset.workspaceState).toBe("failed")
    state.sessions.find((s) => s.sessionId === session.sessionId)!.state = "stopped"
    await advance(5_000)
    expect(screen.getByText("This session is stopped.")).toBeTruthy()
    expect(banner()).toBeNull()
    expect(badge()).toBeNull()
    expect(screen.queryByText(composerNote)).toBeNull()
  })

  it("announces each state, and that the folder is ready", async () => {
    await openNewSession(withDefaultProject(""))
    expect(announced()).toBe("Preparing the folder will start shortly")
    await advance(1_000)
    expect(announced()).toBe(`Creating ${FOLDER}`)
    await advance(2_500)
    expect(announced()).toBe(`Created ${READY}`)
  })

  it("counts the time on the server's clock", async () => {
    const stream = scriptedStream()
    // The browser's clock runs a minute ahead of the server's.
    const skew = 60_000
    await openScripted(stream, () => ({ state: "pending", updatedAtUnixMs: 1 }))
    const serverNow = Date.now() - skew
    stream.push({ seq: 0, tsUnixMs: serverNow, kind: "unspecified", payloadJson: "", ephemeral: true, truncated: false })
    stream.push(wsEvent(1, serverNow, { state: "preparing" }))
    await advance(2_000)
    expect(banner()?.textContent).toBe(`Creating ${FOLDER}… (2s)`)
  })

  it("learns the server's clock from a recorded change sent as it happens", async () => {
    const stream = scriptedStream()
    const skew = 120_000
    await openScripted(stream, () => ({ state: "pending", updatedAtUnixMs: 1 }))
    // No live event says the server's time; the preparing change, recorded
    // and sent after the replay, does.
    await advance(1_000)
    stream.push(wsEvent(1, Date.now() - skew, { state: "preparing" }))
    await advance(10)
    expect(banner()?.textContent).toBe(`Creating ${FOLDER}… (0s)`)
    await advance(3_000)
    expect(banner()?.textContent).toBe(`Creating ${FOLDER}… (3s)`)
  })
})

describe("what the server streams", () => {
  it("reads the session again on a recorded change, and only then", async () => {
    const stream = scriptedStream()
    let reads = 0
    const now = Date.now()
    await openScripted(stream, () => ({ state: "pending", updatedAtUnixMs: now }), { onRead: () => reads++ })
    // Between the session's own polls, which come every five seconds.
    let before = reads
    stream.push(wsEvent(0, now + 10, { state: "pending", waiting: "device_offline" }))
    await advance(100)
    expect(banner()?.textContent).toBe("Waiting for devbox to come online to prepare the folder")
    expect(reads).toBe(before)
    before = reads
    stream.push(wsEvent(1, now + 20, { state: "preparing" }))
    await advance(100)
    expect(reads).toBe(before + 1)
  })

  it("follows a failure, the retry and the waits after it", async () => {
    const stream = scriptedStream()
    const now = Date.now()
    await openScripted(stream, () => ({ state: "pending", updatedAtUnixMs: 1 }))
    stream.push(wsEvent(1, now, { state: "preparing" }))
    stream.push(wsEvent(2, now + 1, { state: "failed", error: "permission_denied" }))
    await advance(10)
    expect(banner()?.textContent).toContain(`Couldn't prepare ${FOLDER}: permission denied`)
    expect(log().queryByText(/Couldn't prepare/)).toBeNull()
    // Retried elsewhere: recorded as pending, then a live wait.
    stream.push(wsEvent(3, now + 2, { state: "pending", waiting: "retry" }))
    stream.push(wsEvent(0, now + 3, { state: "pending", waiting: "daemon_outdated" }))
    await advance(10)
    expect(banner()?.textContent).toBe("Update circus on devbox to prepare the folder")
    expect(log().getByText(`Couldn't prepare ${FOLDER}: permission denied`)).toBeTruthy()
  })

  it("keeps a fetch newer than the last event, and lets it correct a missed live event", async () => {
    const stream = scriptedStream()
    const now = Date.now()
    let fetched: Partial<WorkspaceStatus> = { state: "pending", waiting: "unspecified", updatedAtUnixMs: now }
    await openScripted(stream, () => fetched)
    stream.push(wsEvent(0, now + 10, { state: "pending", waiting: "device_offline" }))
    await advance(10)
    expect(banner()?.textContent).toBe("Waiting for devbox to come online to prepare the folder")
    // The live event that the device is back was lost; the next read has it.
    fetched = { state: "pending", waiting: "retry", updatedAtUnixMs: now + 20 }
    await advance(5_000)
    expect(banner()?.textContent).toBe("Preparing the folder will start shortly")
    // An event of the same time as the read is the same change: no flip-flop.
    stream.push(wsEvent(0, now + 20, { state: "pending", waiting: "retry" }))
    await advance(5_000)
    expect(banner()?.textContent).toBe("Preparing the folder will start shortly")
  })

  it("keeps the live state across a reconnect, until a newer read", async () => {
    const stream = scriptedStream()
    const now = Date.now()
    let fetched: Partial<WorkspaceStatus> = { state: "pending", waiting: "unspecified", updatedAtUnixMs: now }
    await openScripted(stream, () => fetched)
    stream.push(wsEvent(0, now + 10, { state: "pending", waiting: "daemon_outdated" }))
    await advance(10)
    expect(banner()?.textContent).toBe("Update circus on devbox to prepare the folder")

    // The stream ends after a while; it reopens at once and the session is read again.
    await advance(11_000)
    const connections = stream.connections()
    stream.drop()
    await advance(100)
    expect(stream.connections()).toBe(connections + 1)
    expect(banner()?.textContent).toBe("Update circus on devbox to prepare the folder")
    fetched = { state: "pending", waiting: "device_offline", updatedAtUnixMs: now + 30 }
    await advance(5_000)
    expect(banner()?.textContent).toBe("Waiting for devbox to come online to prepare the folder")
  })

  it("speaks Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await openNewSession(withDefaultProject("https://example.com/fresh.git"))
    expect(badge()?.textContent).toBe("准备工作目录")
    await advance(2_000)
    expect(banner()?.textContent).toBe(`正在把 https://example.com/fresh.git 克隆到 ${FOLDER}…（1 秒）`)
    expect(screen.getByText("工作目录就绪后 Claude 才会开始，在此之前发送的消息会保留。")).toBeTruthy()
  })
})
