// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen, within } from "@testing-library/react"
import { Profiler } from "react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { addQueueDemo, addWorkerDemo, seedState, type MockState } from "@/api/mock/state"
import type { CircusClient, RawSessionEvent, SessionDetails, SessionService } from "@/api/types"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import { SessionPage } from "@/routes/session-page"
import { HEALTHY_STREAM_MS } from "@/state/session-watch"
import { fakeWorkerEvents, type Scenario } from "@/test/fake-worker-events"
import { advance, renderWithBackend } from "@/test/render"

type SendReq = Parameters<SessionService["sendMessage"]>[0]

async function renderSession(
  path: string,
  {
    state = seedState(),
    wrapClient,
    onCommit,
  }: { state?: MockState; wrapClient?: (client: CircusClient) => CircusClient; onCommit?: () => void } = {},
) {
  const page = (
    <SidebarProvider>
      <Profiler id="session" onRender={() => onCommit?.()}>
        <SessionPage />
      </Profiler>
    </SidebarProvider>
  )
  const router = createMemoryRouter([{ path: "/p/:projectId/s/:sessionId", element: page }], {
    initialEntries: [path],
  })
  const result = await renderWithBackend(<RouterProvider router={router} />, { state, wrapClient })
  await advance(50)
  return { ...result, router }
}

/** A stream that fails with `error` before it sends anything. */
function failingStream(error: ApiError): AsyncIterable<RawSessionEvent> {
  return {
    [Symbol.asyncIterator]: () => ({ next: () => Promise.reject(error) }),
  }
}

/** A client whose sessions service is the mock's, with some methods replaced. */
const withSessions =
  (patch: (sessions: SessionService) => Partial<SessionService>) =>
  (client: CircusClient): CircusClient => ({ ...client, sessions: { ...client.sessions, ...patch(client.sessions) } })

/**
 * A stream the test writes: each connection starts with a keepalive and
 * then gets what `push` sends; `drop` ends it as the server's hourly end does.
 */
function scriptedStream() {
  const control = { push: (_e: RawSessionEvent) => {}, drop: () => {}, connections: 0 }
  const watchSession: SessionService["watchSession"] = async function* () {
    control.connections++
    yield { seq: 0, tsUnixMs: Date.now(), kind: "unspecified", payloadJson: "", ephemeral: true, truncated: false }
    const queue: (RawSessionEvent | "drop")[] = []
    let wake = () => {}
    control.push = (e) => {
      queue.push(e)
      wake()
    }
    control.drop = () => {
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
  return { control, watchSession }
}
// A question card has text fields of its own.
const composer = () => document.querySelector("textarea")!
async function type(text: string) {
  fireEvent.change(composer(), { target: { value: text } })
  fireEvent.click(screen.getByRole("button", { name: "Submit" }))
  await advance(0)
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  await i18n.changeLanguage("en")
})

describe("sending", () => {
  it("shows a message at once, then sent, then nothing once the agent has it", async () => {
    let release: () => void = () => {}
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        sendMessage: async (req) => {
          await new Promise<void>((resolve) => (release = resolve))
          return mock.sendMessage(req)
        },
      })),
    })
    await type("Check the lockfile")
    expect(screen.getByText("Check the lockfile")).toBeTruthy()
    expect(screen.getByText("Sending…")).toBeTruthy()
    expect(composer().value).toBe("")

    act(() => release())
    await advance(10)
    expect(screen.getByText("Sent")).toBeTruthy()
    expect(screen.getAllByText("Check the lockfile")).toHaveLength(1)

    // The worker hands it to the agent at the turn's first step.
    await advance(150)
    expect(screen.queryByText("Sent")).toBeNull()
    expect(screen.queryByText("Sending…")).toBeNull()
    expect(screen.getAllByText("Check the lockfile")).toHaveLength(1)
  })

  it("keeps a failed message with its reason, and retries it with the same id and text", async () => {
    const calls: SendReq[] = []
    let fail = true
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        sendMessage: async (req) => {
          calls.push(req)
          if (fail) throw new ApiError("resource_exhausted", "too many")
          return mock.sendMessage(req)
        },
      })),
    })
    await type("Run the tests")
    await advance(10)
    expect(screen.getByText("Not sent: too many messages are waiting to be delivered.")).toBeTruthy()

    fail = false
    fireEvent.click(screen.getByRole("button", { name: "Retry" }))
    await advance(10)
    expect(calls).toHaveLength(2)
    expect(calls[1]).toEqual(calls[0])
    expect(screen.queryByText(/Not sent/)).toBeNull()
    expect(screen.getAllByText("Run the tests")).toHaveLength(1)
  })

  it("shows the message once when the stream brings it before the server's answer", async () => {
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        sendMessage: async (req) => {
          const answer = await mock.sendMessage(req)
          await new Promise((resolve) => setTimeout(resolve, 2_000))
          return answer
        },
      })),
    })
    await type("Faster than the answer")
    await advance(500)
    expect(screen.getAllByText("Faster than the answer")).toHaveLength(1)
    // Delivered already: no stage shows, though the call hasn't answered.
    expect(screen.queryByText("Sending…")).toBeNull()
    expect(screen.queryByText("Sent")).toBeNull()
    await advance(2_000)
    expect(screen.getAllByText("Faster than the answer")).toHaveLength(1)
  })

  it("keeps sending possible while a stream is briefly reopened", async () => {
    let calls = 0
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        watchSession: (req) => {
          calls++
          if (calls === 1) {
            return (async function* () {
              for await (const event of mock.watchSession(req)) {
                yield event
                await new Promise((resolve) => setTimeout(resolve, HEALTHY_STREAM_MS))
                throw new ApiError("unavailable", "stream lifetime")
              }
            })()
          }
          // The new stream takes a while to open.
          return (async function* () {
            await new Promise((resolve) => setTimeout(resolve, 3_000))
            yield* mock.watchSession(req)
          })()
        },
      })),
    })
    await advance(HEALTHY_STREAM_MS + 1_000)
    expect(calls).toBe(2)
    fireEvent.change(composer(), { target: { value: "still works" } })
    expect(screen.getByRole("button", { name: "Submit" })).toHaveProperty("disabled", false)
    // Once the loss lasts long enough to tell, sending waits for the connection.
    await advance(1_500)
    expect(screen.getByRole("button", { name: "Submit" })).toHaveProperty("disabled", true)
  })

  it("keeps a message the full view won't show, saying so", async () => {
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        sendMessage: async (req) => ({ ...(await mock.sendMessage({ ...req, clientMsgId: "elsewhere" })), eventSeq: 0 }),
      })),
    })
    await type("Into a full view")
    await advance(10)
    expect(screen.getByText("Sent. This session's history is full, so it won't show here after a reload.")).toBeTruthy()
  })

  it("says why a stopped session refused a message", async () => {
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions(() => ({
        sendMessage: () => Promise.reject(new ApiError("failed_precondition", "stopped")),
      })),
    })
    await type("Anything")
    await advance(10)
    expect(screen.getByText("Not sent: the session is stopped.")).toBeTruthy()
  })
})

describe("a reply", () => {
  it("streams, runs a tool, and ends idle", async () => {
    await renderSession("/p/circus/s/web-vite")
    await type("hello")
    await advance(300)
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy()
    await advance(5_000)
    expect(screen.getByText(/this is the mock backend/)).toBeTruthy()
    expect(screen.getByText(/^Bash\s+git status --short$/)).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull()
  })
})

