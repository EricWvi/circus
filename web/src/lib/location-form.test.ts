import { describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"
import type { Device, ProjectLocation } from "@/api/types"

import {
  addedRow,
  defaultDevice,
  derivedProjectName,
  hasEarlierDefault,
  isReplaced,
  formChanged,
  hasErrors,
  locationsChanged,
  mapServerError,
  pathBaseName,
  pickLocation,
  projectErrorMessage,
  repoNameOf,
  rowsOf,
  shortDefaultPath,
  sortDevices,
  toLocations,
  validateForm,
  validateRows,
  type LocationRow,
} from "./location-form"

const device = (name: string, os: string, online = true): Device => ({
  deviceId: `device-${name}`,
  name,
  os,
  online,
  arch: "amd64",
  version: "0.1.0",
  lastSeenUnixMs: 0,
})
const devices = [device("win", "windows", false), device("mac", "darwin"), device("box", "linux")]
const row = (key: string, d: string, path: string): LocationRow => ({ key, device: d, path })
const form = (name: string, rows: LocationRow[], repoUrl = "") => ({ name, repoUrl, rows })
const loc = (d: string, path: string, deviceRevoked = false): ProjectLocation => ({
  device: d,
  path,
  effectivePath: path === "" ? "~/circus/projects/p1" : "",
  deviceRevoked,
})

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
  it("accepts absolute paths for each device's OS, and empty paths", () => {
    expect(validateRows([row("a", "box", "/src"), row("b", "win", "D:\\src"), row("c", "mac", "")], devices)).toEqual({})
  })

  it("requires a device; one that isn't listed was removed", () => {
    expect(validateRows([row("a", "", "")], devices)).toEqual({ a: { device: "Choose a device." } })
    expect(validateRows([row("a", "gone", "/x")], devices)).toEqual({
      a: { device: "This device was removed. Choose another device or remove the location." },
    })
  })

  it("checks paths against the device's OS", () => {
    expect(validateRows([row("a", "box", "src")], devices).a.path).toMatch(/absolute path, like \/home/)
    expect(validateRows([row("a", "win", "/src")], devices).a.path).toMatch(/C:\\src\\app/)
    expect(validateRows([row("a", "win", "\\\\?\\C:\\x")], devices).a.path).toMatch(/aren't supported/)
    expect(validateRows([row("a", "box", "/a\u200bb")], devices).a.path).toMatch(/invisible/)
  })

  it("flags the later of two equal locations, after normalizing", () => {
    expect(validateRows([row("a", "win", "D:\\src"), row("b", "win", "d:/src/")], devices)).toEqual({
      b: { path: "This location is already listed." },
    })
    expect(validateRows([row("a", "box", "/a//./b"), row("b", "box", "/a/b/")], devices)).toEqual({
      b: { path: "This location is already listed." },
    })
    // The same path on another device is fine.
    expect(validateRows([row("a", "box", "/src"), row("b", "mac", "/src")], devices)).toEqual({})
  })

  it("allows one default location per device", () => {
    expect(validateRows([row("a", "box", ""), row("b", "box", "  ")], devices)).toEqual({
      b: { path: "This device already has a default location." },
    })
    expect(validateRows([row("a", "box", ""), row("b", "mac", ""), row("c", "box", "/x")], devices)).toEqual({})
  })
})

