// @vitest-environment happy-dom
import { cleanup } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { useChangeSessionProvider, useCreateSession } from "@/api/hooks"
import { queryKeys } from "@/api/query-client"
import type { CircusClient, SessionDetails } from "@/api/types"
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
