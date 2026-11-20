// @vitest-environment happy-dom
import { act, cleanup, renderHook } from "@testing-library/react"
import { useState, type PointerEvent, type ReactNode } from "react"
import { afterEach, describe, expect, it, vi } from "vitest"

import { ClientContext } from "@/api/context"
import { ApiError } from "@/api/errors"
import type { CircusClient, Device, GitRemote } from "@/api/types"
import type { LocationRow } from "@/lib/location-form"
import { MAX_PRESS_MS } from "@/lib/pointer-press"

import { useFolderRemote, type PathCommit } from "./use-folder-remote"

const devices: Device[] = [
  { name: "devbox", online: true, lastSeenUnixMs: 0, os: "linux", arch: "amd64", version: "0.1.0" },
  { name: "eric-mbp", online: true, lastSeenUnixMs: 0, os: "darwin", arch: "arm64", version: "0.1.0" },
]

const PALACE = "https://git.example.com/infra/palace.git"
const CIRCUS = "https://github.com/eric/circus.git"
const DOTFILES = "git@github.com:eric/dotfiles.git"

interface Pending {
  device: string
  path: string
  resolve: (remote: GitRemote) => void
  reject: (error: unknown) => void
}

afterEach(() => {
  cleanup()
  document.body.replaceChildren()
})

/** The hook inside a small form, against a device whose answers the test gives. */
function setup({
  rows: initialRows,
  repoUrl: initialUrl = "",
  busy: initialBusy = false,
}: {
  rows: LocationRow[]
  repoUrl?: string
  busy?: boolean
}) {
  const asked: Pending[] = []
  const client = {
    machines: {
      getGitRemote: (req: { device: string; path: string }) =>
        new Promise<GitRemote>((resolve, reject) => asked.push({ ...req, resolve, reject })),
    },
  } as unknown as CircusClient
  const wrapper = ({ children }: { children: ReactNode }) => (
    <ClientContext.Provider value={client}>{children}</ClientContext.Provider>
  )
  const { result } = renderHook(
    () => {
      const [rows, setRows] = useState(initialRows)
      const [repoUrl, setRepoUrl] = useState(initialUrl)
      const [busy, setBusy] = useState(initialBusy)
      const remote = useFolderRemote({ rows, devices, repoUrl, setRepoUrl, busy, repoUrlId: "repo-url" })
      return { remote, rows, setRows, repoUrl, setRepoUrl, setBusy }
    },
    { wrapper },
  )
  const commit = (key: string, how: PathCommit, patch: Partial<LocationRow> = {}, leavingToSubmit = false) =>
    act(() => {
      const row = { ...result.current.rows.find((r) => r.key === key)!, ...patch }
      result.current.setRows(result.current.rows.map((r) => (r.key === key ? row : r)))
      result.current.remote.pathCommitted(row, how, leavingToSubmit)
    })
  const setPath = (key: string, path: string) =>
    act(() => result.current.setRows(result.current.rows.map((r) => (r.key === key ? { ...r, path } : r))))
  const answer = async (i: number, remote: GitRemote | Error) => {
    await act(async () => {
      if (remote instanceof Error) asked[i].reject(remote)
      else asked[i].resolve(remote)
    })
  }
  return { result, asked, commit, setPath, answer }
}

const row = (key: string, device: string, path: string): LocationRow => ({ key, device, path })