describe("approvals", () => {
  it("approves the call through the server", async () => {
    const resolved: unknown[] = []
    await renderSession("/p/circus/s/fix-auth-refresh", {
      wrapClient: withSessions((mock) => ({
        resolveApproval: (req) => {
          resolved.push(req)
          return mock.resolveApproval(req)
        },
      })),
    })
    fireEvent.click(screen.getByRole("button", { name: "Approve" }))
    await advance(5_000)
    expect(resolved).toEqual([{ approvalId: expect.any(String), approve: true }])
    expect(screen.queryByRole("button", { name: "Approve" })).toBeNull()
    // The retried call ran.
    expect(screen.getByText(/Pushed\. The branch is on the remote now\./)).toBeTruthy()
  })
})

describe("questions", () => {
  const resolvedBy = (resolved: unknown[]) =>
    withSessions((mock) => ({
      resolveApproval: (req) => {
        resolved.push(req)
        return mock.resolveApproval(req)
      },
    }))
  const card = () => screen.getByRole("region", { name: "Claude has 2 questions" })

  it("asks, takes the answers to the server, and shows them", async () => {
    const resolved: unknown[] = []
    await renderSession("/p/circus/s/web-vite", { wrapClient: resolvedBy(resolved) })
    await type("Ask me a question first")
    await advance(5_000)
    expect(screen.getByText("Needs an answer")).toBeTruthy()
    const form = within(card())
    const submit = form.getByRole("button", { name: "Send answers" })
    expect(submit).toHaveProperty("disabled", true)
    fireEvent.click(form.getByRole("radio", { name: /^Tabs/ }))
    // The chosen option's preview shows.
    expect(form.getByLabelText("Preview of Tabs").textContent).toContain("General")
    fireEvent.click(form.getByRole("checkbox", { name: /^Unit tests/ }))
    fireEvent.click(form.getByRole("checkbox", { name: /^Lint/ }))
    fireEvent.change(form.getByRole("textbox", { name: "Your own answer to: Which checks should run before merging?" }), {
      target: { value: "and e2e" },
    })
    expect(submit).toHaveProperty("disabled", false)
    fireEvent.click(submit)
    await advance(5_000)
    expect(resolved).toEqual([
      {
        approvalId: expect.any(String),
        approve: true,
        answers: [
          { question: "Which layout should the settings page use?", selected: ["Tabs"], other: "" },
          { question: "Which checks should run before merging?", selected: ["Unit tests", "Lint"], other: "and e2e" },
        ],
      },
    ])
    const summary = within(card())
    expect(summary.getByText("Answered")).toBeTruthy()
    expect(summary.getByText("Unit tests, Lint, and e2e")).toBeTruthy()
    expect(summary.queryByRole("button")).toBeNull()
    // The answers went to Claude as the user's message, which started its turn.
    const answer = within(screen.getByRole("group", { name: "Your answer" }))
    expect(answer.getByText("Tabs")).toBeTruthy()
    expect(screen.getByText(/Thanks, I'll go with your answers\./)).toBeTruthy()
    expect(screen.queryByText("Needs an answer")).toBeNull()
  })

  it("dismisses the questions", async () => {
    const resolved: unknown[] = []
    await renderSession("/p/circus/s/web-vite", { wrapClient: resolvedBy(resolved) })
    await type("Any question?")
    await advance(5_000)
    fireEvent.click(within(card()).getByRole("button", { name: "Dismiss" }))
    await advance(1_000)
    expect(resolved).toEqual([{ approvalId: expect.any(String), approve: false }])
    expect(within(card()).getByText("Dismissed")).toBeTruthy()
    expect(screen.queryByRole("group", { name: "Your answer" })).toBeNull()
  })

  it("replaces a question still pending with a newer one", async () => {
    await renderSession("/p/circus/s/web-vite")
    await type("A question")
    await advance(5_000)
    await type("Another question")
    await advance(5_000)
    const cards = screen.getAllByRole("region", { name: "Claude has 2 questions" })
    expect(cards.map((c) => c.getAttribute("data-question"))).toEqual(["superseded", "pending"])
    expect(within(cards[0]).getByText("Replaced by a newer question")).toBeTruthy()
  })

  it("stays disabled while its answers are on their way, and comes back when they fail", async () => {
    const calls: unknown[] = []
    let fail = true
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        resolveApproval: async (req) => {
          calls.push(req)
          // The first call fails after a while; the server never got it.
          await new Promise((resolve) => setTimeout(resolve, 1_000))
          if (fail) throw new ApiError("unavailable", "The server is unreachable.")
          return mock.resolveApproval(req)
        },
      })),
    })
    await type("A question")
    await advance(5_000)
    const form = within(card())
    fireEvent.click(form.getByRole("radio", { name: /^Tabs/ }))
    fireEvent.click(form.getByRole("checkbox", { name: /^Lint/ }))
    fireEvent.click(form.getByRole("button", { name: "Send answers" }))
    await advance(100)
    // A second answer, or a dismissal, can't follow the first one.
    expect(form.getByRole("button", { name: "Send answers" })).toHaveProperty("disabled", true)
    expect(form.getByRole("button", { name: "Dismiss" })).toHaveProperty("disabled", true)
    expect(form.getByRole("radio", { name: /^Sidebar/ })).toHaveProperty("disabled", true)
    fireEvent.click(form.getByRole("button", { name: "Dismiss" }))
    expect(calls).toHaveLength(1)
    await advance(1_000)
    expect(screen.getByRole("alert").textContent).toBe("Can't reach the server. Try again.")
    expect(form.getByRole("button", { name: "Send answers" })).toHaveProperty("disabled", false)

    fail = false
    fireEvent.click(form.getByRole("button", { name: "Send answers" }))
    await advance(500)
    // Sent: still disabled until the stream says it was answered.
    expect(form.getByRole("button", { name: "Send answers" })).toHaveProperty("disabled", true)
    await advance(5_000)
    expect(card().getAttribute("data-question")).toBe("answered")
    expect(calls).toHaveLength(2)
  })
})