describe("validateForm", () => {
  const rows = [row("a", "box", "/src")]

  it("checks the name, also against other projects regardless of case", () => {
    expect(validateForm(form(" ", rows), devices, []).name).toBe("Enter a project name.")
    expect(validateForm(form("x".repeat(65), rows), devices, []).name).toMatch(/at most 64/)
    expect(validateForm(form("Circus", rows), devices, [{ name: "circus" }]).name).toMatch(/already exists/)
    expect(hasErrors(validateForm(form("web", rows), devices, [{ name: "circus" }]))).toBe(false)
    expect(hasErrors(validateForm(form("web", []), devices, []))).toBe(false)
  })

  it("checks the repository URL", () => {
    expect(validateForm(form("w", rows, "github.com/o/r"), devices, []).repoUrl).toMatch(/accepted forms/)
    expect(validateForm(form("w", rows, "https://h/o r"), devices, []).repoUrl).toMatch(/spaces/)
    expect(validateForm(form("w", rows, " git@github.com:o/r.git "), devices, []).repoUrl).toBeUndefined()
  })

  it("leaves saved locations alone until they change", () => {
    const saved = [loc("gone", "/x")]
    const unchanged = form("w", [row("k", "gone", "/x")])
    expect(validateForm(unchanged, devices, [], saved).rows).toEqual({})
    const edited = form("w", [row("k", "gone", "/y")])
    expect(validateForm(edited, devices, [], saved).rows.k.device).toMatch(/removed/)
  })
})

describe("mapServerError", () => {
  const sent = form(" web ", [row("a", "box", "/a"), row("b", "box", "/b")], "git@h:x")

  it("maps field paths to the name, the repository URL or a row's field", () => {
    expect(mapServerError(new ApiError("already_exists", "taken", "name"), sent)).toEqual({
      rows: {},
      name: 'A project named "web" already exists.',
    })
    expect(mapServerError(new ApiError("invalid_argument", "bad", "locations[1].path"), sent, devices)).toEqual({
      rows: { b: { path: "This path can't be used on its device." } },
    })
    expect(mapServerError(new ApiError("invalid_argument", "gone", "locations[0].device"), sent, devices)).toEqual({
      rows: { a: { device: "Unknown device." } },
    })
    expect(mapServerError(new ApiError("invalid_argument", "bad", "repo_url"), sent)).toEqual({
      rows: {},
      repoUrl: "This repository URL can't be used.",
    })
  })

  it("gives a field the client's own message when it explains the refusal", () => {
    const twice = form("w", [row("a", "box", ""), row("b", "box", "")])
    expect(mapServerError(new ApiError("invalid_argument", "dup", "locations[1].path"), twice, devices)).toEqual({
      rows: { b: { path: "This device already has a default location." } },
    })
    const removed = form("w", [row("a", "gone", "/a")])
    expect(mapServerError(new ApiError("invalid_argument", "x", "locations[0].device"), removed, devices).rows.a?.device).toMatch(
      /removed/,
    )
    expect(mapServerError(new ApiError("invalid_argument", "x", "repo_url"), form("w", [], "nope")).repoUrl).toMatch(
      /accepted forms/,
    )
    expect(mapServerError(new ApiError("invalid_argument", "server text", "name"), form(" ", [])).name).toBe(
      "Enter a project name.",
    )
    expect(mapServerError(new ApiError("invalid_argument", "x", "name"), sent).name).toBe("This name can't be used.")
  })

  it("says when the project is gone, changed or at its limits", () => {
    expect(mapServerError(new ApiError("not_found", "x"), sent)).toEqual({
      rows: {},
      form: "This project no longer exists.",
    })
    expect(projectErrorMessage(new ApiError("resource_exhausted", "x"))).toMatch(/limit on projects/)
    expect(projectErrorMessage(new ApiError("aborted", "x"))).toBe("This project was changed elsewhere.")
    expect(projectErrorMessage(new ApiError("unavailable", "x"))).toMatch(/reach the server/)
    expect(projectErrorMessage(new Error("x"))).toMatch(/went wrong/)
  })

  it("falls back to one line for the form", () => {
    expect(mapServerError(new ApiError("invalid_argument", "many", "locations"), sent)).toEqual({
      rows: {},
      form: "A project has at most 16 locations.",
    })
    expect(mapServerError(new ApiError("invalid_argument", "x", "last_used_location"), sent).form).toBe(
      "Choose one of the project's locations.",
    )
    expect(mapServerError(new ApiError("invalid_argument", "x", "locations[7].path"), sent).form).toMatch(/went wrong/)
    expect(mapServerError(new ApiError("unavailable", "down"), sent).form).toMatch(/reach the server/)
    expect(mapServerError(new Error("boom"), sent).form).toMatch(/went wrong/)
  })
})

