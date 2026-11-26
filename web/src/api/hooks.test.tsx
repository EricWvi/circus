// @vitest-environment happy-dom
import { cleanup } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { MutationState } from "@tanstack/react-query"

import {
  AGENT_ANSWER_WAIT_MS,
  answeredReport,
  useChangeSessionProvider,
  useCreateSession,
  useSetWorkerAgent,
  useSetWorkerGrant,
  useWorkers,
  type WorkerAgentRequest,
} from "@/api/hooks"
import { addAgentsDemo, seedState } from "@/api/mock/state"
import { queryKeys } from "@/api/query-client"
import type { CircusClient, SessionDetails, Worker } from "@/api/types"
import { advance, renderWithBackend } from "@/test/render"

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

// A session read answered half a second after its request was sent: the
// read's time is when it was sent, not when it landed, so live values that
// arrived meanwhile still win over it.
const sid = "golangci-v2"
function slowAnswers(sent: number[]) {
  return (client: CircusClient): CircusClient => {
    const answer = async (): Promise<SessionDetails> => {
      sent.push(Date.now())
      const session = await client.sessions.getSession({ sessionId: sid })
      vi.setSystemTime(Date.now() + 500)
      return session
    }
    return {
      ...client,
      sessions: { ...client.sessions, updateSession: answer, createSession: answer },
    }
  }
}

describe("session reads carry when their request was sent", () => {
  it("the answer of a provider change", async () => {
    const sent: number[] = []
    let change: ReturnType<typeof useChangeSessionProvider> | undefined
    function Probe() {
      change = useChangeSessionProvider()
      return null
    }
    const r = await renderWithBackend(<Probe />, { wrapClient: slowAnswers(sent) })
    let done = false
    void change!.mutateAsync({ sessionId: sid, providerId: "" }).then(() => (done = true))
    await advance(1_000)
    expect(done).toBe(true)
    const cached = r.queryClient.getQueryData<SessionDetails>(queryKeys.session(sid))
    expect(sent).toHaveLength(1)
    expect(cached?.readSentAtMs).toBe(sent[0])
  })

  it("the answer of a new session", async () => {
    const sent: number[] = []
    let create: ReturnType<typeof useCreateSession> | undefined
    function Probe() {
      create = useCreateSession()
      return null
    }
    const r = await renderWithBackend(<Probe />, { wrapClient: slowAnswers(sent) })
    let done = false
    void create!
      .mutateAsync({ clientRequestId: "new", projectId: "toolbox" } as Parameters<NonNullable<typeof create>["mutateAsync"]>[0])
      .then(() => (done = true))
    await advance(1_000)
    expect(done).toBe(true)
    const cached = r.queryClient.getQueryData<SessionDetails>(queryKeys.session(sid))
    expect(sent).toHaveLength(1)
    expect(cached?.readSentAtMs).toBe(sent[0])
  })
})

describe("worker grant changes", () => {
  it("leave a newer change's answer in place when an older one lands late", async () => {
    // The first request's answer is held; the server applies each at once.
    let release!: () => void
    const held = new Promise<void>((resolve) => (release = resolve))
    let calls = 0
    let grant: ReturnType<typeof useSetWorkerGrant> | undefined
    function Probe() {
      useWorkers()
      grant = useSetWorkerGrant({ workerId: "worker-devbox", name: "devbox" })
      return null
    }
    const r = await renderWithBackend(<Probe />, {
      wrapClient: (client) => ({
        ...client,
        machines: {
          ...client.machines,
          setWorkerGrant: async (req) => {
            const worker = await client.machines.setWorkerGrant(req)
            if (++calls === 1) await held
            return worker
          },
        },
      }),
    })
    await advance(50)
    const devbox = () =>
      r.queryClient.getQueryData<Worker[]>(queryKeys.workers)?.find((w) => w.workerId === "worker-devbox")?.integrator
    expect(devbox()).toBe(false)
    grant!.mutate({ integrator: true })
    await advance(10)
    grant!.mutate({ integrator: false })
    await advance(10)
    expect(devbox()).toBe(false)
    release()
    await advance(10)
    expect(devbox()).toBe(false)
  })

  it("write an older answer when the newer change failed", async () => {
    let release!: () => void
    const held = new Promise<void>((resolve) => (release = resolve))
    let calls = 0
    let grant: ReturnType<typeof useSetWorkerGrant> | undefined
    function Probe() {
      useWorkers()
      grant = useSetWorkerGrant({ workerId: "worker-devbox", name: "devbox" })
      return null
    }
    const r = await renderWithBackend(<Probe />, {
      wrapClient: (client) => ({
        ...client,
        machines: {
          ...client.machines,
          setWorkerGrant: async (req) => {
            if (++calls === 2) throw new Error("boom")
            const worker = await client.machines.setWorkerGrant(req)
            await held
            return worker
          },
        },
      }),
    })
    await advance(50)
    grant!.mutate({ integrator: true })
    await advance(10)
    grant!.mutate({ integrator: false })
    await advance(10)
    release()
    await advance(10)
    const devbox = r.queryClient.getQueryData<Worker[]>(queryKeys.workers)?.find((w) => w.workerId === "worker-devbox")
    expect(devbox?.integrator).toBe(true)
  })
})

