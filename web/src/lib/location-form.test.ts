import { describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"
import type { Device } from "@/api/types"

import {
  defaultDevice,
  formChanged,
  sameProjectFields,
  hasErrors,
  mapServerError,
  pickLocation,
  projectErrorMessage,
  sortDevices,
  toLocations,
  validateForm,
  validateRows,
  type LocationRow,
} from "./location-form"

const device = (name: string, os: string, online = true): Device => ({
  name,
  os,
  online,
  arch: "amd64",
  version: "0.1.0",
  lastSeenUnixMs: 0,
})
const devices = [device("win", "windows", false), device("mac", "darwin"), device("box", "linux")]
const row = (key: string, d: string, path: string): LocationRow => ({ key, device: d, path })

describe("devices", () => {
  it("lists online devices first, then by name", () => {
    expect(sortDevices(devices).map((d) => d.name)).toEqual(["box", "mac", "win"])
  })

  it("defaults to the first online device", () => {
    expect(defaultDevice(devices)).toBe("box")
    expect(defaultDevice([device("off", "linux", false)])).toBe("off")
    expect(defaultDevice([])).toBe("")
  })
})

describe("validateRows", () => {
  it("accepts absolute paths for each device's OS", () => {
    expect(validateRows([row("a", "box", "/src"), row("b", "win", "D:\\src")], devices)).toEqual({})
  })

  it("requires a device and a path", () => {
    expect(validateRows([row("a", "", "")], devices)).toEqual({
      a: { device: "Choose a device.", path: "Enter a path." },
    })
    expect(validateRows([row("a", "gone", "/x")], devices)).toEqual({ a: { device: "Unknown device." } })
  })

  it("checks paths against the device's OS", () => {
    expect(validateRows([row("a", "box", "src")], devices).a.path).toMatch(/absolute path, like \/home/)
    expect(validateRows([row("a", "win", "/src")], devices).a.path).toMatch(/C:\\src\\app/)
  })

  it("flags the later of two equal locations, after normalizing", () => {
    expect(validateRows([row("a", "win", "D:\\src"), row("b", "win", "d:/src/")], devices)).toEqual({
      b: { path: "This location is already listed." },
    })
    // The same path on another device is fine.
    expect(validateRows([row("a", "box", "/src"), row("b", "mac", "/src")], devices)).toEqual({})
  })
})

describe("validateForm", () => {
  it("checks the name, also against other projects regardless of case", () => {
    const rows = [row("a", "box", "/src")]
    expect(validateForm({ name: " ", rows }, devices, []).name).toBe("Enter a project name.")
    expect(validateForm({ name: "x".repeat(65), rows }, devices, []).name).toMatch(/at most 64/)
    expect(validateForm({ name: "Circus", rows }, devices, [{ name: "circus" }]).name).toMatch(/already exists/)
    expect(hasErrors(validateForm({ name: "web", rows }, devices, [{ name: "circus" }]))).toBe(false)
  })
})

describe("mapServerError", () => {
  const form = { name: " web ", rows: [row("a", "box", "/a"), row("b", "box", "/b")] }

  it("maps field paths to the name or a row's field", () => {
    expect(mapServerError(new ApiError("already_exists", "taken", "name"), form)).toEqual({
      rows: {},
      name: 'A project named "web" already exists.',
    })
    expect(mapServerError(new ApiError("invalid_argument", "bad", "locations[1].path"), form)).toEqual({
      rows: { b: { path: "This path can't be used on its device." } },
    })
    expect(mapServerError(new ApiError("invalid_argument", "gone", "locations[0].device"), form)).toEqual({
      rows: { a: { device: "Unknown device." } },
    })
  })

  it("never shows the server's text", () => {
    const { name } = mapServerError(new ApiError("invalid_argument", "server text", "name"), {
      ...form,
      name: " ",
    })
    expect(name).toBe("Enter a project name.")
  })

  it("says a value can't be used when the client didn't predict the refusal", () => {
    expect(mapServerError(new ApiError("invalid_argument", "x", "name"), form).name).toBe(
      "This name can't be used.",
    )
    expect(mapServerError(new ApiError("invalid_argument", "x", "locations[0].path"), form).rows.a?.path).toBe(
      "This path can't be used on its device.",
    )
  })

  it("says when the project is gone", () => {
    expect(mapServerError(new ApiError("not_found", "x"), form)).toEqual({
      rows: {},
      form: "This project no longer exists.",
    })
    expect(projectErrorMessage(new ApiError("not_found", "x"))).toBe("This project no longer exists.")
    expect(projectErrorMessage(new ApiError("unavailable", "x"))).toMatch(/reach the server/)
    expect(projectErrorMessage(new Error("x"))).toMatch(/went wrong/)
  })

  it("falls back to one line for the form", () => {
    expect(mapServerError(new ApiError("invalid_argument", "none", "locations"), form)).toEqual({
      rows: {},
      form: "Add at least one location.",
    })
    expect(mapServerError(new ApiError("invalid_argument", "x", "locations[7].path"), form).form).toMatch(/went wrong/)
    expect(mapServerError(new ApiError("unavailable", "down"), form).form).toMatch(/reach the server/)
    expect(mapServerError(new Error("boom"), form).form).toMatch(/went wrong/)
  })
})

describe("formChanged and toLocations", () => {
  const saved = { name: "web", locations: [{ device: "box", path: "/a" }] }

  it("ignores surrounding whitespace and what the server normalizes", () => {
    expect(formChanged(saved, { name: " web ", rows: [row("k", "box", "/a ")] })).toBe(false)
    expect(formChanged(saved, { name: "web", rows: [row("k", "box", "/a/")] }, devices)).toBe(false)
    const win = { name: "w", locations: [{ device: "win", path: "C:\\src" }] }
    expect(formChanged(win, { name: "w", rows: [row("k", "win", "c:/src/")] }, devices)).toBe(false)
    expect(toLocations([row("k", "box", " /a ")])).toEqual([{ device: "box", path: "/a" }])
  })

  it("notices a renamed project or changed locations", () => {
    expect(formChanged(saved, { name: "web2", rows: [row("k", "box", "/a")] })).toBe(true)
    expect(formChanged(saved, { name: "web", rows: [row("k", "mac", "/a")] })).toBe(true)
    expect(formChanged(saved, { name: "web", rows: [row("k", "box", "/a"), row("l", "box", "/b")] })).toBe(true)
  })
})

describe("sameProjectFields", () => {
  it("compares name and locations in order", () => {
    const a = { name: "p", locations: [{ device: "d", path: "/a" }, { device: "d", path: "/b" }] }
    expect(sameProjectFields(a, structuredClone(a))).toBe(true)
    expect(sameProjectFields(a, { ...a, name: "q" })).toBe(false)
    expect(sameProjectFields(a, { ...a, locations: [...a.locations].reverse() })).toBe(false)
  })
})

describe("pickLocation", () => {
  const a = { device: "box", path: "/a" }
  const b = { device: "box", path: "/b" }
  const c = { device: "mac", path: "/c" }

  it("follows the chosen location when it moves", () => {
    expect(pickLocation({ locations: [a, b, c], lastUsedLocation: 0 }, b)).toBe(1)
    expect(pickLocation({ locations: [c, b], lastUsedLocation: 0 }, b)).toBe(1)
  })

  it("falls back to the last used location, clamped", () => {
    expect(pickLocation({ locations: [a, b, c], lastUsedLocation: 2 }, null)).toBe(2)
    expect(pickLocation({ locations: [a], lastUsedLocation: 2 }, c)).toBe(0)
    expect(pickLocation({ locations: [a, b], lastUsedLocation: 5 }, null)).toBe(1)
  })
})
