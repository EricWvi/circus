import { describe, expect, it } from "vitest"

import type { RawSessionEvent } from "@/api/types"

import { currentQueueReason, queueReasonFromPayload, queueReasonText } from "./queue-reason"
import { applyEvents, emptyTranscript } from "./session-events/transcript"

const queueEvent = (receivedAtMs: number, reason: string): RawSessionEvent => ({
  seq: 0,
  tsUnixMs: 1,
  receivedAtMs,
  kind: "queue",
  payloadJson: JSON.stringify({ queue_reason: reason }),
  ephemeral: true,
  truncated: false,
})

describe("queue reasons", () => {
  it("are read from KIND_QUEUE payloads; an unknown one reads as none", () => {
    expect(queueReasonFromPayload({ queue_reason: "provider_full" })).toBe("provider_full")
    expect(queueReasonFromPayload({ queue_reason: "agent_not_installed" })).toBe("agent_not_installed")
    expect(queueReasonFromPayload({ queue_reason: "unspecified" })).toBe("unspecified")
    expect(queueReasonFromPayload({ queue_reason: "brand_new" })).toBe("unspecified")
    expect(queueReasonFromPayload({})).toBe("unspecified")
  })

  it("are kept by the transcript, the latest received, without touching the conversation", () => {
    let t = applyEvents(emptyTranscript, [queueEvent(2_000, "provider_full")])
    expect(t.queue).toEqual({ reason: "provider_full", receivedAtMs: 2_000 })
    t = applyEvents(t, [queueEvent(3_000, "unspecified")])
    expect(t.queue).toEqual({ reason: "unspecified", receivedAtMs: 3_000 })
    expect(t.events).toEqual([])
    expect(t.lastSeq).toBe(0)
  })

  it("come from the stream when it spoke after the read was sent, else from the read", () => {
    expect(currentQueueReason("worker_offline", 5, null)).toBe("worker_offline")
    expect(currentQueueReason("worker_offline", 5, { reason: "provider_full", receivedAtMs: 5 })).toBe("provider_full")
    expect(currentQueueReason("worker_offline", 6, { reason: "provider_full", receivedAtMs: 5 })).toBe("worker_offline")
    expect(currentQueueReason("worker_offline", undefined, { reason: "provider_full", receivedAtMs: 5 })).toBe(
      "provider_full",
    )
  })

  it("read as one sentence each, naming the provider", () => {
    const proxy = { name: "Proxy" }
    expect(queueReasonText("provider_full", proxy)).toBe(
      "Proxy already runs as many sessions as it allows. This one starts when one of them ends.",
    )
    expect(queueReasonText("no_assigned_worker", proxy)).toBe("No online worker assigned to Proxy can run this session.")
    expect(queueReasonText("agent_not_installed", undefined)).toBe(
      "No online worker that can run this session has Claude Code installed.",
    )
    expect(queueReasonText("provider_deleted", undefined)).toBe(
      "This session's provider was deleted. Pick another provider to go on.",
    )
    expect(queueReasonText("provider_secrets_unreadable", proxy)).toBe(
      "The key and variable values of Proxy can't be read. Enter them again.",
    )
    expect(queueReasonText("worker_offline", undefined)).toBe("The worker that runs this session is offline.")
    expect(queueReasonText("unspecified", undefined)).toBe("Waiting for a worker to pick this up…")
  })

  it("read as plain waiting until the provider's name is known", () => {
    for (const reason of ["provider_full", "no_assigned_worker", "provider_secrets_unreadable"] as const) {
      expect(queueReasonText(reason, undefined)).toBe("Waiting for a worker to pick this up…")
    }
  })
})
