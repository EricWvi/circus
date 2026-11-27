import { afterEach, describe, expect, it } from "vitest"

import type { RawSessionEvent } from "@/api/types"
import { i18n } from "@/i18n/i18n"

import { applyEvents, emptyTranscript } from "./session-events/transcript"
import { currentWorkerDisconnectedAt, workerDisconnectedAtFromPayload, workerDisconnectedText } from "./worker-link"

const workerEvent = (receivedAtMs: number, at: string): RawSessionEvent => ({
  seq: 0,
  tsUnixMs: 1,
  receivedAtMs,
  kind: "worker",
  payloadJson: JSON.stringify({ disconnected_at_unix_ms: at }),
  ephemeral: true,
  truncated: false,
})

afterEach(async () => {
  await i18n.changeLanguage("en")
})

describe("worker disconnects", () => {
  it("are read from KIND_WORKER payloads; one this client can't read is none", () => {
    expect(workerDisconnectedAtFromPayload({ disconnected_at_unix_ms: "1763380800000" })).toBe(1_763_380_800_000)
    expect(workerDisconnectedAtFromPayload({ disconnected_at_unix_ms: "0" })).toBe(0)
    expect(workerDisconnectedAtFromPayload({ disconnected_at_unix_ms: 5 })).toBe(5)
    expect(workerDisconnectedAtFromPayload({ disconnected_at_unix_ms: "-3" })).toBe(0)
    expect(workerDisconnectedAtFromPayload({ disconnected_at_unix_ms: "99999999999999999999" })).toBe(0)
    expect(workerDisconnectedAtFromPayload({})).toBe(0)
  })

  it("are kept by the transcript, the latest received, without touching the conversation", () => {
    let t = applyEvents(emptyTranscript, [workerEvent(2_000, "1500")])
    expect(t.worker).toEqual({ disconnectedAtUnixMs: 1_500, receivedAtMs: 2_000 })
    // A newer event wins, the clearing one too.
    t = applyEvents(t, [workerEvent(3_000, "2500"), workerEvent(3_100, "0")])
    expect(t.worker).toEqual({ disconnectedAtUnixMs: 0, receivedAtMs: 3_100 })
    expect(t.events).toEqual([])
    expect(t.lastSeq).toBe(0)
    expect(t.queue).toBeNull()
  })

  it("come from the stream when it spoke after the read was sent (the cleared state too), else from the read", () => {
    expect(currentWorkerDisconnectedAt(1_000, 5, null)).toBe(1_000)
    expect(currentWorkerDisconnectedAt(1_000, 5, { disconnectedAtUnixMs: 0, receivedAtMs: 5 })).toBe(0)
    expect(currentWorkerDisconnectedAt(0, 5, { disconnectedAtUnixMs: 2_000, receivedAtMs: 9 })).toBe(2_000)
    expect(currentWorkerDisconnectedAt(0, 10, { disconnectedAtUnixMs: 2_000, receivedAtMs: 9 })).toBe(0)
    expect(currentWorkerDisconnectedAt(0, undefined, { disconnectedAtUnixMs: 2_000, receivedAtMs: 9 })).toBe(2_000)
  })

  it("read as one sentence, warning about a running turn", async () => {
    const now = 10 * 60_000
    expect(workerDisconnectedText(now - 20_000, true, now)).toBe(
      "The worker disconnected just now. If it doesn't reconnect within a minute, this turn will be interrupted.",
    )
    expect(workerDisconnectedText(now - 3 * 60_000, false, now)).toBe(
      "The worker disconnected 3m ago; waiting for it to reconnect.",
    )
    await i18n.changeLanguage("zh-CN")
    expect(workerDisconnectedText(now - 20_000, true, now)).toBe(
      "工作节点连接已断开（刚刚）。1 分钟内未重连，本轮将中断。",
    )
    expect(workerDisconnectedText(now - 3 * 60_000, false, now)).toBe("工作节点连接已断开（3 分钟前），等待重连。")
    // Before the stream told the server's clock: no time.
    expect(workerDisconnectedText(now, true, null)).toBe("工作节点连接已断开。1 分钟内未重连，本轮将中断。")
    expect(workerDisconnectedText(now, false, null)).toBe("工作节点连接已断开，等待重连。")
    await i18n.changeLanguage("en")
    expect(workerDisconnectedText(now, true, null)).toBe(
      "The worker disconnected. If it doesn't reconnect within a minute, this turn will be interrupted.",
    )
    expect(workerDisconnectedText(now, false, null)).toBe("The worker disconnected; waiting for it to reconnect.")
  })
})