describe("recorded questions", () => {
  const ask = fakeWorkerEvents("ask")
  const pendingAt = ask.findIndex((e) => e.kind === "approval_pending")
  const waiting = { serverState: "running", state: "awaiting_approval", pendingApprovalCount: 1, turnCount: 1 } as const

  it("shows a pending question in the header, not Working… under the card", async () => {
    await renderRecorded("ask", { upTo: pendingAt + 1, startMs: Date.now(), session: waiting })
    expect(screen.getByText("Needs an answer")).toBeTruthy()
    expect(screen.queryByText("Working…")).toBeNull()
    expect(screen.getByRole("radio", { name: /^Red/ })).toBeTruthy()
  })

  it("can't be answered once the session is stopped", async () => {
    const resolved = ask.findIndex((e) => e.kind === "approval_resolved")
    await renderRecorded("ask", { upTo: resolved, session: { serverState: "stopped", state: "stopped" } })
    const form = within(screen.getByRole("region", { name: "Claude has 2 questions" }))
    expect(form.getByRole("button", { name: "Dismiss" })).toHaveProperty("disabled", true)
    expect(form.getByRole("radio", { name: /^Red/ })).toHaveProperty("disabled", true)
  })

  /** A tool approval and then a question pending together, the approval requested at `approvalAtMs`. */
  async function renderBoth(approvalAtMs: number, resolveApproval?: SessionService["resolveApproval"]) {
    const approve = fakeWorkerEvents("approve", approvalAtMs)
    const tool = approve.slice(0, approve.findIndex((e) => e.kind === "approval_pending") + 1)
    const offset = tool.at(-1)!.seq
    const question = fakeWorkerEvents("ask", Date.now())
      .slice(3, pendingAt + 1)
      // Both scenarios number their tool use alike: the question's gets an id of its own.
      .map((e) => ({ ...e, payloadJson: e.payloadJson.replaceAll(/toolu_[0-9a-f-]+/g, "toolu_question") }))
      .map((e) => (e.seq > 0 ? { ...e, seq: e.seq + offset } : e))
    const events = [...tool, ...question]
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        watchSession: ({ signal }) => replayStream(events, signal),
        getSession: async (req) => ({ ...(await mock.getSession(req)), ...waiting, pendingApprovalCount: 2 }),
        ...(resolveApproval ? { resolveApproval } : {}),
      })),
    })
  }

  /** Resolves approvals as `outcome` says for each call, after `ms`, recording the calls. */
  function resolver(outcome: (req: { approve: boolean; answers?: unknown[] }) => { ms: number; error?: ApiError }) {
    const calls: string[] = []
    const resolve: SessionService["resolveApproval"] = async (req) => {
      calls.push(req.answers ? "answers" : `decide:${req.approve}`)
      const { ms, error } = outcome(req)
      await new Promise((done) => setTimeout(done, ms))
      if (error) throw error
    }
    return { calls, resolve }
  }
  const unreachable = new ApiError("unavailable", "The server is unreachable.")
  const stale = new ApiError("failed_precondition", "The approval expired.")
  const answerAndDeny = async () => {
    const form = within(screen.getByRole("region", { name: "Claude has 2 questions" }))
    fireEvent.click(form.getByRole("radio", { name: /^Blue/ }))
    fireEvent.click(form.getByRole("checkbox", { name: /^Export/ }))
    return form
  }

  it("shows a failure of the answers and takes the card back, though a tool decision came in between", async () => {
    const { calls, resolve } = resolver((req) => (req.answers ? { ms: 1_000, error: unreachable } : { ms: 0 }))
    await renderBoth(Date.now(), resolve)
    const form = await answerAndDeny()
    fireEvent.click(form.getByRole("button", { name: "Send answers" }))
    await advance(100)
    fireEvent.click(screen.getByRole("button", { name: "Deny" }))
    await advance(3_000)
    expect(calls).toEqual(["answers", "decide:false"])
    expect(screen.getByText("Can't reach the server. Try again.")).toBeTruthy()
    expect(form.getByRole("button", { name: "Send answers" })).toHaveProperty("disabled", false)
  })

  it("says a question no longer waits when the server refuses its answers as stale", async () => {
    const { resolve } = resolver(() => ({ ms: 0, error: stale }))
    await renderBoth(Date.now(), resolve)
    const form = await answerAndDeny()
    fireEvent.click(form.getByRole("button", { name: "Send answers" }))
    await advance(100)
    expect(screen.getByText("This question no longer waits for an answer.")).toBeTruthy()
    expect(form.getByRole("button", { name: "Send answers" })).toHaveProperty("disabled", false)
  })

  it("shows a failure of a tool decision, though answers were sent after it", async () => {
    const { calls, resolve } = resolver((req) => (req.answers ? { ms: 0 } : { ms: 1_000, error: stale }))
    await renderBoth(Date.now(), resolve)
    const form = await answerAndDeny()
    fireEvent.click(screen.getByRole("button", { name: "Deny" }))
    await advance(100)
    fireEvent.click(form.getByRole("button", { name: "Send answers" }))
    await advance(3_000)
    expect(calls).toEqual(["decide:false", "answers"])
    expect(screen.getByText("That no longer applies; the session moved on.")).toBeTruthy()
    // The answers went through: their card waits for the stream to say so.
    expect(form.getByRole("button", { name: "Send answers" })).toHaveProperty("disabled", true)
  })

  it("says it needs approval while a tool approval waits beside a question", async () => {
    await renderBoth(Date.now())
    expect(screen.getByText("Needs approval")).toBeTruthy()
  })

  it("says it needs an answer once the tool approval beside the question lapsed", async () => {
    await renderBoth(Date.now() - 16 * 60_000)
    expect(screen.getByText("Needs an answer")).toBeTruthy()
  })
})

describe("approvals in a stopped session", () => {
  it("can't be decided any more", async () => {
    const state = seedState()
    state.sessions.find((x) => x.sessionId === "fix-auth-refresh")!.state = "stopped"
    await renderSession("/p/circus/s/fix-auth-refresh", { state })
    expect(screen.getByRole("button", { name: "Approve" })).toHaveProperty("disabled", true)
  })
})

describe("interrupted turn", () => {
  it("continues with the continue prompt in the active language", async () => {
    await i18n.changeLanguage("zh-CN")
    const sent: SendReq[] = []
    // The session runs on eric-win's worker, offline in the seed.
    const state = seedState()
    state.workers.find((w) => w.name === "eric-win")!.online = true
    await renderSession("/p/toolbox/s/dotfiles-go", {
      state,
      wrapClient: withSessions((mock) => ({
        sendMessage: (req) => {
          sent.push(req)
          return mock.sendMessage(req)
        },
      })),
    })
    fireEvent.click(screen.getByRole("button", { name: "继续" }))
    await advance(10)
    expect(sent).toMatchObject([{ text: "继续" }])
  })

  it("retries once with the turn's message, as a new message", async () => {
    const sent: SendReq[] = []
    // The session runs on eric-win's worker, offline in the seed.
    const state = seedState()
    state.workers.find((w) => w.name === "eric-win")!.online = true
    await renderSession("/p/toolbox/s/dotfiles-go", {
      state,
      wrapClient: withSessions((mock) => ({
        sendMessage: (req) => {
          sent.push(req)
          return mock.sendMessage(req)
        },
      })),
    })
    const retry = screen.getByRole("button", { name: "Retry" })
    fireEvent.click(retry)
    fireEvent.click(retry)
    await advance(10)
    expect(sent).toMatchObject([{ text: "Rewrite install.sh in Go" }])
    expect(screen.getAllByText("Rewrite install.sh in Go")).toHaveLength(2)
    await advance(1_000)
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull()
    expect(screen.getByText("Turn interrupted")).toBeTruthy()
  })
})

describe("the stream", () => {
  it("reconnects after the last event it applied, without showing anything twice", async () => {
    const afterSeqs: number[] = []
    let breakFirst = true
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        watchSession: (req) => {
          afterSeqs.push(req.afterSeq)
          const first = breakFirst
          breakFirst = false
          return (async function* () {
            let n = 0
            for await (const event of mock.watchSession(req)) {
              yield event
              // The first stream serves a while, then ends like the server's hourly end.
              if (first && ++n === 4) {
                await new Promise((resolve) => setTimeout(resolve, HEALTHY_STREAM_MS))
                throw new ApiError("unavailable", "stream lifetime")
              }
            }
          })()
        },
      })),
    })
    const bubbles = () => document.querySelectorAll(".is-user").length
    const before = bubbles()
    expect(before).toBeGreaterThan(0)
    expect(afterSeqs).toEqual([0])
    await advance(HEALTHY_STREAM_MS + 50)
    // Reopened at once, after what it had applied; no notice, nothing twice.
    expect(afterSeqs).toEqual([0, 3])
    expect(bubbles()).toBe(before)
    expect(screen.queryByRole("button", { name: "Retry now" })).toBeNull()
  })

  it("shows the reconnect banner once the loss lasts", async () => {
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions(() => ({
        watchSession: () => failingStream(new ApiError("unavailable", "down")),
      })),
    })
    await advance(2_500)
    expect(screen.getByRole("button", { name: "Retry now" })).toBeTruthy()
    expect(screen.getByRole("status").textContent).toBe("Reconnecting…")
  })

  it("stops watching a session that is gone", async () => {
    let opened = 0
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions(() => ({
        watchSession: () => {
          opened++
          return failingStream(new ApiError("not_found", "gone"))
        },
      })),
    })
    await advance(60_000)
    expect(opened).toBe(1)
    expect(screen.getByText("This session no longer exists.")).toBeTruthy()
  })
})

/** A stream that sends a keepalive and `events`, then stays open. */
function replayStream(events: RawSessionEvent[], signal: AbortSignal): AsyncIterable<RawSessionEvent> {
  return (async function* () {
    yield { seq: 0, tsUnixMs: Date.now(), kind: "unspecified", payloadJson: "", ephemeral: true, truncated: false }
    yield* events
    await new Promise((resolve) => signal.addEventListener("abort", resolve, { once: true }))
  })()
}

