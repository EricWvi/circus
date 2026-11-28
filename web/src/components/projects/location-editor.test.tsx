// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen } from "@testing-library/react"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type { Device } from "@/api/types"
import type { LocationRow } from "@/lib/location-form"
import { advance, renderWithBackend } from "@/test/render"

import { LocationEditor } from "./location-editor"
import { PickerFrameContext } from "./picker-frame"

beforeEach(() => {
  vi.useFakeTimers()
  localStorage.clear()
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

const devices: Device[] = [
  { deviceId: "device-devbox", name: "devbox", online: true, lastSeenUnixMs: 0, os: "linux", arch: "amd64", version: "0.1.0", updateVersion: "" },
  { deviceId: "device-eric-win", name: "eric-win", online: false, lastSeenUnixMs: 0, os: "windows", arch: "amd64", version: "0.1.0", updateVersion: "" },
]

async function renderEditor(rows: LocationRow[], { busy = false, framed = false } = {}) {
  const onChange = vi.fn()
  const onTouch = vi.fn()
  const frame = { current: framed ? document.body : null }
  await renderWithBackend(
    <PickerFrameContext.Provider value={framed ? frame : null}>
      <LocationEditor
      rows={rows}
      devices={devices}
      errors={{}}
      busy={busy}
      defaultPath="~/circus/projects/…"
      defaultPathHint="hint"
      onChange={onChange}
      onTouch={onTouch}
      />
    </PickerFrameContext.Provider>,
  )
  return { onChange, onTouch }
}

const browse = (device: string) => screen.getByRole("button", { name: `Browse folders on ${device}` })

describe("LocationEditor folder picker", () => {
  it("writes the chosen folder into the row and touches the path", async () => {
    const rows = [{ key: "a", device: "devbox", path: "/home/eric/src" }]
    const { onChange, onTouch } = await renderEditor(rows)
    fireEvent.click(browse("devbox"))
    await advance(10)
    expect(screen.getByRole("dialog", { name: "Folders on devbox" })).toBeTruthy()
    fireEvent.click(screen.getByRole("option", { name: /^circus/ }))
    await advance(10)
    fireEvent.click(screen.getByRole("button", { name: "Select this folder" }))
    await advance(10)
    expect(onChange).toHaveBeenLastCalledWith([{ key: "a", device: "devbox", path: "/home/eric/src/circus" }])
    expect(onTouch).toHaveBeenCalledWith("a", "path")
    expect(screen.queryByRole("dialog")).toBeNull()
  })

  it("closes on Escape without changes, returning keyboard focus to the button", async () => {
    const { onChange } = await renderEditor([{ key: "a", device: "devbox", path: "" }])
    const button = browse("devbox")
    act(() => button.focus())
    fireEvent.keyDown(document, { key: "Enter" })
    fireEvent.click(button)
    await advance(10)
    const input = screen.getByRole("combobox", { name: "Folder path" })
    expect(document.activeElement).toBe(input)
    fireEvent.keyDown(input, { key: "Escape" })
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()
    expect(document.activeElement).toBe(button)
    expect(onChange).not.toHaveBeenCalled()
  })

  it.each([
    [false, "start"],
    [true, "center"],
  ])("lines up with the field, or centers in a frame (framed: %s)", async (framed, align) => {
    await renderEditor(
      [
        { key: "a", device: "devbox", path: "" },
        { key: "b", device: "devbox", path: "/home/eric/src" },
      ],
      { framed },
    )
    for (const i of [0, 1]) {
      fireEvent.click(screen.getAllByRole("button", { name: "Browse folders on devbox" })[i])
      await advance(10)
      const picker = screen.getByRole("dialog", { name: "Folders on devbox" })
      expect(picker.getAttribute("data-align")).toBe(align)
      expect(picker.className).toContain(framed ? "w-[min(28rem" : "w-[max(28rem")
      fireEvent.keyDown(screen.getByRole("combobox", { name: "Folder path" }), { key: "Escape" })
      await advance(10)
    }
  })

  it("explains why browsing is unavailable", async () => {
    await renderEditor([
      { key: "a", device: "eric-win", path: "" },
      { key: "b", device: "", path: "" },
    ])
    expect(browse("eric-win").hasAttribute("disabled")).toBe(true)
    expect(screen.getByLabelText("eric-win is offline")).toBeTruthy()
    expect(screen.getByLabelText("Choose a device first")).toBeTruthy()
  })

  it("doesn't open while busy", async () => {
    await renderEditor([{ key: "a", device: "devbox", path: "" }], { busy: true })
    expect(browse("devbox").getAttribute("aria-disabled")).toBe("true")
    fireEvent.click(browse("devbox"))
    await advance(10)
    expect(screen.queryByRole("dialog")).toBeNull()
  })
})
