import { afterEach, describe, expect, it } from "vitest"

import type { WorkspaceStatus } from "@/api/types"
import { i18n } from "@/i18n/i18n"

import {
  currentWorkspace,
  formatElapsed,
  showsDetail,
  workspaceBlocks,
  workspaceEventText,
  workspaceReason,
} from "./workspace"

const status = (patch: Partial<WorkspaceStatus>): WorkspaceStatus => ({
  state: "pending",
  waiting: "unspecified",
  device: "devbox",
  path: "~/circus/projects/p1",
  repoUrl: "https://h/r.git",
  background: false,
  action: "unspecified",
  error: "unspecified",
  errorDetail: "",
  attemptStartedAtUnixMs: 0,
  updatedAtUnixMs: 0,
  ...patch,
})

afterEach(async () => {
  await i18n.changeLanguage("en")
})

describe("currentWorkspace", () => {
  const live = (patch: Partial<WorkspaceStatus>, tsUnixMs: number, attemptStartedAtUnixMs = 0) => ({
    status: status(patch),
    tsUnixMs,
    attemptStartedAtUnixMs,
  })

  it("takes whichever the server wrote last", () => {
    const fetched = status({ state: "preparing", attemptStartedAtUnixMs: 900, updatedAtUnixMs: 1_000 })
    expect(currentWorkspace(fetched, null)).toBe(fetched)
    expect(currentWorkspace(undefined, null)).toBeUndefined()
    const ready = currentWorkspace(fetched, live({ state: "ready", path: "/abs" }, 2_000))
    expect(ready).toMatchObject({ state: "ready", path: "/abs", updatedAtUnixMs: 2_000, attemptStartedAtUnixMs: 900 })
  })

  it("keeps a fetch newer than the last event", () => {
    // The event said device_offline; the fetch after it says the wait changed.
    const event = live({ waiting: "device_offline" }, 1_100)
    const fetched = status({ waiting: "retry", updatedAtUnixMs: 1_200 })
    expect(currentWorkspace(fetched, event)).toBe(fetched)
  })

  it("lets the next fetch correct a missed live event", () => {
    // Seen: offline at 1_100. Missed: retry at 1_200. The fetch has 1_200.
    const seen = live({ waiting: "device_offline" }, 1_100)
    expect(currentWorkspace(status({ waiting: "device_offline", updatedAtUnixMs: 1_100 }), seen)?.waiting).toBe("device_offline")
    expect(currentWorkspace(status({ waiting: "retry", updatedAtUnixMs: 1_200 }), seen)?.waiting).toBe("retry")
  })

  it("doesn't flip-flop on equal times: the fetch of the same change wins, every time", () => {
    const event = live({ state: "preparing" }, 1_000, 1_000)
    const fetched = status({ state: "preparing", attemptStartedAtUnixMs: 998, updatedAtUnixMs: 1_000 })
    for (let i = 0; i < 3; i++) expect(currentWorkspace(fetched, event)).toBe(fetched)
  })

  it("uses events that arrive before the first fetch", () => {
    expect(currentWorkspace(undefined, live({ waiting: "daemon_outdated" }, 500))).toMatchObject({
      state: "pending",
      waiting: "daemon_outdated",
      updatedAtUnixMs: 500,
    })
  })

  it("times a preparation from the stream's start, else the fetched one, else the event", () => {
    const fetchedOld = status({ state: "preparing", attemptStartedAtUnixMs: 900, updatedAtUnixMs: 1_000 })
    // A new attempt the stream saw start: not the fetched one's start.
    expect(currentWorkspace(fetchedOld, live({ state: "preparing" }, 2_000, 2_000))?.attemptStartedAtUnixMs).toBe(2_000)
    // A live change of the same attempt the stream didn't see start.
    expect(currentWorkspace(fetchedOld, live({ state: "preparing", background: true }, 2_000))?.attemptStartedAtUnixMs).toBe(900)
    const pending = status({ state: "pending", attemptStartedAtUnixMs: 500, updatedAtUnixMs: 1_000 })
    expect(currentWorkspace(pending, live({ state: "preparing" }, 2_000))?.attemptStartedAtUnixMs).toBe(2_000)
    expect(currentWorkspace(undefined, live({ state: "preparing" }, 2_000))?.attemptStartedAtUnixMs).toBe(2_000)
  })
})

