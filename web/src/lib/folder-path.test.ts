import { describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"
import type { DirectoryEntry } from "@/api/types"

import { directoryKey, filterEntries, joinPath, requestErrorMessage, splitInput, withSeparator } from "./folder-path"

const entries = (...names: string[]): DirectoryEntry[] =>
  names.map((name) => ({ name, gitRepository: false, symlink: false }))

describe("splitInput", () => {
  it.each([
    ["/home/eric/src", "linux", "/home/eric", "src"],
    ["/home/eric/", "linux", "/home/eric", ""],
    ["/home/eric//", "linux", "/home/eric", ""],
    ["/", "linux", "/", ""],
    ["/sr", "linux", "/", "sr"],
    ["~/", "linux", "~", ""],
    ["~/Doc", "linux", "~", "Doc"],
    ["C:\\Users\\e", "windows", "C:\\Users", "e"],
    ["C:\\", "windows", "C:\\", ""],
    ["C:/Users/", "windows", "C:/Users", ""],
    ["a\\b", "linux", null, "a\\b"],
    ["src", "linux", null, "src"],
    ["", "linux", null, ""],
    ["~", "linux", null, "~"],
  ])("splits %j on %s", (input, os, dir, filter) => {
    expect(splitInput(input, os)).toEqual({ dir, filter })
  })
})

describe("directoryKey", () => {
  it("drops trailing separators except on a root", () => {
    expect(directoryKey("/home/", "linux")).toBe("/home")
    expect(directoryKey("//", "linux")).toBe("/")
    expect(directoryKey("~/", "darwin")).toBe("~")
    expect(directoryKey("C:/", "windows")).toBe("C:\\")
    expect(directoryKey("D:\\src\\", "windows")).toBe("D:\\src")
  })
})

describe("joinPath and withSeparator", () => {
  it("uses the path's own separator", () => {
    expect(withSeparator("/home/eric")).toBe("/home/eric/")
    expect(withSeparator("/")).toBe("/")
    expect(withSeparator("C:\\Users")).toBe("C:\\Users\\")
    expect(withSeparator("C:\\")).toBe("C:\\")
    expect(joinPath("/", "etc")).toBe("/etc")
    expect(joinPath("/home", "eric")).toBe("/home/eric")
    expect(joinPath("C:\\Users", "eric")).toBe("C:\\Users\\eric")
    expect(joinPath("\\\\server\\share", "x")).toBe("\\\\server\\share\\x")
  })
})

describe("filterEntries", () => {
  const list = entries("Documents", "Downloads", "circus", "many-folders", "notes", "src")
  const names = (filter: string) => filterEntries(list, filter).map((e) => e.name)

  it("keeps everything without a filter", () => {
    expect(names("")).toEqual(["Documents", "Downloads", "circus", "many-folders", "notes", "src"])
  })

  it("puts prefix matches first, ignoring case, then substring matches", () => {
    expect(names("d")).toEqual(["Documents", "Downloads", "many-folders"])
    expect(names("S")).toEqual(["src", "Documents", "Downloads", "circus", "many-folders", "notes"])
    expect(names("CIR")).toEqual(["circus"])
    expect(names("xyz")).toEqual([])
  })
})

describe("requestErrorMessage", () => {
  it.each([
    ["failed_precondition", "box is offline."],
    ["unavailable", "box didn't answer. If it's online, update circus on this device."],
    ["resource_exhausted", "box is busy. Try again in a moment."],
    ["deadline_exceeded", "box didn't answer in time."],
    ["not_found", "box is no longer one of your devices."],
    ["internal", "Something went wrong. Try again."],
  ] as const)("explains %s", (code, message) => {
    expect(requestErrorMessage(new ApiError(code, "x"), "box")).toBe(message)
  })
})
