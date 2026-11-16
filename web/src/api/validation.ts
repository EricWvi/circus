// Input rules shared by the mock server and the forms. The server is the
// authority; the UI runs the same checks first for instant feedback.

import type { Location } from "./types"

export const EMAIL_MAX = 254
/** Bytes of UTF-8. */
export const PASSWORD_MIN = 8
export const PASSWORD_MAX = 72
export const PROJECT_NAME_MAX = 64

// oxlint-disable-next-line no-control-regex
const controlChars = /[\u0000-\u001f\u007f]/
// C0, DEL and C1 controls, as the server rejects in emails.
// oxlint-disable-next-line no-control-regex
const emailControlChars = /[\u0000-\u001f\u007f-\u009f]/

/** The form emails are stored and compared in. */
export function normalizeEmail(email: string): string {
  return email.trim().toLowerCase()
}

const utf8Length = (text: string) => new TextEncoder().encode(text).length

/**
 * Checks an email after normalizing. Deliberately loose, like the server: one
 * "@" between non-empty parts, no whitespace or control characters, at most
 * 254 bytes of UTF-8.
 */
export function validateEmail(email: string): string | null {
  const normalized = normalizeEmail(email)
  if (normalized === "") return "Enter an email address."
  const parts = normalized.split("@")
  if (
    utf8Length(normalized) > EMAIL_MAX ||
    parts.length !== 2 ||
    parts.some((part) => part === "") ||
    /\s/.test(normalized) ||
    emailControlChars.test(normalized)
  ) {
    return "Enter a valid email address."
  }
  return null
}

export function validatePassword(password: string): string | null {
  const bytes = utf8Length(password)
  if (bytes < PASSWORD_MIN) return `Password must be at least ${PASSWORD_MIN} characters.`
  if (bytes > PASSWORD_MAX) return `Password is too long (at most ${PASSWORD_MAX} bytes).`
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