describe("texts", () => {
  it("blocks the session until ready", () => {
    expect(workspaceBlocks(undefined)).toBe(false)
    expect(workspaceBlocks(status({ state: "ready" }))).toBe(false)
    for (const state of ["pending", "preparing", "failed"] as const) expect(workspaceBlocks(status({ state }))).toBe(true)
  })

  it("translates reasons, with the remote and the device where they belong", async () => {
    expect(workspaceReason("not_empty", { device: "d", detail: "" })).toBe("the folder already contains files")
    expect(workspaceReason("remote_mismatch", { device: "d", detail: "git@h:o/r" })).toBe(
      "the folder is a repository of a different remote (git@h:o/r)",
    )
    expect(workspaceReason("git_unavailable", { device: "devbox", detail: "" })).toBe("git is not installed on devbox")
    for (const error of ["io", "daemon_fault", "cancelled", "invalid_request", "unspecified"] as const) {
      expect(workspaceReason(error, { device: "d", detail: "x" })).toBe("something went wrong on the device; see its log")
    }
    await i18n.changeLanguage("zh-CN")
    expect(workspaceReason("git_unavailable", { device: "devbox", detail: "" })).toBe("devbox 上没有安装 git")
  })

  it("shows the device's words apart, except the remote a mismatch already names", () => {
    expect(showsDetail({ error: "clone_failed", errorDetail: "fatal: x" })).toBe(true)
    expect(showsDetail({ error: "remote_mismatch", errorDetail: "git@h:o/r" })).toBe(false)
    expect(showsDetail({ error: "not_empty", errorDetail: "" })).toBe(false)
  })

  it("words each recorded change", async () => {
    const path = "/home/u/circus/projects/p1"
    expect(workspaceEventText(status({ state: "ready", action: "cloned", path }))).toBe(`Cloned https://h/r.git into ${path}`)
    expect(workspaceEventText(status({ state: "ready", action: "used_repository", path }))).toBe(
      `Using the existing repository at ${path}`,
    )
    expect(workspaceEventText(status({ state: "ready", action: "created", path }))).toBe(`Created ${path}`)
    expect(workspaceEventText(status({ state: "ready", action: "used_empty", path }))).toBe(`Using the empty folder ${path}`)
    expect(workspaceEventText(status({ state: "preparing", repoUrl: "" }))).toBe("Creating ~/circus/projects/p1")
    expect(workspaceEventText(status({ state: "failed", error: "no_space" }))).toBe(
      "Couldn't prepare ~/circus/projects/p1: the device is out of disk space",
    )
    expect(workspaceEventText(status({ waiting: "device_offline" }))).toBe(
      "Waiting for devbox to come online to prepare the folder",
    )
    await i18n.changeLanguage("zh-CN")
    expect(workspaceEventText(status({ state: "ready", action: "cloned", path }))).toBe(`已把 https://h/r.git 克隆到 ${path}`)
  })

  it("counts elapsed time by the second", async () => {
    expect(formatElapsed(1_000, 1_500)).toBe("0s")
    expect(formatElapsed(0, 42_000)).toBe("42s")
    expect(formatElapsed(0, 185_000)).toBe("3m 5s")
    expect(formatElapsed(0, 4_330_000)).toBe("1h 12m")
    expect(formatElapsed(5_000, 0)).toBe("0s")
    await i18n.changeLanguage("zh-CN")
    expect(formatElapsed(0, 185_000)).toBe("3 分 5 秒")
  })
})
