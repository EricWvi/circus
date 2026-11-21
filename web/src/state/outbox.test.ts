import { describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"
import type { SessionService } from "@/api/types"
import { withOutbox } from "@/lib/session-events/outbox"
import type { SessionEvent } from "@/lib/types"

import { createOutbox, newClientMsgId, sendFailure } from "./outbox"

type SendReq = Parameters<SessionService["sendMessage"]>[0]

/** A sendMessage whose answers the test gives one at a time. */
function server() {
  const calls: { req: SendReq; resolve: () => void; reject: (e: unknown) => void }[] = []
  return {
    calls,
    sendMessage: (req: SendReq) =>
      new Promise<{ inputId: string; eventSeq: number }>((resolve, reject) => {
        calls.push({ req, resolve: () => resolve({ inputId: "in-1", eventSeq: 1 }), reject })
      }),
  }
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 0))

describe("the outbox", () => {
  it("shows a message as sending, then accepted, and tells when it reached the server", async () => {
    const s = server()
    const sent: string[] = []
    const outbox = createOutbox(s, { onSent: (id) => sent.push(id) })
    const done = outbox.send("s1", "hello")
    expect(outbox.messages("s1")).toMatchObject([{ text: "hello", delivery: "sending", priority: "normal" }])
    expect(s.calls[0].req).toMatchObject({ sessionId: "s1", text: "hello", priority: "normal" })
    s.calls[0].resolve()
    await done
    expect(outbox.messages("s1")).toMatchObject([{ delivery: "accepted" }])
    expect(sent).toEqual(["s1"])
  })

  it("keeps a failed message and retries it with the same id and text, once at a time", async () => {
    const s = server()
    const outbox = createOutbox(s)
    const first = outbox.send("s1", "hello")
    s.calls[0].reject(new ApiError("resource_exhausted", "too many"))
    await expect(first).rejects.toBeInstanceOf(ApiError)
    const [failed] = outbox.messages("s1")
    expect(failed).toMatchObject({ delivery: "failed", failure: "too_many" })

    const retry = outbox.retry(failed.clientMsgId)
    void outbox.retry(failed.clientMsgId)
    expect(s.calls).toHaveLength(2)
    expect(s.calls[1].req).toEqual(s.calls[0].req)
    expect(outbox.messages("s1")[0]).toMatchObject({ delivery: "sending" })
    expect(outbox.messages("s1")[0].failure).toBeUndefined()
    s.calls[1].resolve()
    await retry
    expect(outbox.messages("s1")[0]).toMatchObject({ delivery: "accepted" })
  })

  it("forgets what the stream shows, but not a send still under way", async () => {
    const s = server()
    const outbox = createOutbox(s)
    void outbox.send("s1", "a")
    const ids = new Set(outbox.messages("s1").map((m) => m.clientMsgId))
    outbox.forget("s1", ids)
    expect(outbox.messages("s1")).toHaveLength(1)
    s.calls[0].resolve()
    await settle()
    outbox.forget("s1", ids)
    expect(outbox.messages("s1")).toEqual([])
  })

  it("keeps a message the session's full view will never show, saying so", async () => {
    const outbox = createOutbox({ sendMessage: async () => ({ inputId: "in-1", eventSeq: 0 }) })
    await outbox.send("s1", "hello")
    expect(outbox.messages("s1")).toMatchObject([{ text: "hello", delivery: "not_kept" }])
  })

  it("keeps the same list until something changes, for useSyncExternalStore", () => {
    const outbox = createOutbox(server())
    expect(outbox.messages("s1")).toBe(outbox.messages("s1"))
    void outbox.send("s1", "a")
    expect(outbox.messages("s1")).toBe(outbox.messages("s1"))
  })

  it("names the reason of a failure", () => {
    expect(sendFailure(new ApiError("resource_exhausted", ""))).toBe("too_many")
    expect(sendFailure(new ApiError("failed_precondition", ""))).toBe("stopped")
    expect(sendFailure(new ApiError("unavailable", ""))).toBe("unreachable")
    expect(sendFailure(new Error("x"))).toBe("other")
  })

  it("makes UUIDs", () => {
    expect(newClientMsgId()).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
  })
})

describe("withOutbox", () => {
  const at = "2026-11-21T00:00:00Z"
  const streamed: SessionEvent = {
    kind: "user_message",
    id: "msg:c1",
    at,
    text: "hello",
    clientMsgId: "c1",
    delivery: "delivered",
  }
  const pending = (clientMsgId: string) => ({
    sessionId: "s1",
    clientMsgId,
    text: "again",
    priority: "normal" as const,
    at,
    delivery: "failed" as const,
    failure: "too_many" as const,
  })

  it("shows a message the stream has only once, where the stream put it", () => {
    const events = [streamed]
    expect(withOutbox(events, [{ ...pending("c1"), text: "hello" }])).toBe(events)
  })

  it("adds the others after the stream's events, with their stage", () => {
    expect(withOutbox([streamed], [pending("c2")])).toEqual([
      streamed,
      { kind: "user_message", id: "msg:c2", at, text: "again", clientMsgId: "c2", delivery: "failed", failure: "too_many" },
    ])
  })
})