describe("useFolderRemote reads", () => {
  it("a picked folder, and a typed one when the path changed since the last answer", async () => {
    const { asked, commit, answer } = setup({ rows: [row("a", "devbox", "")] })
    commit("a", "picked", { path: "/home/eric/src/circus" })
    expect(asked.map((a) => [a.device, a.path])).toEqual([["devbox", "/home/eric/src/circus"]])
    await answer(0, { url: CIRCUS })
    // Leaving the field with the value just read doesn't read again.
    commit("a", "blurred")
    expect(asked).toHaveLength(1)
    commit("a", "blurred", { path: " /home/eric/src/dotfiles " })
    expect(asked.map((a) => a.path)).toEqual(["/home/eric/src/circus", "/home/eric/src/dotfiles"])
    await answer(1, { url: DOTFILES })
    commit("a", "blurred")
    expect(asked).toHaveLength(2)
    // Picking reads the same folder again once nothing is in flight.
    commit("a", "picked")
    expect(asked).toHaveLength(3)
  })

  it("again after the row's device changed", async () => {
    const { asked, commit, answer } = setup({ rows: [row("a", "devbox", "/srv/palace")] })
    commit("a", "blurred")
    await answer(0, { url: PALACE })
    commit("a", "blurred", { device: "eric-mbp" })
    expect(asked.map((a) => a.device)).toEqual(["devbox", "eric-mbp"])
  })

  it("again when the path returns to a value whose answer was dropped", async () => {
    const { result, asked, commit, setPath, answer } = setup({ rows: [row("a", "devbox", "")] })
    commit("a", "blurred", { path: "/srv/palace" })
    setPath("a", "/srv/palacex")
    await answer(0, { url: PALACE })
    commit("a", "blurred", { path: "/srv/palace" })
    expect(asked).toHaveLength(2)
    await answer(1, { url: PALACE })
    expect(result.current.repoUrl).toBe(PALACE)
  })

  it("not again after a failure for the same path, until it is picked", async () => {
    const { asked, commit, answer } = setup({ rows: [row("a", "devbox", "/srv/palace")] })
    commit("a", "blurred")
    await answer(0, new ApiError("failed_precondition", "offline"))
    commit("a", "blurred")
    expect(asked).toHaveLength(1)
    commit("a", "picked")
    expect(asked).toHaveLength(2)
  })

  it("once per folder while a request is in flight, sharing it between rows", async () => {
    const { result, asked, commit, answer } = setup({
      rows: [row("a", "devbox", ""), row("b", "devbox", "")],
      repoUrl: "x",
    })
    for (let i = 0; i < 3; i++) commit("a", "picked", { path: "/srv/palace" })
    commit("a", "blurred")
    commit("b", "blurred", { path: " /srv/palace" })
    expect(asked).toHaveLength(1)
    await answer(0, { url: PALACE })
    expect(result.current.remote.suggestion).toMatchObject({ kind: "differs", url: PALACE })
    // Both rows took the answer: neither reads again on blur.
    commit("a", "blurred")
    commit("b", "blurred")
    expect(asked).toHaveLength(1)
  })

  it.each([
    ["the default directory", "devbox", "  "],
    ["a relative path", "devbox", "src/circus"],
    ["a path on an unknown device", "nope", "/srv/palace"],
    ["a path without a device", "", "/srv/palace"],
  ])("nothing for %s", (_, device, path) => {
    const { asked, commit } = setup({ rows: [row("a", device, path)] })
    commit("a", "blurred")
    commit("a", "picked")
    expect(asked).toHaveLength(0)
  })
})

