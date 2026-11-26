import { afterEach, describe, expect, it } from "vitest"

import type { WorkerAgent } from "@/api/types"
import { i18n } from "@/i18n/i18n"

import {
  agentActions,
  agentBusy,
  agentOf,
  formatBytes,
  installProgress,
  installs,
  lastReportedAt,
  localWorker,
  pendingWish,
  runsAgent,
  shownError,
  shownState,
  type AgentContext,
} from "./agents"

const agent = (fields: Partial<WorkerAgent> = {}): WorkerAgent => ({
  agent: "claude_code",
  state: "installed",
  installedVersion: "2.1.3",
  targetVersion: "2.1.3",
  error: "",
  downloadedBytes: 0,
  totalBytes: 0,
  wanted: true,
  wantedVersion: "2.1.3",
  reportedAtUnixMs: 1,
  ...fields,
})

const notWanted = { wanted: false, wantedVersion: "" }

afterEach(async () => {
  await i18n.changeLanguage("en")
})

const online = { online: true }
const offline = { online: false }

describe("a worker's agent", () => {
  it("is the worker's entry for it, or none when it was neither reported nor asked for", () => {
    expect(agentOf({ agents: [] }, "claude_code")).toBeUndefined()
    const reported = agent({ state: "failed" })
    expect(agentOf({ agents: [reported] }, "claude_code")).toBe(reported)
  })

  it.each<[string, Partial<WorkerAgent>, AgentContext, "install" | "uninstall" | null]>([
    ["installed and wanted", {}, online, null],
    ["installed, not wanted", { ...notWanted }, online, "uninstall"],
    ["installed, not wanted, offline", { ...notWanted }, offline, "uninstall"],
    ["outdated, not wanted", { state: "outdated", installedVersion: "2.0.9", ...notWanted }, online, "uninstall"],
    ["not installed, wanted at the target version", { state: "not_installed", installedVersion: "" }, online, "install"],
    ["not installed, wanted at an older version", { state: "not_installed", wantedVersion: "2.0.9" }, online, null],
    // No version on either side is no match: the wish names none.
    ["not installed, wanted without a version", { state: "not_installed", wantedVersion: "", targetVersion: "" }, online, null],
    ["outdated, update asked for", { state: "outdated", installedVersion: "2.0.9" }, online, "install"],
    ["outdated, wanted at the old version", { state: "outdated", installedVersion: "2.0.9", wantedVersion: "2.0.9" }, online, null],
    // A state this client doesn't know carries no wish.
    ["unknown, wanted", { state: "unknown" }, online, null],
    ["unknown, not wanted", { state: "unknown", ...notWanted }, online, null],
    ["failed, wanted, online", { state: "failed" }, online, null],
    // Installed again when the worker connects.
    ["failed, wanted, offline", { state: "failed" }, offline, "install"],
    // Retried, and the worker hasn't reported since the answer.
    ["failed, retried, not reported since", { state: "failed" }, { online: true, answeredReportAt: 1 }, "install"],
    ["failed, retried, reported since", { state: "failed" }, { online: true, answeredReportAt: 0 }, null],
    ["failed, wanted at an older version, offline", { state: "failed", wantedVersion: "2.0.9" }, offline, null],
    ["installing", { state: "installing" }, online, null],
    ["uninstalling, not wanted", { state: "uninstalling", ...notWanted }, online, null],
  ])("waits for the worker when %s", (_, fields, ctx, wish) => {
    expect(pendingWish(agent(fields), ctx)).toBe(wish)
  })

  it.each<[string, Partial<WorkerAgent>, AgentContext, string[]]>([
    ["installed", {}, online, ["uninstall"]],
    ["installed, uninstall asked for", { ...notWanted }, offline, ["keep"]],
    ["not installed", { state: "not_installed", ...notWanted }, online, ["install"]],
    ["not installed, install asked for", { state: "not_installed" }, offline, ["cancel"]],
    ["not installed, wanted by an older worker build", { state: "not_installed", wantedVersion: "2.0.9" }, online, ["install"]],
    ["in a state this client doesn't know", { state: "unknown", ...notWanted }, online, ["install"]],
    ["outdated", { state: "outdated", installedVersion: "2.0.9", wantedVersion: "2.0.9" }, online, ["update", "uninstall"]],
    ["outdated, update asked for", { state: "outdated", installedVersion: "2.0.9" }, offline, ["uninstall"]],
    // Keeping it means updating it: the worker installs its target version.
    ["outdated, uninstall asked for", { state: "outdated", ...notWanted }, offline, ["update"]],
    ["failed install", { state: "failed", error: "disk full" }, online, ["retry", "dismiss"]],
    ["failed install, retried", { state: "failed" }, { online: true, answeredReportAt: 1 }, ["dismiss"]],
    ["failed install, offline", { state: "failed" }, offline, ["dismiss"]],
    ["failed uninstall", { state: "failed", ...notWanted }, online, ["install"]],
    ["installing", { state: "installing" }, online, []],
    ["uninstalling", { state: "uninstalling", ...notWanted }, online, []],
  ])("offers what can be done when %s", (_, fields, ctx, actions) => {
    expect(agentActions(agent(fields), ctx)).toEqual(actions)
  })

  it("installs for install, update, retry and keep, and not for uninstall, cancel and dismiss", () => {
    const actions = ["install", "update", "retry", "keep", "uninstall", "cancel", "dismiss"] as const
    expect(actions.map(installs)).toEqual([true, true, true, true, false, false, false])
  })

  it("shows a failure nobody wants installed as not installed, without its error", () => {
    const failed = agent({ state: "failed", error: "disk full" })
    expect([shownState(failed), shownError(failed)]).toEqual(["failed", "disk full"])
    const givenUp = { ...failed, ...notWanted }
    expect([shownState(givenUp), shownError(givenUp)]).toEqual(["not_installed", ""])
    expect(shownState(agent({ state: "installed", ...notWanted }))).toBe("installed")
    expect(shownError(agent({ error: "stale" }))).toBe("")
  })

  it("keeps the worker list polling fast while an online worker works on an agent, or will", () => {
    const worker = (isOnline: boolean, fields: Partial<WorkerAgent>) => ({ online: isOnline, agents: [agent(fields)] })
    expect(agentBusy(worker(true, {}))).toBe(false)
    expect(agentBusy(worker(true, { state: "installing" }))).toBe(true)
    expect(agentBusy(worker(true, { state: "uninstalling", ...notWanted }))).toBe(true)
    expect(agentBusy(worker(true, { state: "not_installed" }))).toBe(true)
    expect(agentBusy(worker(true, { state: "unknown" }))).toBe(false)
    expect(agentBusy(worker(true, { state: "failed" }))).toBe(false)
    expect(agentBusy(worker(true, { state: "failed" }), () => 1)).toBe(true)
    // An offline worker's report is old news, and it acts on no wish until it connects.
    expect(agentBusy(worker(false, { state: "installing" }))).toBe(false)
    expect(agentBusy(worker(false, { state: "uninstalling", ...notWanted }))).toBe(false)
    expect(agentBusy(worker(false, { state: "not_installed" }))).toBe(false)
    expect(agentBusy({ online: true, agents: [] })).toBe(false)
  })

  it("was last reported when the worker's latest report came", () => {
    expect(lastReportedAt({ agents: [] })).toBe(0)
    expect(lastReportedAt({ agents: [agent({ reportedAtUnixMs: 7 }), agent({ reportedAtUnixMs: 9 })] })).toBe(9)
  })

  it("shows download progress only when the total is known", () => {
    expect(installProgress({ downloadedBytes: 37_250_000, totalBytes: 74_500_000 })).toBe(50)
    expect(installProgress({ downloadedBytes: 74_499_999, totalBytes: 74_500_000 })).toBe(99)
    expect(installProgress({ downloadedBytes: 80_000_000, totalBytes: 74_500_000 })).toBe(100)
    expect(installProgress({ downloadedBytes: 12_800_000, totalBytes: 0 })).toBeNull()
  })

  it("formats byte counts in the active language, in kilobytes at the least", async () => {
    expect(formatBytes(0)).toBe("0 kB")
    expect(formatBytes(999)).toBe("1 kB")
    expect(formatBytes(31_200_000)).toBe("31.2 MB")
    expect(formatBytes(745_000_000)).toBe("745 MB")
    expect(formatBytes(1_250_000_000)).toBe("1.3 GB")
    await i18n.changeLanguage("zh-CN")
    expect(formatBytes(31_200_000)).toBe("31.2 MB")
  })

  it("lets sessions run on a worker with it installed at its target version", () => {
    expect(runsAgent({ agents: [] })).toBe(false)
    expect(runsAgent({ agents: [agent({ state: "outdated" })] })).toBe(false)
    expect(runsAgent({ agents: [agent()] })).toBe(true)
  })

  it("pins a session to the worker on its device's machine, if there is one", () => {
    const workers = [
      { name: "cloud", localDevices: [] },
      { name: "mbp", localDevices: ["eric-mbp", "eric-mbp-2"] },
    ]
    expect(localWorker(workers, "eric-mbp-2")?.name).toBe("mbp")
    expect(localWorker(workers, "devbox")).toBeUndefined()
  })
})
