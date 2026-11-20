import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError, type ErrorCode } from "@/api/errors"
import type { CircusClient, DirectoryListing } from "@/api/types"

import { createMockBackend } from "./backend"
import { listMockDirectory, MAX_DIRECTORY_ENTRIES, SLOW_LISTING_MS } from "./directories"
import { seedState } from "./state"

const list = (path: string, includeHidden = false, os = "linux") => {
  const { outcome } = listMockDirectory(os, { path, includeHidden })
  if (outcome instanceof ApiError) throw outcome
  return outcome
}
const names = (listing: DirectoryListing) => listing.entries.map((e) => e.name)

describe("mock directory tree", () => {
  it("resolves the home directory and paths under it", () => {
    for (const path of ["", "~", "~/", "/home/eric", "/home/eric/./src/.."]) {
      expect(list(path)).toMatchObject({ path: "/home/eric", parent: "/home", home: "/home/eric" })
    }
    expect(list("~", false, "darwin")).toMatchObject({ path: "/Users/eric", home: "/Users/eric" })
    expect(list("~/src")).toMatchObject({ path: "/home/eric/src", parent: "/home/eric" })
    expect(list("/")).toMatchObject({ path: "/", parent: "" })
  })

  it("lists subdirectories only, sorted, with git repositories and links marked", () => {
    const home = list("~")
    expect(names(home)).toEqual([
      "Documents",
      "Downloads",
      "circus",
      "many-folders",
      "notes",
      "private",
      "src",
    ])
    expect(home.entries.find((e) => e.name === "notes")).toEqual({ name: "notes", gitRepository: false, symlink: true })
    expect(list("~/src").entries.find((e) => e.name === "circus")).toEqual({
      name: "circus",
      gitRepository: true,
      symlink: false,
    })
    expect(names(list("~/notes"))).toEqual(["invoices", "taxes-2025"])
    expect(list("~/notes").path).toBe("/home/eric/notes")
  })

  it("lists hidden folders on request", () => {
    expect(names(list("~", true))).toEqual(expect.arrayContaining([".cache", ".config", ".ssh"]))
    expect(names(list("~")).some((n) => n.startsWith("."))).toBe(false)
  })

  it("keeps the first entries of a large folder", () => {
    const many = list("~/many-folders")
    expect(many.entries).toHaveLength(MAX_DIRECTORY_ENTRIES)
    expect(many.truncated).toBe(true)
    expect(many.entries.at(-1)?.name).toBe("folder-1000")
    expect(list("~/Downloads")).toMatchObject({ entries: [], truncated: false })
  })

  it.each([
    ["relative", "", "invalid_path"],
    ["/nope", "/nope", "not_found"],
    ["~/README.md", "/home/eric/README.md", "not_directory"],
    ["~/README.md/x", "/home/eric/README.md/x", "not_directory"],
    ["~/private", "/home/eric/private", "permission_denied"],
    ["~/.ssh/keys", "/home/eric/.ssh/keys", "permission_denied"],
  ])("reports %s as a path error", (path, resolved, error) => {
    expect(list(path)).toMatchObject({ path: resolved, entries: [], error })
  })

  it.each([
    ["/mnt/slow", SLOW_LISTING_MS, undefined],
    ["/mnt/stale-nfs", SLOW_LISTING_MS, undefined],
    ["/mnt/busy", 0, "resource_exhausted"],
    ["/mnt/unreachable", 0, "unavailable"],
    ["/mnt/hung", SLOW_LISTING_MS, "deadline_exceeded"],
  ])("misbehaves under %s", (path, delayMs, code) => {
    const result = listMockDirectory("linux", { path, includeHidden: false })
    expect(result.delayMs).toBe(delayMs)
    if (code) expect(result.outcome).toMatchObject({ code })
    else expect(result.outcome).not.toBeInstanceOf(ApiError)
  })
})

describe("listDirectory in the mock backend", () => {
  let client: CircusClient
  let simulation: ReturnType<typeof createMockBackend>["simulation"]
  beforeEach(async () => {
    vi.useFakeTimers()
    const backend = createMockBackend({ state: seedState(), latency: () => 0 })
    client = backend.client
    simulation = backend.simulation
    const login = client.auth.login({ email: "eric@example.com", password: "circus" })
    await vi.runAllTimersAsync()
    await login
  })
  afterEach(() => {
    vi.useRealTimers()
  })

  async function settle<T>(promise: Promise<T>): Promise<T | ApiError> {
    const caught = promise.catch((e: unknown) => e as ApiError)
    await vi.runAllTimersAsync()
    return caught
  }
  const codeOf = async (promise: Promise<unknown>): Promise<ErrorCode | undefined> => {
    const result = await settle(promise)
    return result instanceof ApiError ? result.code : undefined
  }

  it("lists a directory on an online device", async () => {
    const listing = await settle(client.machines.listDirectory({ device: "eric-mbp", path: "", includeHidden: false }))
    expect(listing).toMatchObject({ path: "/Users/eric" })
  })

  it("fails for unknown and offline devices", async () => {
    expect(await codeOf(client.machines.listDirectory({ device: "nope", path: "", includeHidden: false }))).toBe(
      "not_found",
    )
    expect(await codeOf(client.machines.listDirectory({ device: "eric-win", path: "", includeHidden: false }))).toBe(
      "failed_precondition",
    )
    simulation.setDeviceOffline("devbox", true)
    expect(await codeOf(client.machines.listDirectory({ device: "devbox", path: "", includeHidden: false }))).toBe(
      "failed_precondition",
    )
  })

  it("fails the request where the tree says so, after its delay", async () => {
    let done = false
    const pending = client.machines
      .listDirectory({ device: "devbox", path: "/mnt/hung", includeHidden: false })
      .catch((e: unknown) => e as ApiError)
      .finally(() => (done = true))
    await vi.advanceTimersByTimeAsync(SLOW_LISTING_MS - 1)
    expect(done).toBe(false)
    await vi.advanceTimersByTimeAsync(1)
    expect((await pending as ApiError).code).toBe("deadline_exceeded")
  })
})