describe("formChanged, locationsChanged and toLocations", () => {
  const saved = { name: "web", repoUrl: "", locations: [loc("box", "/a"), loc("mac", "")] }

  it("ignores surrounding whitespace and what the server normalizes", () => {
    expect(formChanged(saved, form(" web ", [row("k", "box", "/a "), row("l", "mac", " ")]))).toBe(false)
    expect(formChanged(saved, form("web", [row("k", "box", "/a//"), row("l", "mac", "")]), devices)).toBe(false)
    const win = { name: "w", repoUrl: "", locations: [loc("win", "C:\\src")] }
    expect(formChanged(win, form("w", [row("k", "win", "c:/src/")]), devices)).toBe(false)
    expect(toLocations([row("k", "box", " /a "), row("l", "mac", "  ")])).toEqual([
      { device: "box", path: "/a" },
      { device: "mac", path: "" },
    ])
  })

  it("notices a renamed project, a new repository URL or changed locations", () => {
    const rows = [row("k", "box", "/a"), row("l", "mac", "")]
    expect(formChanged(saved, form("web2", rows))).toBe(true)
    expect(formChanged(saved, form("web", rows, "git@h:x"))).toBe(true)
    expect(locationsChanged(saved.locations, rows)).toBe(false)
    expect(locationsChanged(saved.locations, [rows[0]])).toBe(true)
    expect(locationsChanged(saved.locations, [rows[0], row("l", "mac", "/b")])).toBe(true)
    expect(locationsChanged(saved.locations, [row("k", "mac", "/a"), rows[1]])).toBe(true)
  })

  it("starts rows from the saved locations", () => {
    expect(rowsOf({ locations: saved.locations }).map((r) => ({ device: r.device, path: r.path }))).toEqual([
      { device: "box", path: "/a" },
      { device: "mac", path: "" },
    ])
  })
})

describe("pickLocation", () => {
  const a = loc("box", "/a")
  const b = loc("box", "")
  const c = loc("mac", "/c")
  const gone = loc("old", "/x", true)

  it("follows the chosen location when it moves", () => {
    expect(pickLocation({ locations: [a, b, c], lastUsedLocation: 0 }, b)).toBe(1)
    expect(pickLocation({ locations: [c, b], lastUsedLocation: 0 }, b)).toBe(1)
  })

  it("falls back to the last used location, clamped", () => {
    expect(pickLocation({ locations: [a, b, c], lastUsedLocation: 2 }, null)).toBe(2)
    expect(pickLocation({ locations: [a], lastUsedLocation: 2 }, c)).toBe(0)
    expect(pickLocation({ locations: [a, b], lastUsedLocation: 5 }, null)).toBe(1)
  })

  it("skips locations whose device was removed, and has none to offer without one", () => {
    expect(pickLocation({ locations: [gone, a], lastUsedLocation: 0 }, null)).toBe(1)
    expect(pickLocation({ locations: [a, gone], lastUsedLocation: 0 }, gone)).toBe(0)
    expect(pickLocation({ locations: [gone], lastUsedLocation: 0 }, null)).toBe(-1)
    expect(pickLocation({ locations: [], lastUsedLocation: 0 }, null)).toBe(-1)
  })
})

describe("a revoked device whose name is enrolled again", () => {
  const saved = [{ device: "box", path: "/a", deviceRevoked: true }]
  const rows = rowsOf({ locations: saved.map((l) => ({ ...l, effectivePath: "" })) })

  it("counts as a change while a device has the name, so saving binds it", () => {
    expect(rows[0].revoked).toBe(true)
    expect(isReplaced(rows[0], devices)).toBe(true)
    expect(locationsChanged(saved, rows, devices)).toBe(true)
    // Without the name listed, the location is just removed; nothing to save.
    const others = devices.filter((d) => d.name !== "box")
    expect(isReplaced(rows[0], others)).toBe(false)
    expect(locationsChanged(saved, rows, others)).toBe(false)
  })
})