/** The session page fed a recorded scenario (up to `upTo` events), with the session read as `session` says. */
async function renderRecorded(
  scenario: Scenario,
  {
    upTo,
    startMs,
    session = {},
  }: { upTo?: number; startMs?: number; session?: Partial<SessionDetails> } = {},
) {
  const events = fakeWorkerEvents(scenario, startMs).slice(0, upTo)
  return renderSession("/p/circus/s/web-vite", {
    wrapClient: withSessions((mock) => ({
      watchSession: ({ signal }) => replayStream(events, signal),
      getSession: async (req) => ({ ...(await mock.getSession(req)), ...session }),
    })),
  })
}

describe("recorded fake worker sessions", () => {
  it("ends the working state of a turn without RESULT once the server counts it over", async () => {
    const events = fakeWorkerEvents("broken")
    const open = events.findIndex((e) => e.kind === "error")
    // The server's state still from before the turn: the stream decides.
    await renderRecorded("broken", { upTo: open, session: { serverState: "queued", state: "queued", turnCount: 1 } })
    expect(screen.getByRole("button", { name: "Stop" })).toBeTruthy()
    expect(screen.getByText("Working…")).toBeTruthy()
    cleanup()
    // The server says the turn is over (it ended without RESULT).
    await renderRecorded("broken", { upTo: open, session: { serverState: "idle", state: "idle", turnCount: 2 } })
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull()
    expect(screen.queryByText("Working…")).toBeNull()
    cleanup()
    // Broken: nothing runs.
    await renderRecorded("broken", { upTo: open, session: { serverState: "broken", state: "broken", turnCount: 2 } })
    expect(screen.queryByText("Working…")).toBeNull()
    expect(screen.getByText("Part of this session's history couldn't be saved")).toBeTruthy()
  })

  it("settles a call whose result was too large to show", async () => {
    await renderRecorded("truncated_tool_result")
    expect(screen.getByText("Too large to show here.")).toBeTruthy()
    expect(screen.getByText("The turn ended without a result for this call.")).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Stop" })).toBeNull()
  })

  it("offers an approval until the server no longer counts it, or long past its lifetime", async () => {
    const pending = fakeWorkerEvents("approve").findIndex((e) => e.kind === "approval_pending")
    const approve = () => screen.getByRole("button", { name: "Approve" })
    const now = Date.now()
    // Past the lifetime by the browser's clock, but within the skew it allows.
    await renderRecorded("approve", { upTo: pending + 1, startMs: now - 12 * 60_000 })
    expect(approve()).toHaveProperty("disabled", false)
    cleanup()
    // A read of the session from before the request says nothing about it.
    await renderRecorded("approve", {
      upTo: pending + 1,
      startMs: now,
      session: { pendingApprovalCount: 0, lastActivityAtUnixMs: now - 60_000 },
    })
    expect(approve()).toHaveProperty("disabled", false)
    cleanup()
    // Read after the request, the server counts no pending approval.
    await renderRecorded("approve", {
      upTo: pending + 1,
      startMs: now,
      session: { pendingApprovalCount: 0, lastActivityAtUnixMs: now + 60_000 },
    })
    expect(approve()).toHaveProperty("disabled", true)
    cleanup()
    // Long past the lifetime by the browser's clock.
    await renderRecorded("approve", { upTo: pending + 1, startMs: now - 16 * 60_000 })
    expect(approve()).toHaveProperty("disabled", true)
    expect(screen.getByRole("button", { name: "Deny" })).toHaveProperty("disabled", true)
  })

  it("shows a pending approval in the header, not Working… under the card", async () => {
    const events = fakeWorkerEvents("approve")
    const pendingAt = events.findIndex((e) => e.kind === "approval_pending")
    await renderRecorded("approve", {
      upTo: pendingAt + 1,
      startMs: Date.now(),
      session: { serverState: "running", state: "awaiting_approval", pendingApprovalCount: 1, turnCount: 1 },
    })
    expect(screen.getByText("Needs approval")).toBeTruthy()
    expect(screen.queryByText("Working…")).toBeNull()
  })
})

describe("a live turn", () => {
  it("streams thinking, text and tools without switching a component from uncontrolled to controlled", async () => {
    const warnings: string[] = []
    const record = (...args: unknown[]) => void warnings.push(args.map(String).join(" "))
    const warn = vi.spyOn(console, "warn").mockImplementation(record)
    const error = vi.spyOn(console, "error").mockImplementation(record)
    try {
      const stream = scriptedStream()
      await renderSession("/p/circus/s/web-vite", { wrapClient: withSessions(() => ({ watchSession: stream.watchSession })) })
      let seq = 1000
      for (const scenario of ["think", "tool"] as const) {
        for (const e of fakeWorkerEvents(scenario, Date.now())) {
          act(() => stream.control.push(e.seq > 0 ? { ...e, seq: seq++ } : e))
          await advance(30)
        }
      }
      await advance(2_000)
      expect(document.body.textContent).toContain("Thought for")
    } finally {
      warn.mockRestore()
      error.mockRestore()
    }
    expect(warnings.filter((w) => /uncontrolled|controlled to/i.test(w))).toEqual([])
  })
})

describe("a failed turn", () => {
  it("marks the CLI's error and the turn's end as failed, says so in the header, and takes the next message", async () => {
    await renderSession("/p/circus/s/web-vite")
    expect(screen.getByText("Idle")).toBeTruthy()
    await type("check the key: 401")
    await advance(3_000)
    const failed = document.querySelector<HTMLElement>("[data-failed]")!
    expect(failed.textContent).toMatch(/^Failed to authenticate\. API Error: 401/)
    expect(document.querySelector("[data-turn-error]")?.textContent).toBe("The turn ended with an error")
    // The header's outcome and the stream agree: one line, not a second one from the header's side.
    expect(document.querySelectorAll("[data-turn-error]")).toHaveLength(1)
    expect(screen.getByText("Turn failed")).toBeTruthy()

    await type("hello again")
    await advance(10_000)
    expect(screen.getByText("Idle")).toBeTruthy()
    expect(document.querySelectorAll("[data-failed]")).toHaveLength(1)
  })

  it("ends the turn with a failure line when the server says it failed and the stream shows nothing of it", async () => {
    // A worker-side abort the server counts as an error: its RESULT reads as aborted.
    await renderRecorded("interrupt", {
      upTo: fakeWorkerEvents("interrupt").findIndex((e) => e.kind === "turn_stopped"),
      session: { serverState: "idle", state: "failed", turnCount: 1 },
    })
    expect(screen.getByText("Turn failed")).toBeTruthy()
    expect([...document.querySelectorAll("[data-turn-error]")].map((e) => e.textContent)).toEqual([
      "The turn ended with an error",
    ])
    cleanup()
    // The server says the turn completed: no line.
    await renderRecorded("interrupt", {
      upTo: fakeWorkerEvents("interrupt").findIndex((e) => e.kind === "turn_stopped"),
      session: { serverState: "idle", state: "idle", turnCount: 1 },
    })
    expect(document.querySelector("[data-turn-error]")).toBeNull()
  })

  it("says it in Chinese", async () => {
    await renderSession("/p/circus/s/web-vite")
    await type("401")
    await advance(3_000)
    await act(() => i18n.changeLanguage("zh-CN"))
    expect(document.querySelector("[data-turn-error]")?.textContent).toBe("这一轮因错误而结束")
    expect(screen.getByText("上一轮失败")).toBeTruthy()
  })
})

