// Input rules shared by the mock server and the forms. The server is the
// authority; the UI runs the same checks first for instant feedback.

import type { Location } from "./types"

export const USERNAME_MAX = 32
export const PASSWORD_MIN = 8
export const PASSWORD_MAX = 128
export const PROJECT_NAME_MAX = 64

const usernamePattern = /^[A-Za-z0-9][A-Za-z0-9._-]*$/
// oxlint-disable-next-line no-control-regex
const controlChars = /[\u0000-\u001f\u007f]/

export function validateUsername(username: string): string | null {
  if (username === "") return "Enter a username."
  if (username.length > USERNAME_MAX || !usernamePattern.test(username)) {
    return `Usernames use letters, digits, ".", "_" and "-", up to ${USERNAME_MAX} characters.`
  }
  return null
}

export function validatePassword(password: string): string | null {
  if (password.length < PASSWORD_MIN) {
    return `Password must be at least ${PASSWORD_MIN} characters.`
  }
  if (password.length > PASSWORD_MAX) {
    return `Password must be at most ${PASSWORD_MAX} characters.`
  }
  return null
}

/** Checks a project name after trimming. */
export function validateProjectName(name: string): string | null {
  const trimmed = name.trim()
  if (trimmed === "") return "Enter a project name."
  if ([...trimmed].length > PROJECT_NAME_MAX) {
    return `Project names are at most ${PROJECT_NAME_MAX} characters.`
  }
  if (controlChars.test(trimmed)) return "Project names can't contain control characters."
  return null
}

/** Project names compare case-insensitively for uniqueness. */
export function sameProjectName(a: string, b: string): boolean {
  return a.trim().toLowerCase() === b.trim().toLowerCase()
}

const isWindows = (os: string) => os.toLowerCase() === "windows"

/** Whether `path` is absolute on a device running `os` (a GOOS value). */
export function isAbsolutePath(path: string, os: string): boolean {
  if (isWindows(os)) return /^[A-Za-z]:[\\/]/.test(path) || /^\\\\[^\\]+\\[^\\]+/.test(path)
  return path.startsWith("/")
}

/**
 * Trims whitespace and trailing separators, keeping a bare root intact. On
 * Windows, separators become backslashes and the drive letter upper case, so
 * "d:/work" and "D:\work" are the same location.
 */
export function normalizePath(path: string, os: string): string {
  const trimmed = path.trim()
  if (!isWindows(os)) {
    const stripped = trimmed.replace(/\/+$/, "")
    return stripped === "" && trimmed !== "" ? "/" : stripped
  }
  const windows = trimmed
    .replaceAll("/", "\\")
    .replace(/^([A-Za-z]):/, (_, drive: string) => `${drive.toUpperCase()}:`)
  const stripped = windows.replace(/\\+$/, "")
  if (/^[A-Z]:$/.test(stripped)) return `${stripped}\\`
  return stripped
}

export function sameLocation(a: Location, b: Location): boolean {
  return a.device === b.device && a.path === b.path
}