describe("useFolderRemote answers", () => {
  it("fill an empty field, with a note until the field is edited", async () => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "")], repoUrl: "  " })
    commit("a", "picked", { path: "/home/eric/src/dotfiles" })
    await answer(0, { url: DOTFILES })
    expect(result.current.repoUrl).toBe(DOTFILES)
    expect(result.current.remote.suggestion).toEqual({
      kind: "filled",
      rowKey: "a",
      device: "devbox",
      path: "/home/eric/src/dotfiles",
      url: DOTFILES,
    })
    act(() => result.current.remote.editRepoUrl("git@github.com:eric/dotfiles.gi"))
    expect(result.current.remote.suggestion).toBeNull()
    // Typing the same URL back doesn't bring the note back.
    act(() => result.current.remote.editRepoUrl(DOTFILES))
    expect(result.current.remote.suggestion).toBeNull()
  })

  it("hint at another URL without overwriting, and use it on request", async () => {
    const { result, commit, answer } = setup({
      rows: [row("a", "devbox", "/srv/palace")],
      repoUrl: "https://example.com/mine.git",
    })
    commit("a", "blurred")
    await answer(0, { url: PALACE })
    expect(result.current.repoUrl).toBe("https://example.com/mine.git")
    expect(result.current.remote.suggestion).toMatchObject({ kind: "differs", path: "/srv/palace", url: PALACE })
    act(() => result.current.remote.applySuggested())
    expect(result.current.repoUrl).toBe(PALACE)
    expect(result.current.remote.suggestion).toMatchObject({ kind: "filled" })
  })

  it("leave the field alone when it holds the same URL, ignoring surrounding spaces", async () => {
    const { result, commit, answer } = setup({
      rows: [row("a", "devbox", "/srv/palace")],
      repoUrl: ` ${PALACE} `,
    })
    commit("a", "blurred")
    await answer(0, { url: PALACE })
    expect(result.current.repoUrl).toBe(` ${PALACE} `)
    expect(result.current.remote.suggestion).toBeNull()
  })

  it("hide a hint while the field holds its URL", async () => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "/srv/palace")], repoUrl: "x" })
    commit("a", "blurred")
    await answer(0, { url: PALACE })
    act(() => result.current.remote.editRepoUrl(PALACE))
    expect(result.current.remote.suggestion).toBeNull()
  })

  it("replace the note or hint with the latest one", async () => {
    const { result, commit, answer } = setup({
      rows: [row("a", "devbox", ""), row("b", "eric-mbp", "")],
      repoUrl: "https://example.com/mine.git",
    })
    commit("a", "picked", { path: "/home/eric/src/circus" })
    commit("b", "picked", { path: "/Users/eric/src/dotfiles" })
    await answer(1, { url: DOTFILES })
    expect(result.current.remote.suggestion).toMatchObject({ kind: "differs", device: "eric-mbp" })
    await answer(0, { url: CIRCUS })
    expect(result.current.remote.suggestion).toMatchObject({ kind: "differs", device: "devbox", url: CIRCUS })
  })

  it.each([
    ["the path changed", { path: "/home/eric/src/toolbox" }],
    ["the device changed", { device: "eric-mbp" }],
  ])("are dropped when %s before they arrive", async (_, patch) => {
    const { result, asked, commit, answer } = setup({ rows: [row("a", "devbox", "")] })
    commit("a", "picked", { path: "/home/eric/src/circus" })
    act(() => result.current.setRows([{ ...result.current.rows[0], ...patch }]))
    await answer(0, { url: CIRCUS })
    expect(result.current.repoUrl).toBe("")
    expect(result.current.remote.suggestion).toBeNull()
    expect(asked).toHaveLength(1)
  })

  it("are dropped when the row is gone", async () => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "/srv/palace")] })
    commit("a", "blurred")
    act(() => result.current.setRows([]))
    await answer(0, { url: PALACE })
    expect(result.current.repoUrl).toBe("")
  })

  it.each<[string, GitRemote | Error]>([
    ["an invalid URL", { url: "not a url" }],
    ["an option-like URL", { url: "-uhost:x" }],
    ["an empty URL", { url: "" }],
    ["no origin", { url: "", error: "no_origin" }],
    ["not a repository", { url: "", error: "not_repository" }],
    ["an offline device", new ApiError("failed_precondition", "offline")],
    ["a device too old to read remotes", new ApiError("unavailable", "too old")],
  ])("change nothing for %s", async (_, remote) => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "/srv/palace")] })
    commit("a", "blurred")
    await answer(0, remote)
    expect(result.current.repoUrl).toBe("")
    expect(result.current.remote.suggestion).toBeNull()
  })
})

