import { describe, expect, it } from "vitest"

import {
  approvalLapsed,
  APPROVAL_LIFETIME_MS,
  CLOCK_SKEW_MS,
  showsTurnFailure,
  turnRunning,
  viewState,
} from "./session-state"

describe("viewState", () => {
  it("puts stopped and broken first, then approvals, running and a lost or failed turn", () => {
    const s = (state: Parameters<typeof viewState>[0]["state"], extra = {}) =>
      viewState({ state, lastTurnOutcome: "completed", pendingApprovalCount: 0, ...extra })
    expect(s("stopped", { pendingApprovalCount: 2 })).toBe("stopped")
    expect(s("broken")).toBe("broken")
    expect(s("running", { pendingApprovalCount: 1 })).toBe("awaiting_approval")
    expect(s("running")).toBe("running")
    expect(s("idle", { lastTurnOutcome: "interrupted" })).toBe("interrupted")
    expect(s("idle", { lastTurnOutcome: "error" })).toBe("failed")
    expect(s("running", { lastTurnOutcome: "error" })).toBe("running")
    expect(s("queued")).toBe("queued")
  })

  it("shows a queued session as queued, however its last turn ended", () => {
    const s = (lastTurnOutcome: Parameters<typeof viewState>[0]["lastTurnOutcome"]) =>
      viewState({ state: "queued", lastTurnOutcome, pendingApprovalCount: 0 })
    expect(s("error")).toBe("queued")
    expect(s("interrupted")).toBe("queued")
    expect(s("stopped")).toBe("queued")
  })
})

describe("showsTurnFailure", () => {
  const stream = { lastSeq: 5, turnNo: 2, turnFailureShown: false }
  const failed = { state: "failed" as const, turnCount: 2 }

  it("adds a line only for the failed turn the stream has and shows nothing of", () => {
    expect(showsTurnFailure(stream, failed, false)).toBe(true)
    expect(showsTurnFailure({ ...stream, turnFailureShown: true }, failed, false)).toBe(false)
    expect(showsTurnFailure(stream, { ...failed, state: "idle" }, false)).toBe(false)
    expect(showsTurnFailure(stream, failed, true)).toBe(false)
    // Another turn than the one the server counts, or no stream yet.
    expect(showsTurnFailure({ ...stream, turnNo: 3 }, failed, false)).toBe(false)
    expect(showsTurnFailure({ ...stream, lastSeq: 0 }, failed, false)).toBe(false)
  })
})

describe("approvalLapsed", () => {
  const requested = { requestedAtUnixMs: 1_000_000 }
  const pending = { pendingApprovalCount: 1, lastActivityAtUnixMs: 1_000_000 }

  it("believes the server once it was read after the request", () => {
    expect(approvalLapsed(requested, { pendingApprovalCount: 0, lastActivityAtUnixMs: 1_000_000 }, 0)).toBe(true)
    // Read before the request: not an answer about it.
    expect(approvalLapsed(requested, { pendingApprovalCount: 0, lastActivityAtUnixMs: 999_999 }, 1_000_000)).toBe(false)
  })

  it("leaves a request alone on the browser's clock until well past its lifetime", () => {
    const end = requested.requestedAtUnixMs + APPROVAL_LIFETIME_MS
    expect(approvalLapsed(requested, pending, end)).toBe(false)
    expect(approvalLapsed(requested, pending, end + CLOCK_SKEW_MS - 1)).toBe(false)
    expect(approvalLapsed(requested, pending, end + CLOCK_SKEW_MS)).toBe(true)
  })
})

describe("turnRunning", () => {
  const open = { lastSeq: 9, turnOpen: true, turnNo: 3 }

  it("follows the stream once it has delivered anything", () => {
    expect(turnRunning(open, { serverState: "running", turnCount: 3 })).toBe(true)
    // RESULT came; the server reports the turn's end a moment later.
    expect(turnRunning({ ...open, turnOpen: false }, { serverState: "running", turnCount: 3 })).toBe(false)
    // The turn started before the session was last read.
    expect(turnRunning(open, { serverState: "queued", turnCount: 2 })).toBe(true)
  })

  it("ends a turn the server counts as over, though no RESULT came", () => {
    expect(turnRunning(open, { serverState: "idle", turnCount: 3 })).toBe(false)
    expect(turnRunning(open, { serverState: "queued", turnCount: 4 })).toBe(false)
  })

  it("runs nothing in a stopped or broken session", () => {
    expect(turnRunning(open, { serverState: "broken", turnCount: 3 })).toBe(false)
    expect(turnRunning(open, { serverState: "stopped", turnCount: 2 })).toBe(false)
  })

  it("follows the session before the stream has delivered anything", () => {
    const empty = { lastSeq: 0, turnOpen: false, turnNo: 0 }
    expect(turnRunning(empty, { serverState: "running", turnCount: 1 })).toBe(true)
    expect(turnRunning(empty, { serverState: "idle", turnCount: 1 })).toBe(false)
  })
})