describe("the session's permission mode", () => {
  it("shows in the header, with what it means", async () => {
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        getSession: async (req) => ({ ...(await mock.getSession(req)), permissionMode: "acceptEdits" }),
      })),
    })
    const mode = document.querySelector<HTMLElement>("[data-session-permission-mode]")!
    expect(mode.dataset.sessionPermissionMode).toBe("acceptEdits")
    expect(mode.querySelector("span[aria-hidden]")?.textContent).toBe("Start mode: Accept edits")
    // It is the mode the session started in: Claude may leave it.
    expect(mode.getAttribute("title")).toBe(
      "Permission mode the session started with: Accept edits\n" +
        "File edits in the working folder and common file-system commands, including deletions and redirections " +
        "(such as mkdir, touch, mv, cp, rm and >), run without asking; " +
        "other commands come to you for approval.\n" +
        "Claude can switch modes during the session; this shows the mode it started with.",
    )
    expect(mode.querySelector(".sr-only")?.textContent).toMatch(/^Permission mode the session started with: Accept edits File edits/)
  })

  it("shows auto for a session created without one, and a mode it doesn't know by its name", async () => {
    await renderSession("/p/circus/s/web-vite")
    expect(document.querySelector("[data-session-permission-mode]")?.textContent).toContain("Start mode: Auto")
    cleanup()
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        getSession: async (req) => ({ ...(await mock.getSession(req)), permissionMode: "newMode" }),
      })),
    })
    expect(document.querySelector("[data-session-permission-mode]")?.getAttribute("title")).toMatch(
      /^Permission mode the session started with: newMode\nClaude can switch modes/,
    )
  })
})

describe("notes to Claude", () => {
  const label = "Told Claude: the previous turn was interrupted"
  const sent = /^The previous turn was interrupted before it finished: /

  it("shows a collapsed row that opens to the exact text Claude was sent", async () => {
    await renderRecorded("crash_recover")
    const row = screen.getByRole("button", { name: label })
    expect(row.getAttribute("aria-expanded")).toBe("false")
    expect(screen.queryByText(sent)).toBeNull()

    fireEvent.click(row)
    await advance(0)
    expect(row.getAttribute("aria-expanded")).toBe("true")
    expect(screen.getByText(sent)).toBeTruthy()

    fireEvent.click(row)
    await advance(500)
    expect(row.getAttribute("aria-expanded")).toBe("false")
    expect(screen.queryByText(sent)).toBeNull()
  })

  it("shows nothing for an approval's retry or an interrupt", async () => {
    await renderRecorded("approve")
    expect(screen.queryByText(/^Told Claude/)).toBeNull()
    cleanup()
    await renderRecorded("interrupt")
    expect(screen.getByText("You stopped this turn")).toBeTruthy()
    expect(screen.queryByText(/^Told Claude/)).toBeNull()
  })

  /** The session page fed the notes `contents` after a user message. */
  async function renderNotes(contents: unknown[]) {
    const at = Date.now()
    const events: RawSessionEvent[] = contents.map((content, i) => ({
      seq: i + 1,
      tsUnixMs: at,
      kind: "user",
      payloadJson: JSON.stringify({
        type: "user",
        uuid: `u${i}`,
        isSynthetic: true,
        circus_note: "job_notice",
        message: { role: "user", content },
      }),
      ephemeral: false,
      truncated: false,
    }))
    return renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions(() => ({ watchSession: ({ signal }) => replayStream(events, signal) })),
    })
  }
  const jobLabel = "Told Claude: a background job finished"

  it("shows a note without text as its label alone, with nothing to open", async () => {
    await renderNotes([[{ type: "image", source: {} }]])
    expect(screen.getByText(jobLabel)).toBeTruthy()
    expect(screen.queryByRole("button", { name: jobLabel })).toBeNull()
  })

  it("lets the keyboard reach the text of a note where it scrolls", async () => {
    const long = Array.from({ length: 200 }, (_, i) => `line ${i}`).join("\n")
    const scrollHeight = vi.spyOn(HTMLElement.prototype, "scrollHeight", "get").mockImplementation(function (this: HTMLElement) {
      return this.textContent === long ? 4000 : 0
    })
    const clientHeight = vi.spyOn(HTMLElement.prototype, "clientHeight", "get").mockReturnValue(256)
    try {
      await renderNotes([long, "short"])
      const [first, second] = screen.getAllByRole("button", { name: jobLabel })
      fireEvent.click(first)
      fireEvent.click(second)
      await advance(0)
      const region = screen.getByRole("region", { name: jobLabel })
      expect(region.textContent).toBe(long)
      expect(region.tabIndex).toBe(0)
      // The short one doesn't scroll and takes no focus.
      expect(screen.getByText("short").hasAttribute("tabindex")).toBe(false)
    } finally {
      scrollHeight.mockRestore()
      clientHeight.mockRestore()
    }
  })

  it("labels it in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    await renderRecorded("broken")
    expect(screen.getByRole("button", { name: "已告知 Claude：部分历史记录缺失" })).toBeTruthy()
  })
})