describe("addedRow", () => {
  it("prefers a device without a default location in the form", () => {
    expect(addedRow([], devices)).toMatchObject({ row: { device: "box", path: "" }, needsPath: false })
    expect(addedRow([row("a", "box", "")], devices)).toMatchObject({ row: { device: "mac" }, needsPath: false })
    expect(addedRow([row("a", "box", "/x")], devices)).toMatchObject({ row: { device: "box" }, needsPath: false })
  })

  it("asks for a path on the first device once every device has a default", () => {
    const all = devices.map((d, i) => row(String(i), d.name, ""))
    expect(addedRow(all, devices)).toMatchObject({ row: { device: "box", path: "" }, needsPath: true })
    expect(hasEarlierDefault([...all, row("x", "box", "")], all.length)).toBe(true)
    expect(hasEarlierDefault(all, 0)).toBe(false)
  })
})

describe("shortDefaultPath", () => {
  it("keeps both ends of a long project id", () => {
    const id = "01a1e8e1-69db-70b7-b9ad-c2be3d618a7e"
    expect(shortDefaultPath(`~/circus/projects/${id}`)).toBe("~/circus/projects/01a1e8…d618a7e")
    expect(shortDefaultPath(`~/circus/projects/${id}`, { head: 4, tail: 4 })).toBe("~/circus/projects/01a1…8a7e")
    expect(shortDefaultPath("~/circus/projects/circus")).toBe("~/circus/projects/circus")
    expect(shortDefaultPath("~/circus/projects/…")).toBe("~/circus/projects/…")
  })
})

describe("derived project names", () => {
  it.each([
    ["/home/eric/src/circus", "circus"],
    ["/home/eric/src/circus/", "circus"],
    ["  ~/src/dotfiles  ", "dotfiles"],
    ["C:\\Users\\eric\\src\\toolbox\\", "toolbox"],
    ["/", ""],
    ["~", ""],
    ["C:\\", ""],
    ["/srv/..", ""],
  ])("takes the last folder of %j", (path, name) => {
    expect(pathBaseName(path)).toBe(name)
  })

  it.each([
    ["https://github.com/owner/repo.git", "repo"],
    ["https://github.com/owner/repo", "repo"],
    ["https://github.com/owner/repo/", "repo"],
    ["ssh://git@host:22/owner/repo.git", "repo"],
    ["git@github.com:owner/repo.git", "repo"],
    ["git@host:repo", "repo"],
    ["git://host/owner/my%20repo.git", "my repo"],
    ["file:///srv/git/repo.git", "repo"],
    ["https://github.com", ""],
    ["not a url", ""],
    ["", ""],
  ])("takes the repository name of %j", (url, name) => {
    expect(repoNameOf(url)).toBe(name)
  })

  const rows = (...paths: string[]) => paths.map((path, i) => ({ key: String(i), device: "devbox", path }))

  it("prefers the first location with a path, then the repository URL", () => {
    const repoUrl = "git@github.com:owner/repo.git"
    expect(derivedProjectName({ rows: rows("", "/srv/palace", "/srv/other"), repoUrl }, [])).toBe("palace")
    expect(derivedProjectName({ rows: rows(""), repoUrl }, [])).toBe("repo")
    expect(derivedProjectName({ rows: [], repoUrl }, [])).toBe("repo")
    expect(derivedProjectName({ rows: rows(""), repoUrl: "" }, [])).toBeUndefined()
  })

  it("derives nothing that isn't a valid, unused name", () => {
    expect(derivedProjectName({ rows: rows("/srv/" + "x".repeat(65)), repoUrl: "" }, [])).toBeUndefined()
    expect(derivedProjectName({ rows: rows("/srv/Palace"), repoUrl: "" }, [{ name: "palace" }])).toBeUndefined()
    // A path's own name counts even when it gives none: no fallback to the URL.
    expect(derivedProjectName({ rows: rows("/"), repoUrl: "https://h/o/repo.git" }, [])).toBeUndefined()
  })
})
