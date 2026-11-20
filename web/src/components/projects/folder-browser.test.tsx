// @vitest-environment happy-dom
import { cleanup, fireEvent, screen, within } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { ApiError } from "@/api/errors"
import { SLOW_LISTING_MS } from "@/api/mock/directories"
import type { CircusClient } from "@/api/types"
import { advance, renderWithBackend } from "@/test/render"

import { FolderBrowser, SHOW_HIDDEN_KEY } from "./folder-browser"

beforeEach(() => {
  vi.useFakeTimers()
  localStorage.clear()
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

async function renderBrowser({
  device = "devbox",
  os = "linux",
  startPath = "",
  wrapClient,
}: {
  device?: string
  os?: string
  startPath?: string
  wrapClient?: (client: CircusClient) => CircusClient
} = {}) {
  const onSelect = vi.fn()
  const onCancel = vi.fn()
  const result = await renderWithBackend(
    <FolderBrowser
      device={device}
      os={os}
      startPath={startPath}
      layout="popover"
      onSelect={onSelect}
      onCancel={onCancel}
    />,
    { wrapClient },
  )
  await advance(10)
  return { ...result, onSelect, onCancel }
}

const input = () => screen.getByRole("combobox", { name: "Folder path" }) as HTMLInputElement
/** The names the options show. */
const nameOf = (option: Element) => option.querySelector("span")?.textContent
const options = () => screen.queryAllByRole("option").map(nameOf)
const highlighted = () => {
  const option = screen.queryAllByRole("option").find((o) => o.getAttribute("aria-selected") === "true")
  return option && nameOf(option)
}

async function type(value: string) {
  fireEvent.change(input(), { target: { value } })
  await advance(10)
}

async function press(key: string, init: KeyboardEventInit = {}) {
  const el = input()
  el.setSelectionRange(el.value.length, el.value.length)
  fireEvent.keyDown(el, { key, ...init })
  await advance(10)
}

describe("FolderBrowser", () => {
  it("starts at home without a path, showing the resolved path", async () => {
    await renderBrowser()
    expect(input().value).toBe("/home/eric/")
    expect(options()).toEqual(["..", "Documents", "Downloads", "circus", "many-folders", "notes", "private", "src"])
    // The first folder, not "..", starts highlighted.
    expect(highlighted()).toBe("Documents")
  })

  it("starts at the row's path", async () => {
    await renderBrowser({ startPath: "/home/eric/src/circus" })
    expect(input().value).toBe("/home/eric/src/circus/")
    expect(options()).toEqual(["..", "cmd", "internal", "web"])
  })

  it("falls back to home when the start path can't be listed, saying why", async () => {
    await renderBrowser({ startPath: "/home/eric/gone" })
    expect(input().value).toBe("/home/eric/")
    expect((screen.getByRole("status")).textContent).toContain(
      "Couldn't open /home/eric/gone: This folder doesn't exist. Showing the home folder instead.",
    )
    // Opening another folder clears the note.
    await press("Enter")
    expect(input().value).toBe("/home/eric/Documents/")
    expect(screen.queryByText(/Couldn't open/)).toBeNull()
  })

  it("filters by the text after the last separator, prefix matches first", async () => {
    await renderBrowser()
    await type("/home/eric/S")
    expect(options()).toEqual(["src", "Documents", "Downloads", "circus", "many-folders", "notes"])
    expect(highlighted()).toBe("src")
    await type("/home/eric/zzz")
    expect(options()).toEqual([])
    expect(screen.getByText("No folders match “zzz”.")).toBeTruthy()
  })

  it("navigates on a trailing separator and with ~", async () => {
    await renderBrowser()
    await type("/home/eric/src/")
    expect(options()).toContain("circus")
    expect(input().value).toBe("/home/eric/src/")
    await type("~/Documents/")
    expect(input().value).toBe("/home/eric/Documents/")
    expect(options()).toEqual(["..", "invoices", "taxes-2025"])
    await type("~")
    await press("Enter")
    expect(input().value).toBe("/home/eric/")
  })

  it("moves the highlight with the arrow keys and opens with Enter", async () => {
    await renderBrowser({ startPath: "/home/eric/src" })
    expect(highlighted()).toMatch(/^an-extraordinarily/)
    await press("ArrowDown")
    expect(highlighted()).toMatch(/^circus/)
    await press("ArrowUp")
    await press("ArrowUp")
    expect(highlighted()).toBe("..")
    // The highlight stops at the ends.
    await press("ArrowUp")
    expect(highlighted()).toBe("..")
    await press("Enter")
    expect(input().value).toBe("/home/eric/")
    await type("/home/eric/sr")
    await press("Enter")
    expect(input().value).toBe("/home/eric/src/")
  })

  it("goes up on Backspace with an empty filter", async () => {
    await renderBrowser({ startPath: "/home/eric/src/circus" })
    await press("Backspace")
    expect(input().value).toBe("/home/eric/src/")
    await type("/home/eric/src/c")
    // With a filter, Backspace edits the text.
    const event = fireEvent.keyDown(input(), { key: "Backspace" })
    expect(event).toBe(true)
  })

  it("opens a folder on click", async () => {
    await renderBrowser()
    fireEvent.click(screen.getByRole("option", { name: /^src/ }))
    await advance(10)
    expect(input().value).toBe("/home/eric/src/")
    expect((screen.getByRole("option", { name: /^circus/ })).textContent).toContain("Git repository")
  })

  it("marks git repositories and links, and keeps long names whole in the title", async () => {
    await renderBrowser({ startPath: "~/src" })
    expect((screen.getByRole("option", { name: /^circus/ })).textContent).toContain("Git repository")
    expect((screen.getByRole("option", { name: /^shared/ })).textContent).toContain("Symbolic link")
    const long = screen.getByRole("option", { name: /^an-extraordinarily/ })
    expect(long.getAttribute("title")).toBe("an-extraordinarily-long-folder-name-that-does-not-fit-on-one-line-of-the-picker")
  })

  it("selects the current folder with the button or Ctrl+Enter", async () => {
    const { onSelect } = await renderBrowser({ startPath: "~/src/" })
    await type("/home/eric/src/ci")
    await press("Enter", { ctrlKey: true })
    expect(onSelect).toHaveBeenLastCalledWith("/home/eric/src")
    fireEvent.click(screen.getByRole("button", { name: "Select this folder" }))
    expect(onSelect).toHaveBeenCalledTimes(2)
  })

  it("selects once a loading folder is listed", async () => {
    const { onSelect } = await renderBrowser()
    await type("/mnt/slow/")
    expect(screen.getByRole("listbox").getAttribute("aria-busy")).toBe("true")
    fireEvent.click(screen.getByRole("button", { name: "Select this folder" }))
    await advance(10)
    expect(onSelect).not.toHaveBeenCalled()
    await advance(SLOW_LISTING_MS)
    expect(onSelect).toHaveBeenCalledWith("/mnt/slow")
  })

  it("doesn't select a folder left while it loaded", async () => {
    const { onSelect } = await renderBrowser()
    await type("/mnt/slow/")
    fireEvent.click(screen.getByRole("button", { name: "Select this folder" }))
    await type("/home/eric/")
    await advance(SLOW_LISTING_MS)
    expect(onSelect).not.toHaveBeenCalled()
  })

  it("keeps the previous listing while the next loads", async () => {
    await renderBrowser()
    await type("/mnt/slow/")
    expect(options()).toContain("Documents")
    await advance(SLOW_LISTING_MS)
    expect(options()).toEqual(["..", "archive", "media"])
  })

  it.each([
    ["/home/eric/private/", "Permission denied: circus can't read this folder."],
    ["/home/eric/nope/", "This folder doesn't exist."],
    ["/home/eric/README.md/", "This is a file, not a folder."],
  ])("shows a path error for %s and stays usable", async (path, message) => {
    await renderBrowser()
    await type(path)
    expect((screen.getByRole("alert")).textContent).toContain(message)
    expect((screen.getByRole("button", { name: "Select this folder" })).hasAttribute("disabled")).toBe(true)
    expect(options()).toEqual([".."])
    await press("Enter")
    expect(screen.queryByRole("alert")).toBeNull()
  })

  it("shows the truncation note", async () => {
    await renderBrowser({ startPath: "~/many-folders" })
    expect(screen.getByText("Showing the first 1000 folders.")).toBeTruthy()
    expect(options()).toHaveLength(1001)
  })

  it("says when a folder is empty", async () => {
    await renderBrowser({ startPath: "~/Downloads" })
    expect(screen.getByText("No subfolders.")).toBeTruthy()
  })

  it.each([
    ["eric-win", undefined, "eric-win is offline."],
    ["devbox", "unavailable", "devbox didn't answer. If it's online, update circus on this device."],
    ["devbox", "resource_exhausted", "devbox is busy. Try again in a moment."],
    ["devbox", "deadline_exceeded", "devbox didn't answer in time."],
  ] as const)("shows a request failure on %s (%s) with a retry", async (device, code, message) => {
    let fail = code !== undefined
    await renderBrowser({
      device,
      wrapClient: (client) => ({
        ...client,
        machines: {
          ...client.machines,
          listDirectory: (req) => {
            if (fail) return Promise.reject(new ApiError(code ?? "unknown", "no"))
            return client.machines.listDirectory(req)
          },
        },
      }),
    })
    const alert = screen.getByRole("alert")
    expect((alert).textContent).toContain(message)
    expect((screen.getByRole("button", { name: "Select this folder" })).hasAttribute("disabled")).toBe(true)
    if (code === undefined) return
    fail = false
    fireEvent.click(within(alert).getByRole("button", { name: "Try again" }))
    await advance(10)
    expect(screen.queryByRole("alert")).toBeNull()
    expect(options()).toContain("src")
  })

  it("lists hidden folders on request and remembers the choice", async () => {
    await renderBrowser()
    expect(options()).not.toContain(".config")
    fireEvent.click(screen.getByRole("checkbox", { name: "Show hidden folders" }))
    await advance(10)
    expect(options()).toContain(".config")
    expect(localStorage.getItem(SHOW_HIDDEN_KEY)).toBe("true")
    cleanup()
    await renderBrowser()
    expect(options()).toContain(".config")
  })

  it("works without storage", async () => {
    const getItem = vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("blocked")
    })
    const setItem = vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("blocked")
    })
    try {
      await renderBrowser()
      fireEvent.click(screen.getByRole("checkbox", { name: "Show hidden folders" }))
      await advance(10)
      expect(options()).toContain(".config")
    } finally {
      getItem.mockRestore()
      setItem.mockRestore()
    }
  })

  it("cancels", async () => {
    const { onCancel } = await renderBrowser()
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }))
    expect(onCancel).toHaveBeenCalled()
  })
})