describe("notices above the composer", () => {
  const broken = "Part of this session's history couldn't be saved"
  const offlineNotice = /is offline \(last seen/

  it("shows broken over device offline", async () => {
    const state = seedState()
    state.devices.find((d) => d.name === "palace-builder")!.online = false
    await renderSession("/p/palace/s/libvips", { state })
    expect(screen.getByText(broken)).toBeTruthy()
    expect(screen.queryByText(offlineNotice)).toBeNull()
    expect(composer().disabled).toBe(true)
  })

  it("replaces the composer in a stopped session, with no other notice", async () => {
    const state = seedState()
    state.sessions.find((s) => s.sessionId === "libvips")!.state = "stopped"
    state.devices.find((d) => d.name === "palace-builder")!.online = false
    await renderSession("/p/palace/s/libvips", { state })
    expect(screen.getByText("This session is stopped.")).toBeTruthy()
    expect(screen.queryByText(broken)).toBeNull()
    expect(screen.queryByText(offlineNotice)).toBeNull()
    expect(screen.queryByRole("textbox")).toBeNull()
  })

  it("shows device offline alone, with the composer enabled", async () => {
    const state = seedState()
    state.devices.find((d) => d.name === "devbox")!.online = false
    await renderSession("/p/toolbox/s/golangci-v2", { state })
    expect(screen.getByText(offlineNotice)).toBeTruthy()
    expect(composer().disabled).toBe(false)
  })
})

describe("the session's provider", () => {
  const picker = () => screen.getByRole("combobox", { name: "Change provider" })
  async function choose(name: string) {
    fireEvent.click(picker())
    await advance(0)
    fireEvent.click(screen.getByRole("option", { name }))
    await advance(50)
  }

  it("shows it in the header and changes it between turns, taking effect at the next start", async () => {
    const updates: Parameters<SessionService["updateSession"]>[0][] = []
    const { state } = await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        updateSession: (req) => {
          updates.push(req)
          return mock.updateSession(req)
        },
      })),
    })
    expect(picker().textContent).toBe("Local login")
    fireEvent.click(picker())
    await advance(0)
    expect(screen.getByText("Takes effect the next time Claude Code starts for this session.")).toBeTruthy()
    expect(screen.getAllByRole("option").map((o) => o.textContent)).toEqual([
      "Anthropic",
      "Legacy key",
      "Team proxy",
      "Local login",
    ])
    fireEvent.click(screen.getByRole("option", { name: "Team proxy" }))
    await advance(50)
    expect(updates).toEqual([{ sessionId: "web-vite", providerId: "provider-proxy" }])
    expect(state.sessions.find((s) => s.sessionId === "web-vite")!.providerId).toBe("provider-proxy")
    expect(picker().textContent).toBe("Team proxy")

    await choose("Local login")
    expect(updates.at(-1)).toEqual({ sessionId: "web-vite", providerId: "" })
  })

  it("can't be changed while a turn runs", async () => {
    await renderSession("/p/circus/s/web-vite")
    await type("Look around")
    await advance(300)
    expect(picker().hasAttribute("disabled")).toBe(true)
    expect(picker().closest("[aria-label]:not([role=combobox])")?.getAttribute("aria-label")).toBe(
      "Can't change it while a turn runs.",
    )
    // The turn ends: it can change again.
    await advance(10_000)
    expect(picker().hasAttribute("disabled")).toBe(false)
  })

  it("can't be changed while the server runs a turn the stream hasn't shown", async () => {
    // Another tab's turn: the session reads as running, nothing streamed here.
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        getSession: async (req) => ({ ...(await mock.getSession(req)), serverState: "running" as const, state: "running" as const }),
      })),
    })
    expect(picker().hasAttribute("disabled")).toBe(true)
  })

  it("can't be changed while the stream shows a turn the server hasn't reported", async () => {
    let turnCount: number | undefined
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        // Reads lag behind: idle, at the turn count of the first read.
        getSession: async (req) => {
          const s = await mock.getSession(req)
          turnCount ??= s.turnCount
          return { ...s, serverState: "idle" as const, state: "idle" as const, turnCount }
        },
      })),
    })
    expect(picker().hasAttribute("disabled")).toBe(false)
    await type("Look around")
    await advance(300)
    expect(picker().hasAttribute("disabled")).toBe(true)
  })

  it("closes its menu once a change isn't allowed", async () => {
    let running = false
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        getSession: async (req) => {
          const s = await mock.getSession(req)
          return running ? { ...s, serverState: "running" as const, state: "running" as const } : s
        },
      })),
    })
    fireEvent.click(picker())
    await advance(0)
    expect(screen.getAllByRole("option").length).toBeGreaterThan(0)
    // Another tab starts a turn; the next read says so.
    running = true
    await advance(5_000)
    expect(screen.queryAllByRole("option")).toEqual([])
    // Once the turn is over, the menu stays closed until opened again.
    running = false
    await advance(5_000)
    expect(picker().hasAttribute("disabled")).toBe(false)
    expect(screen.queryAllByRole("option")).toEqual([])
  })

  it("says the session was stopped when that refused the change", async () => {
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions((mock) => ({
        // Stopped elsewhere just before the change arrives.
        updateSession: async (req) => {
          await mock.stopSession({ sessionId: req.sessionId })
          return mock.updateSession(req)
        },
      })),
    })
    await choose("Anthropic")
    await advance(50)
    expect(screen.getByRole("alert").textContent).toBe("The session was stopped, so its provider can't change.")
  })

  it("is only shown for a stopped session", async () => {
    await renderSession("/p/circus/s/quic-spike")
    expect(screen.queryByRole("combobox", { name: "Change provider" })).toBeNull()
    expect(screen.getByText("Local login")).toBeTruthy()
  })

  it("says why a change was refused", async () => {
    await renderSession("/p/circus/s/web-vite", {
      wrapClient: withSessions(() => ({
        updateSession: () => Promise.reject(new ApiError("failed_precondition", "a turn runs")),
      })),
    })
    await choose("Anthropic")
    expect(screen.getByRole("alert").textContent).toBe("A turn started meanwhile. Change the provider once it ends.")
  })
})

describe("a queued session", () => {
  const notice = () => document.querySelector("[data-queue-reason]")

  it.each([
    ["queue-full", "provider_full", "Team proxy already runs as many sessions as it allows. This one starts when one of them ends. Open Providers"],
    ["queue-no-worker", "no_assigned_worker", "No online worker assigned to Anthropic can run this session. Open Providers"],
    ["queue-secrets", "provider_secrets_unreadable", "The key and variable values of Legacy key can't be read. Enter them again. Open Providers"],
    ["queue-offline", "worker_offline", "The worker that runs this session is offline. "],
    ["queue-agent", "agent_not_installed", "No online worker that can run this session has Claude Code installed. Open Devices"],
    ["queue-deleted", "provider_deleted", "This session's provider was deleted. Pick another provider to go on. Change provider"],
  ])("%s says why it waits (%s)", async (id, reason, text) => {
    const state = seedState()
    addQueueDemo(state)
    const project = state.sessions.find((s) => s.sessionId === id)!.projectId
    await renderSession(`/p/${project}/s/${id}`, { state })
    expect(notice()?.getAttribute("data-queue-reason")).toBe(reason)
    expect(notice()?.textContent).toBe(text)
  })

  it.each(["queue-full", "queue-no-worker", "queue-secrets"])(
    "links to the providers on the Devices page (%s)",
    async (id) => {
      const state = seedState()
      addQueueDemo(state)
      const project = state.sessions.find((s) => s.sessionId === id)!.projectId
      await renderSession(`/p/${project}/s/${id}`, { state })
      const link = within(notice() as HTMLElement).getByRole("link", { name: "Open Providers" })
      expect(link.getAttribute("href")).toBe("/devices#providers")
    },
  )

  it("links to Devices when no worker that may run it has Claude Code", async () => {
    const state = seedState()
    addQueueDemo(state)
    await renderSession("/p/palace/s/queue-agent", { state })
    const link = within(notice() as HTMLElement).getByRole("link", { name: "Open Devices" })
    expect(link.getAttribute("href")).toBe("/devices")
  })

  it.each(["error", "interrupted"] as const)(
    "says why it waits, not how its last turn ended (%s)",
    async (outcome) => {
      const state = seedState()
      addQueueDemo(state)
      state.sessions.find((s) => s.sessionId === "queue-deleted")!.lastTurnOutcome = outcome
      await renderSession("/p/palace/s/queue-deleted", { state })
      expect(screen.getByText("Queued")).toBeTruthy()
      expect(notice()?.getAttribute("data-queue-reason")).toBe("provider_deleted")
      expect(screen.getByRole("button", { name: "Change provider" })).toBeTruthy()
    },
  )

  it("offers another provider when its own was deleted, and goes on with it", async () => {
    const state = seedState()
    addQueueDemo(state)
    await renderSession("/p/palace/s/queue-deleted", { state })
    expect(screen.getByRole("combobox", { name: "Change provider" }).textContent).toBe("Deleted provider")
    fireEvent.click(screen.getByRole("button", { name: "Change provider" }))
    await advance(0)
    fireEvent.click(screen.getByRole("option", { name: "Anthropic" }))
    await advance(50)
    // palace-builder has no worker of its own: any of Anthropic's takes it.
    expect(state.sessions.find((s) => s.sessionId === "queue-deleted")!.providerId).toBe("provider-anthropic")
    await advance(10_000)
    expect(notice()).toBeNull()
    expect(screen.getByText(/this is the mock backend/)).toBeTruthy()
  })

  const queueEvent = (reason: string): RawSessionEvent => ({
    seq: 0,
    tsUnixMs: Date.now(),
    kind: "queue",
    payloadJson: JSON.stringify({ queue_reason: reason }),
    ephemeral: true,
    truncated: false,
  })
  const reason = () => notice()?.getAttribute("data-queue-reason")

  it("renders nothing again for a read that changed nothing", async () => {
    const state = seedState()
    addQueueDemo(state)
    let commits = 0
    let reads = 0
    await renderSession("/p/circus/s/queue-full", {
      state,
      onCommit: () => commits++,
      wrapClient: withSessions((mock) => ({ getSession: (req) => (reads++, mock.getSession(req)) })),
    })
    await advance(1_000)
    const [commitsBefore, readsBefore] = [commits, reads]
    // Polls every five seconds, each answered with the same session.
    await advance(15_000)
    expect(reads - readsBefore).toBe(3)
    expect(commits).toBe(commitsBefore)
  })

  it("follows the stream's queue events between reads of the session", async () => {
    const state = seedState()
    addQueueDemo(state)
    const { control, watchSession } = scriptedStream()
    await renderSession("/p/circus/s/queue-full", { state, wrapClient: withSessions(() => ({ watchSession })) })
    expect(reason()).toBe("provider_full")

    // The reason changes between the session's reads, which come every five seconds.
    act(() => control.push(queueEvent("worker_offline")))
    await advance(10)
    expect(reason()).toBe("worker_offline")
    // A read sent after it arrived wins again.
    await advance(5_000)
    expect(reason()).toBe("provider_full")
  })

  it("keeps a reason that arrived while a read was under way", async () => {
    const state = seedState()
    addQueueDemo(state)
    const { control, watchSession } = scriptedStream()
    let hold: Promise<void> | null = null
    let release = () => {}
    await renderSession("/p/circus/s/queue-full", {
      state,
      wrapClient: withSessions((mock) => ({
        watchSession,
        // The server builds the answer at once; it reaches the browser when released.
        getSession: async (req) => {
          const answer = await mock.getSession(req)
          if (hold) await hold
          return answer
        },
      })),
    })
    expect(reason()).toBe("provider_full")
    hold = new Promise<void>((resolve) => (release = resolve))
    await advance(5_000)
    // The poll's read is under way when the reason changes.
    act(() => control.push(queueEvent("no_assigned_worker")))
    await advance(10)
    act(() => release())
    hold = null
    await advance(10)
    expect(reason()).toBe("no_assigned_worker")
  })

  it("keeps the stream's reason when the stream opens again, until a later read lands", async () => {
    const state = seedState()
    addQueueDemo(state)
    const { control, watchSession } = scriptedStream()
    let hold: Promise<void> | null = null
    let release = () => {}
    const seen: (string | null | undefined)[] = []
    await renderSession("/p/circus/s/queue-full", {
      state,
      onCommit: () => seen.push(reason()),
      wrapClient: withSessions((mock) => ({
        watchSession,
        getSession: async (req) => {
          const answer = await mock.getSession(req)
          if (hold) await hold
          return answer
        },
      })),
    })
    // A stream that served a while reopens at once when it ends.
    await advance(HEALTHY_STREAM_MS)
    act(() => control.push(queueEvent("worker_offline")))
    await advance(10)
    expect(reason()).toBe("worker_offline")
    // Reads are slow now: the cached one, older than the event, must not show.
    state.sessions.find((s) => s.sessionId === "queue-full")!.queueReason = "worker_offline"
    hold = new Promise<void>((resolve) => (release = resolve))
    seen.length = 0
    act(() => control.drop())
    await advance(100)
    expect(control.connections).toBe(2)
    act(() => release())
    hold = null
    await advance(100)
    expect(reason()).toBe("worker_offline")
    expect(seen.filter((r) => r !== "worker_offline")).toEqual([])
  })
})

