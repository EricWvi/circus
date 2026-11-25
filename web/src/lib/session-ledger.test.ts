import { afterEach, describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"
import type { SessionLedger, SessionLedgerStatus } from "@/api/types"
import { i18n } from "@/i18n/i18n"

import {
  assignedNumbers,
  holdsUnmergedChanges,
  ledgerEntryView,
  mergeErrorMessage,
  mergeState,
} from "./session-ledger"

afterEach(async () => {
  await i18n.changeLanguage("en")
})

const status = (over: Partial<SessionLedgerStatus> = {}): SessionLedgerStatus => ({
  state: "ready",
  path: "/state/ledger/s1",
  error: "unspecified",
  errorDetail: "",
  collectedAtUnixMs: 1,
  collectionDue: false,
  changesetId: "c1",
  changeCount: 2,
  problemCount: 0,
  updatedAtUnixMs: 1,
  applying: "none",
  ...over,
})

const entry = { path: "web/D?a-x.md", basePath: "", number: "D?a", title: "X", baseTitle: "", added: true, contentChanged: false, moved: false, retitled: false, deleted: false, problems: [] }

const ledger = (over: Partial<SessionLedger> = {}): SessionLedger => ({
  status: status(),
  changeset: {
    changesetId: "c1",
    state: "open",
    baseCommit: "b",
    createdAtUnixMs: 1,
    updatedAtUnixMs: 1,
    mergedCommit: "",
    discardReason: "",
  },
  entries: [entry],
  problems: [],
  warnings: [],
  fastForward: true,
  merged: [],
  ...over,
})

describe("the header entry", () => {
  it("is hidden without a snapshot", () => {
    expect(ledgerEntryView(undefined)).toEqual({ kind: "none" })
    expect(ledgerEntryView(status({ state: "unspecified" }))).toEqual({ kind: "none" })
  })

  it("is muted while the snapshot is prepared", () => {
    expect(ledgerEntryView(status({ state: "pending" }))).toEqual({ kind: "preparing" })
    expect(ledgerEntryView(status({ state: "preparing" }))).toEqual({ kind: "preparing" })
  })

  it("says why the snapshot failed", () => {
    expect(ledgerEntryView(status({ state: "failed", error: "no_space" }))).toEqual({
      kind: "failed",
      reason: "the device is out of disk space.",
    })
  })

  it("counts the changes once ready, marking problems", () => {
    expect(ledgerEntryView(status())).toEqual({ kind: "ready", changes: 2, problems: false })
    expect(ledgerEntryView(status({ problemCount: 1 }))).toEqual({ kind: "ready", changes: 2, problems: true })
    expect(ledgerEntryView(status({ changeCount: 0, changesetId: "" }))).toEqual({ kind: "ready", changes: 0, problems: false })
  })
})

describe("the merge control", () => {
  it("is enabled for an open changeset with changes that fast-forwards, without problems, between turns", () => {
    expect(mergeState(ledger(), { turnRunning: false, deviceOffline: false })).toEqual({ kind: "enabled", changesetId: "c1" })
  })

  it("waits for the turn to end", () => {
    expect(mergeState(ledger(), { turnRunning: true, deviceOffline: false })).toEqual({ kind: "disabled", reason: "Merge after this turn ends" })
  })

  it("waits for an integrator when it doesn't fast-forward, turn or not", () => {
    expect(mergeState(ledger({ fastForward: false }), { turnRunning: false, deviceOffline: false })).toEqual({ kind: "waiting" })
    expect(mergeState(ledger({ fastForward: false }), { turnRunning: true, deviceOffline: false })).toEqual({ kind: "waiting" })
  })

  it("is disabled while there are problems", () => {
    const problems = [{ path: "", message: "too many files" }]
    expect(mergeState(ledger({ problems }), { turnRunning: false, deviceOffline: false })).toEqual({
      kind: "disabled",
      reason: "Ask the agent to fix the problems before merging.",
    })
    expect(mergeState(ledger({ problems, fastForward: false }), { turnRunning: false, deviceOffline: false }).kind).toBe("disabled")
  })

  it("is disabled when the device is offline with changes not collected yet", () => {
    const due = ledger({ status: status({ collectionDue: true }) })
    expect(mergeState(due, { turnRunning: false, deviceOffline: true })).toEqual({
      kind: "disabled",
      reason: "The device is offline and has changes that aren't collected yet. Merge once it is back.",
    })
    // Either alone doesn't block it: the server collects first.
    expect(mergeState(due, { turnRunning: false, deviceOffline: false }).kind).toBe("enabled")
    expect(mergeState(ledger(), { turnRunning: false, deviceOffline: true }).kind).toBe("enabled")
  })

  it("isn't there without changes or a ready snapshot", () => {
    expect(mergeState(ledger({ entries: [] }), { turnRunning: false, deviceOffline: false })).toEqual({ kind: "none" })
    expect(mergeState(ledger({ changeset: undefined }), { turnRunning: false, deviceOffline: false })).toEqual({ kind: "none" })
    expect(mergeState(ledger({ status: status({ state: "failed" }) }), { turnRunning: false, deviceOffline: false })).toEqual({ kind: "none" })
  })

  it("says the turn-running reason in Chinese too", async () => {
    await i18n.changeLanguage("zh-CN")
    expect(mergeState(ledger(), { turnRunning: true, deviceOffline: false })).toEqual({ kind: "disabled", reason: "本轮结束后可合入" })
  })
})

describe("merge errors", () => {
  const refused = (type?: string) =>
    new ApiError("failed_precondition", "refused", undefined, type ? { preconditionType: type } : undefined)

  it("names each refusal", () => {
    expect(mergeErrorMessage(refused("TURN_RUNNING"))).toBe("A turn is running. Merge after it ends.")
    expect(mergeErrorMessage(refused("NOT_FAST_FORWARD"))).toBe(
      "The project's Ledger has changed meanwhile; an integrator merges these changes now.",
    )
    expect(mergeErrorMessage(refused("DEVICE_OFFLINE"))).toBe(
      "The device is offline and has changes that aren't collected yet. Merge once it is back.",
    )
    expect(mergeErrorMessage(refused("PROBLEMS"))).toBe("The changes have problems. Ask the agent to fix them first.")
    expect(mergeErrorMessage(refused("NO_CHANGES"))).toBe("There are no Ledger changes to merge any more.")
    expect(mergeErrorMessage(refused("CHANGESET_CHANGED"))).toBe(
      "The changes have changed since you looked. Check them again, then merge.",
    )
  })

  it("has a message for a refusal it doesn't know, and for other failures", () => {
    expect(mergeErrorMessage(refused())).toBe("The changes can't be merged right now.")
    expect(mergeErrorMessage(refused("toString"))).toBe("The changes can't be merged right now.")
    expect(mergeErrorMessage(new ApiError("unavailable", "x"))).toBe(i18n.t("errors.unreachable"))
  })

  it("names each refusal in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    expect(mergeErrorMessage(refused("TURN_RUNNING"))).toBe("有一轮正在运行，请在它结束后合入。")
    expect(mergeErrorMessage(refused("NOT_FAST_FORWARD"))).toBe("项目的 Ledger 期间已有变化，这些改动现在由 integrator 合入。")
  })
})

describe("unmerged changes", () => {
  it("are an open changeset with changes, or a collection due", () => {
    expect(holdsUnmergedChanges(undefined)).toBe(false)
    expect(holdsUnmergedChanges(status())).toBe(true)
    expect(holdsUnmergedChanges(status({ changesetId: "", changeCount: 0 }))).toBe(false)
    expect(holdsUnmergedChanges(status({ changesetId: "", changeCount: 0, collectionDue: true }))).toBe(true)
  })

  it("assigned numbers come in order", () => {
    expect(assignedNumbers({ "D?c": 94, "D?a": 92, "D?b": 93 })).toEqual([92, 93, 94])
  })
})