describe("useFolderRemote suggestions belong to their row", () => {
  async function hinted() {
    const setupResult = setup({
      rows: [row("a", "devbox", "/srv/palace"), row("b", "devbox", "")],
      repoUrl: "https://example.com/mine.git",
    })
    setupResult.commit("a", "blurred")
    await setupResult.answer(0, { url: PALACE })
    expect(setupResult.result.current.remote.suggestion).toMatchObject({ kind: "differs", rowKey: "a" })
    return setupResult
  }

  it("a hint goes with its row, and can't be used then", async () => {
    const { result } = await hinted()
    act(() => result.current.setRows(result.current.rows.filter((r) => r.key !== "a")))
    expect(result.current.remote.suggestion).toBeNull()
    act(() => result.current.remote.applySuggested())
    expect(result.current.repoUrl).toBe("https://example.com/mine.git")
  })

  it.each([
    ["path", { path: "/home/eric/src/circus" }],
    ["device", { device: "eric-mbp" }],
  ])("a hint hides while its row's %s differs", async (_, patch) => {
    const { result } = await hinted()
    act(() => result.current.setRows(result.current.rows.map((r) => (r.key === "a" ? { ...r, ...patch } : r))))
    expect(result.current.remote.suggestion).toBeNull()
    // Back to the folder it was read for, it shows again.
    act(() => result.current.setRows(result.current.rows.map((r) => (r.key === "a" ? row("a", "devbox", "/srv/palace ") : r))))
    expect(result.current.remote.suggestion).toMatchObject({ kind: "differs" })
  })

  it.each([
    ["path changes", (r: ReturnType<typeof setup>["result"]) => r.current.setRows([row("a", "devbox", "/srv")])],
    ["device changes", (r: ReturnType<typeof setup>["result"]) => r.current.setRows([row("a", "eric-mbp", "/srv/palace")])],
    ["row goes", (r: ReturnType<typeof setup>["result"]) => r.current.setRows([])],
  ])("a fill note stays while the field keeps its URL, though its row's %s", async (_, change) => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "")] })
    commit("a", "picked", { path: "/srv/palace" })
    await answer(0, { url: PALACE })
    act(() => change(result))
    expect(result.current.repoUrl).toBe(PALACE)
    expect(result.current.remote.suggestion).toMatchObject({ kind: "filled", path: "/srv/palace", url: PALACE })
    act(() => result.current.remote.editRepoUrl(`${PALACE} `))
    expect(result.current.remote.suggestion).toBeNull()
  })

  it("a note for a URL chosen with Use this URL stays when its row's path changes", async () => {
    const { result, commit, setPath, answer } = setup({ rows: [row("a", "devbox", "/srv/palace")], repoUrl: "mine" })
    commit("a", "blurred")
    await answer(0, { url: PALACE })
    act(() => result.current.remote.applySuggested())
    setPath("a", "/home/eric/src/palace")
    expect(result.current.remote.suggestion).toMatchObject({ kind: "filled", url: PALACE })
  })

  it("an answer equal to a noted URL moves the note to its folder", async () => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "/srv/palace")], repoUrl: "mine" })
    commit("a", "blurred")
    await answer(0, { url: PALACE })
    act(() => result.current.remote.applySuggested())
    commit("a", "picked", { path: "/home/eric/src/shared/palace" })
    await answer(1, { url: PALACE })
    expect(result.current.remote.suggestion).toMatchObject({ kind: "filled", path: "/home/eric/src/shared/palace" })
  })

  it("an answer equal to the field clears what its row said before", async () => {
    const { result, commit, setPath, answer } = setup({
      rows: [row("a", "devbox", "/srv/palace")],
      repoUrl: CIRCUS,
    })
    commit("a", "blurred")
    await answer(0, { url: PALACE })
    commit("a", "blurred", { path: "/home/eric/src/circus" })
    await answer(1, { url: CIRCUS })
    expect(result.current.remote.suggestion).toBeNull()
    // The palace hint doesn't come back with the path.
    setPath("a", "/srv/palace")
    expect(result.current.remote.suggestion).toBeNull()
  })

  it("an answer equal to the hook's own fill moves the note to its row", async () => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", ""), row("b", "eric-mbp", "")] })
    commit("a", "picked", { path: "/srv/palace" })
    await answer(0, { url: PALACE })
    commit("b", "picked", { path: "/srv/palace" })
    await answer(1, { url: PALACE })
    expect(result.current.remote.suggestion).toMatchObject({ kind: "filled", rowKey: "b", device: "eric-mbp" })
  })

  it("a reset forgets the hint, the fill and the last reads", async () => {
    const { result, asked, commit } = await hinted()
    act(() => result.current.remote.reset())
    expect(result.current.remote.suggestion).toBeNull()
    commit("a", "blurred")
    expect(asked).toHaveLength(2)
  })
})