describe("a disconnected worker", () => {
  const notice = () => document.querySelector("[data-worker-disconnected]")
  const running =
    "The worker disconnected just now. If it doesn't reconnect within a minute, this turn will be interrupted."
  const idle = "The worker disconnected just now; waiting for it to reconnect."
  const workerEvent = (at: number): RawSessionEvent => ({
    seq: 0,
    tsUnixMs: Date.now(),
    kind: "worker",
    payloadJson: JSON.stringify({ disconnected_at_unix_ms: String(at) }),
    ephemeral: true,
    truncated: false,
  })

  it("warns that a running turn will be interrupted, keeping Stop enabled", async () => {
    const state = seedState()
    addWorkerDemo(state)
    await renderSession("/p/circus/s/worker-gone-running", { state })
    expect(notice()?.textContent).toBe(running)
    const stop = screen.getByRole<HTMLButtonElement>("button", { name: "Stop" })
    expect(stop.disabled).toBe(false)
  })

  it("says so in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    const state = seedState()
    addWorkerDemo(state)
    await renderSession("/p/circus/s/worker-gone-running", { state })
    expect(notice()?.textContent).toBe("工作节点连接已断开（刚刚）。1 分钟内未重连，本轮将中断。")
    cleanup()
    await renderSession("/p/palace/s/worker-gone-idle", { state })
    expect(notice()?.textContent).toBe("工作节点连接已断开（刚刚），等待重连。")
  })

  it("waits for an idle session's worker to reconnect, the time moving on", async () => {
    const state = seedState()
    const { control, watchSession } = scriptedStream()
    await renderSession("/p/toolbox/s/golangci-v2", { state, wrapClient: withSessions(() => ({ watchSession })) })
    // The server's reads say it too from now on (the mock's lease doesn't run out here).
    const at = Date.now() - 50_000
    state.sessions.find((s) => s.sessionId === "golangci-v2")!.workerDisconnectedAtUnixMs = at
    act(() => control.push(workerEvent(at)))
    await advance(10)
    expect(notice()?.textContent).toBe(idle)
    await advance(15_000)
    expect(notice()?.textContent).toBe("The worker disconnected 1m ago; waiting for it to reconnect.")
  })

  it("goes once the lease ends, the running turn interrupted", async () => {
    const state = seedState()
    addWorkerDemo(state)
    await renderSession("/p/circus/s/worker-gone-running", { state })
    expect(notice()).not.toBeNull()
    await advance(60_000)
    expect(notice()).toBeNull()
    expect(screen.getByText("Turn interrupted")).toBeTruthy()
  })

  it("follows the stream's events, the clearing one too", async () => {
    const { control, watchSession } = scriptedStream()
    await renderSession("/p/toolbox/s/golangci-v2", { wrapClient: withSessions(() => ({ watchSession })) })
    expect(notice()).toBeNull()
    act(() => control.push(workerEvent(Date.now() - 2_000)))
    await advance(10)
    expect(notice()?.textContent).toBe(idle)
    act(() => control.push(workerEvent(0)))
    await advance(10)
    expect(notice()).toBeNull()
  })

  it("is not shown in a stopped session", async () => {
    const state = seedState()
    addWorkerDemo(state)
    state.sessions.find((s) => s.sessionId === "worker-gone-idle")!.state = "stopped"
    await renderSession("/p/palace/s/worker-gone-idle", { state })
    expect(screen.getByText("This session is stopped.")).toBeTruthy()
    expect(notice()).toBeNull()
  })

  it("is not shown while a broken session's card is", async () => {
    const state = seedState()
    state.sessions.find((s) => s.sessionId === "libvips")!.workerDisconnectedAtUnixMs = Date.now()
    await renderSession("/p/palace/s/libvips", { state })
    expect(screen.getByText("Part of this session's history couldn't be saved")).toBeTruthy()
    expect(notice()).toBeNull()
  })

  it("stops the demo's running turn, the worker still away", async () => {
    const state = seedState()
    addWorkerDemo(state)
    await renderSession("/p/circus/s/worker-gone-running", { state })
    expect(notice()?.textContent).toBe(running)
    fireEvent.click(screen.getByRole("button", { name: "Stop" }))
    await advance(500)
    expect(notice()?.textContent).toBe("The worker disconnected just now; waiting for it to reconnect.")
    expect(state.sessions.find((s) => s.sessionId === "worker-gone-running")!.state).toBe("idle")
  })

  // Each commit of the view, and whether the notice was in it.
  const sid = "golangci-v2"
  const slowReads = () => {
    const gate = { hold: null as Promise<void> | null, release: () => {} }
    const slow = (mock: SessionService): Partial<SessionService> => ({
      getSession: async (req) => {
        const answer = await mock.getSession(req)
        if (gate.hold) await gate.hold
        return answer
      },
    })
    const close = () => {
      gate.hold = new Promise<void>((resolve) => (gate.release = resolve))
    }
    const open = () => {
      act(() => gate.release())
      gate.hold = null
    }
    return { slow, close, open }
  }

  it("never commits a frame without the notice after a live disconnect the next reads repeat", async () => {
    const state = seedState()
    const { control, watchSession } = scriptedStream()
    const seen: boolean[] = []
    await renderSession(`/p/toolbox/s/${sid}`, {
      state,
      wrapClient: withSessions(() => ({ watchSession })),
      onCommit: () => seen.push(notice() !== null),
    })
    const at = Date.now() - 2_000
    state.sessions.find((s) => s.sessionId === sid)!.workerDisconnectedAtUnixMs = at
    act(() => control.push(workerEvent(at)))
    await advance(10)
    expect(notice()).not.toBeNull()
    seen.length = 0
    // Two polls land; neither may take the notice away, even for a frame.
    await advance(12_000)
    expect(seen).not.toContain(false)
    expect(notice()).not.toBeNull()
  })

  it("lets a read sent after a live disconnect arrived, landing before it is applied, decide", async () => {
    const { queryKeys } = await import("@/api/query-client")
    const state = seedState()
    const { control, watchSession } = scriptedStream()
    let frozen: SessionDetails | null = null
    const r = await renderSession(`/p/toolbox/s/${sid}`, {
      state,
      // Reads repeat the first one: the worker is connected again by then.
      wrapClient: withSessions((mock) => ({ watchSession, getSession: async (req) => frozen ?? mock.getSession(req) })),
    })
    await advance(1_000)
    frozen = r.queryClient.getQueryData(queryKeys.session(sid)) as SessionDetails
    const oldSent = frozen.readSentAtMs
    expect(notice()).toBeNull()
    // The worker disconnects; the event arrives, its apply timer has not run.
    await act(async () => control.push(workerEvent(Date.now() - 1_000)))
    // 50 ms later, with no timer run, a read is sent and lands.
    vi.setSystemTime(Date.now() + 50)
    await act(async () => {
      await r.queryClient.refetchQueries({ queryKey: queryKeys.session(sid) })
    })
    expect((r.queryClient.getQueryData(queryKeys.session(sid)) as SessionDetails).readSentAtMs).not.toBe(oldSent)
    await advance(0)
    expect(notice()).toBeNull()
    await advance(3_000)
    expect(notice()).toBeNull()
  })

  it("never commits a frame with a stale notice after a live clear the next reads repeat", async () => {
    const state = seedState()
    state.sessions.find((s) => s.sessionId === sid)!.workerDisconnectedAtUnixMs = Date.now() - 2_000
    const { control, watchSession } = scriptedStream()
    const seen: boolean[] = []
    await renderSession(`/p/toolbox/s/${sid}`, {
      state,
      wrapClient: withSessions(() => ({ watchSession })),
      onCommit: () => seen.push(notice() !== null),
    })
    expect(notice()).not.toBeNull()
    state.sessions.find((s) => s.sessionId === sid)!.workerDisconnectedAtUnixMs = 0
    act(() => control.push(workerEvent(0)))
    await advance(10)
    expect(notice()).toBeNull()
    seen.length = 0
    await advance(12_000)
    expect(seen).not.toContain(true)
  })

  it("keeps a live disconnect through a stream reopen, until the read it asks for lands", async () => {
    const state = seedState()
    const { control, watchSession } = scriptedStream()
    const reads = slowReads()
    const seen: boolean[] = []
    await renderSession(`/p/toolbox/s/${sid}`, {
      state,
      onCommit: () => seen.push(notice() !== null),
      wrapClient: withSessions((mock) => ({ watchSession, ...reads.slow(mock) })),
    })
    await advance(HEALTHY_STREAM_MS)
    const at = Date.now() - 2_000
    state.sessions.find((s) => s.sessionId === sid)!.workerDisconnectedAtUnixMs = at
    act(() => control.push(workerEvent(at)))
    await advance(10)
    expect(notice()).not.toBeNull()
    seen.length = 0
    reads.close()
    act(() => control.drop())
    await advance(100)
    expect(control.connections).toBe(2)
    reads.open()
    await advance(100)
    expect(notice()).not.toBeNull()
    expect(seen).not.toContain(false)
  })

  it("keeps a live clear through a stream reopen, until the read it asks for lands", async () => {
    const state = seedState()
    state.sessions.find((s) => s.sessionId === sid)!.workerDisconnectedAtUnixMs = Date.now() - 2_000
    const { control, watchSession } = scriptedStream()
    const reads = slowReads()
    const seen: boolean[] = []
    await renderSession(`/p/toolbox/s/${sid}`, {
      state,
      onCommit: () => seen.push(notice() !== null),
      wrapClient: withSessions((mock) => ({ watchSession, ...reads.slow(mock) })),
    })
    expect(notice()).not.toBeNull()
    await advance(HEALTHY_STREAM_MS)
    state.sessions.find((s) => s.sessionId === sid)!.workerDisconnectedAtUnixMs = 0
    act(() => control.push(workerEvent(0)))
    await advance(10)
    expect(notice()).toBeNull()
    seen.length = 0
    reads.close()
    act(() => control.drop())
    await advance(100)
    reads.open()
    await advance(100)
    expect(notice()).toBeNull()
    expect(seen).not.toContain(true)
  })

  it("reads the session again as soon as the stream reopens, which settles what changed meanwhile", async () => {
    const state = seedState()
    const { control, watchSession } = scriptedStream()
    await renderSession(`/p/toolbox/s/${sid}`, { state, wrapClient: withSessions(() => ({ watchSession })) })
    await advance(HEALTHY_STREAM_MS)
    const at = Date.now() - 2_000
    act(() => control.push(workerEvent(at)))
    await advance(10)
    expect(notice()).not.toBeNull()
    // The worker came back while the stream was closed: no event says so,
    // only a read.
    await advance(1_000)
    act(() => control.drop())
    await advance(100)
    expect(control.connections).toBe(2)
    // Well before the next poll (every five seconds).
    expect(notice()).toBeNull()
  })

  describe("with this browser's clock ten minutes ahead of the server's", () => {
    const skew = -10 * 60_000 // the server's time is this browser's plus skew

    it("tells the time by a live event's clock", async () => {
      const state = seedState()
      const { control, watchSession } = scriptedStream()
      await renderSession(`/p/toolbox/s/${sid}`, { state, wrapClient: withSessions(() => ({ watchSession })) })
      const serverNow = Date.now() + skew
      act(() => control.push({ ...workerEvent(serverNow - 5_000), tsUnixMs: serverNow }))
      await advance(10)
      expect(notice()?.textContent).toBe(idle)
    })

    it("says no time until the stream told the server's clock, then the right one", async () => {
      const state = seedState()
      const at = Date.now() + skew - 5_000
      const watchSession: SessionService["watchSession"] = async function* () {
        await new Promise((resolve) => setTimeout(resolve, 200))
        yield { seq: 0, tsUnixMs: Date.now() + skew, kind: "unspecified", payloadJson: "", ephemeral: true, truncated: false }
        await new Promise<never>(() => {})
      }
      await renderSession(`/p/toolbox/s/${sid}`, {
        state,
        wrapClient: withSessions((mock) => ({
          watchSession,
          getSession: async (req) => ({ ...(await mock.getSession(req)), workerDisconnectedAtUnixMs: at }),
        })),
      })
      expect(notice()?.textContent).toBe("The worker disconnected; waiting for it to reconnect.")
      await advance(1_000)
      expect(notice()?.textContent).toBe(idle)
      await advance(15_000)
      expect(notice()?.textContent).toBe(idle)
    })
  })

  it("shows over device offline", async () => {
    const state = seedState()
    addWorkerDemo(state)
    state.devices.find((d) => d.name === "palace-builder")!.online = false
    await renderSession("/p/palace/s/worker-gone-idle", { state })
    expect(notice()?.textContent).toBe(idle)
    expect(screen.queryByText(/is offline \(last seen/)).toBeNull()
  })
})