describe("worker agent changes", () => {
  // nas is offline, so the mock only records the wish: the answer is all
  // that changes. After the first read, the list never answers again, so
  // only the change's own answer can reach the cache.
  function render(
    setWorkerAgent?: (
      req: Parameters<CircusClient["machines"]["setWorkerAgent"]>[0],
      client: CircusClient,
    ) => Promise<Worker>,
  ) {
    const state = seedState()
    addAgentsDemo(state)
    let lists = 0
    let change: ReturnType<typeof useSetWorkerAgent> | undefined
    function Probe() {
      useWorkers()
      change = useSetWorkerAgent({ workerId: "worker-nas", name: "nas" }, "claude_code")
      return null
    }
    const rendered = renderWithBackend(<Probe />, {
      state,
      wrapClient: (client) => ({
        ...client,
        machines: {
          ...client.machines,
          listWorkers: () => (++lists === 1 ? client.machines.listWorkers() : new Promise<never>(() => {})),
          setWorkerAgent: setWorkerAgent ? (req) => setWorkerAgent(req, client) : client.machines.setWorkerAgent,
        },
      }),
    })
    return { rendered, change: () => change!, lists: () => lists }
  }
  const wanted = (r: Awaited<ReturnType<typeof renderWithBackend>>) =>
    r.queryClient.getQueryData<Worker[]>(queryKeys.workers)?.find((w) => w.workerId === "worker-nas")?.agents[0].wanted

  it("write the returned worker into the list at once, and read the list again", async () => {
    const { rendered, change, lists } = render()
    const r = await rendered
    await advance(50)
    expect(wanted(r)).toBe(true)
    expect(lists()).toBe(1)
    change().mutate({ install: false })
    await advance(10)
    expect(wanted(r)).toBe(false)
    expect(lists()).toBe(2)
  })

  it("leave a newer change's answer in place when an older one lands late", async () => {
    let release!: () => void
    const held = new Promise<void>((resolve) => (release = resolve))
    let calls = 0
    const { rendered, change } = render(async (req, client) => {
      const worker = await client.machines.setWorkerAgent(req)
      if (++calls === 1) await held
      return worker
    })
    const r = await rendered
    await advance(50)
    change().mutate({ install: false })
    await advance(10)
    change().mutate({ install: true })
    await advance(10)
    expect(wanted(r)).toBe(true)
    release()
    await advance(10)
    expect(wanted(r)).toBe(true)
  })
})

describe("the answer a worker hasn't acted on", () => {
  const change = (status: "success" | "error" | "pending", reportedAtUnixMs: number, submittedAt: number) =>
    ({
      status,
      submittedAt,
      data: status === "success" ? ({ agents: [{ agent: "claude_code", reportedAtUnixMs }] } as Worker) : undefined,
    }) as MutationState<Worker, unknown, WorkerAgentRequest>

  it("is the report of the latest successful change", () => {
    const now = 100_000
    expect(answeredReport([], "claude_code", now)).toBeUndefined()
    expect(answeredReport([change("success", 1, now - 2), change("success", 2, now - 1)], "claude_code", now)).toBe(2)
    expect(answeredReport([change("success", 1, now - 2), change("error", 0, now - 1)], "claude_code", now)).toBe(1)
  })

  it("counts only for a while after the request", () => {
    const now = 100_000
    expect(answeredReport([change("success", 1, now - AGENT_ANSWER_WAIT_MS + 1)], "claude_code", now)).toBe(1)
    expect(answeredReport([change("success", 1, now - AGENT_ANSWER_WAIT_MS)], "claude_code", now)).toBeUndefined()
  })
})