describe("useFolderRemote fills only the person's empty field", () => {
  it("replaces its own unedited fill with a newer answer, from the same row or another", async () => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", ""), row("b", "devbox", "")] })
    commit("a", "blurred", { path: "/home/eric" })
    await answer(0, { url: DOTFILES })
    expect(result.current.repoUrl).toBe(DOTFILES)
    commit("a", "picked", { path: "/home/eric/src/circus" })
    await answer(1, { url: CIRCUS })
    expect(result.current.repoUrl).toBe(CIRCUS)
    expect(result.current.remote.suggestion).toMatchObject({ kind: "filled", path: "/home/eric/src/circus" })
    commit("b", "picked", { path: "/srv/palace" })
    await answer(2, { url: PALACE })
    expect(result.current.repoUrl).toBe(PALACE)
    expect(result.current.remote.suggestion).toMatchObject({ kind: "filled", rowKey: "b" })
  })

  it.each([
    ["edited", (r: ReturnType<typeof setup>["result"]) => r.current.remote.editRepoUrl(`${PALACE}x`)],
    ["chosen with Use this URL", null],
  ])("keeps a URL the person %s, hinting instead", async (_, edit) => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "")], repoUrl: edit ? "" : "mine" })
    commit("a", "picked", { path: "/srv/palace" })
    await answer(0, { url: PALACE })
    act(() => (edit ? edit(result) : result.current.remote.applySuggested()))
    const kept = result.current.repoUrl
    commit("a", "picked", { path: "/home/eric/src/circus" })
    await answer(1, { url: CIRCUS })
    expect(result.current.repoUrl).toBe(kept)
    expect(result.current.remote.suggestion).toMatchObject({ kind: "differs", url: CIRCUS })
  })

  it("only hints when the path was left for a submit button", async () => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "")] })
    commit("a", "blurred", { path: "/srv/palace" }, true)
    await answer(0, { url: PALACE })
    expect(result.current.repoUrl).toBe("")
    expect(result.current.remote.suggestion).toMatchObject({ kind: "differs", url: PALACE })
  })

  it("only hints for reads sent while a submit button is pressed, until a tick after its release", async () => {
    const form = document.createElement("form")
    const submit = document.createElement("button")
    submit.type = "submit"
    form.append(submit)
    document.body.append(form)
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", ""), row("b", "devbox", "")] })
    act(() => result.current.remote.formPointerDown({ target: submit, button: 0 } as unknown as PointerEvent))
    commit("a", "blurred", { path: "/srv/palace" })
    await answer(0, { url: PALACE })
    expect(result.current.repoUrl).toBe("")
    expect(result.current.remote.suggestion).toMatchObject({ kind: "differs" })
    submit.dispatchEvent(new Event("pointerup", { bubbles: true }))
    await act(() => new Promise((resolve) => setTimeout(resolve, 0)))
    commit("b", "blurred", { path: "/home/eric/src/circus" })
    await answer(1, { url: CIRCUS })
    expect(result.current.repoUrl).toBe(CIRCUS)
  })

  it("ignores a press of another mouse button on a submit button", async () => {
    const submit = document.createElement("button")
    submit.type = "submit"
    document.body.append(submit)
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "")] })
    act(() => result.current.remote.formPointerDown({ target: submit, button: 2 } as unknown as PointerEvent))
    commit("a", "blurred", { path: "/srv/palace" })
    await answer(0, { url: PALACE })
    expect(result.current.repoUrl).toBe(PALACE)
  })

  it.each([
    ["a context menu opens", () => document.dispatchEvent(new Event("contextmenu"))],
    ["the window loses focus", () => window.dispatchEvent(new Event("blur"))],
    ["the press outlasts its limit", () => vi.advanceTimersByTime(MAX_PRESS_MS)],
  ])("stops counting a press without a release when %s", async (_, happen) => {
    vi.useFakeTimers()
    try {
      const submit = document.createElement("button")
      submit.type = "submit"
      document.body.append(submit)
      const { result, commit, answer } = setup({ rows: [row("a", "devbox", "")] })
      act(() => result.current.remote.formPointerDown({ target: submit, button: 0 } as unknown as PointerEvent))
      act(() => happen())
      commit("a", "blurred", { path: "/srv/palace" })
      await answer(0, { url: PALACE })
      expect(result.current.repoUrl).toBe(PALACE)
    } finally {
      vi.useRealTimers()
    }
  })

  it("ignores pointerdown on anything but a submit button", async () => {
    const other = document.createElement("button")
    other.type = "button"
    document.body.append(other)
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "")] })
    act(() => result.current.remote.formPointerDown({ target: other, button: 0 } as unknown as PointerEvent))
    commit("a", "blurred", { path: "/srv/palace" })
    await answer(0, { url: PALACE })
    expect(result.current.repoUrl).toBe(PALACE)
  })

  it("only hints when a submit started after the read was sent, not before", async () => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "")] })
    commit("a", "picked", { path: "/srv/palace" })
    act(() => result.current.remote.submitStarted())
    await answer(0, { url: PALACE })
    expect(result.current.repoUrl).toBe("")
    expect(result.current.remote.suggestion).toMatchObject({ kind: "differs" })
    commit("a", "picked", { path: "/home/eric/src/circus" })
    await answer(1, { url: CIRCUS })
    expect(result.current.repoUrl).toBe(CIRCUS)
  })

  it("only hints while the repository URL field has focus", async () => {
    const input = document.createElement("input")
    input.id = "repo-url"
    document.body.append(input)
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "")] })
    commit("a", "picked", { path: "/srv/palace" })
    act(() => input.focus())
    await answer(0, { url: PALACE })
    expect(result.current.repoUrl).toBe("")
    expect(result.current.remote.suggestion).toMatchObject({ kind: "differs" })
  })

  it("only hints while saving, and uses the hint once saving is done", async () => {
    const { result, commit, answer } = setup({ rows: [row("a", "devbox", "/srv/palace")], busy: true })
    commit("a", "blurred")
    await answer(0, { url: PALACE })
    expect(result.current.repoUrl).toBe("")
    expect(result.current.remote.suggestion).toMatchObject({ kind: "differs" })
    act(() => result.current.remote.applySuggested())
    expect(result.current.repoUrl).toBe("")
    act(() => result.current.setBusy(false))
    act(() => result.current.remote.applySuggested())
    expect(result.current.repoUrl).toBe(PALACE)
  })
})
