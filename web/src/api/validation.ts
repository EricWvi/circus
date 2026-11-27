// Input rules shared by the mock server and the forms. The server is the
// authority; the UI runs the same checks first for instant feedback.

import { i18n } from "@/i18n/i18n"

import caseFold from "./case-fold.json"
import { scpLike, urlLike } from "./git-url"
import type { Location } from "./types"

export const EMAIL_MAX = 254
/** Bytes of UTF-8. */
export const PASSWORD_MIN = 8
export const PASSWORD_MAX = 72
export const PROJECT_NAME_MAX = 64

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
  if (normalized === "") return i18n.t("validation.emailRequired")
  const parts = normalized.split("@")
  if (
    utf8Length(normalized) > EMAIL_MAX ||
    parts.length !== 2 ||
    parts.some((part) => part === "") ||
    /\s/.test(normalized) ||
    emailControlChars.test(normalized)
  ) {
    return i18n.t("validation.emailInvalid")
  }
  return null
}

export function validatePassword(password: string): string | null {
  const bytes = utf8Length(password)
  if (bytes < PASSWORD_MIN) return i18n.t("validation.passwordTooShort", { min: PASSWORD_MIN })
  if (bytes > PASSWORD_MAX) return i18n.t("validation.passwordTooLong", { max: PASSWORD_MAX })
  return null
}

// Project names, paths and repository URLs follow the control plane's
// rules (internal/controlplane/webapi/rules.go), and device and worker names
// its enrollment's (auth.ValidateName); validation-parity.test.ts runs the
// Go tests' cases against these functions.

/** UTF-8 bytes. */
export const PATH_MAX_BYTES = 4096
export const REPO_URL_MAX_BYTES = 2048
export const MAX_LOCATIONS = 16
/** Where a location without a path resolves on its device, followed by the project id. */
export const DEFAULT_PATH_PREFIX = "~/circus/projects/"

/** A checked value: normalized as the server stores it, or why it is refused. */
export type Checked = { ok: true; value: string } | { ok: false; problem: string }

const accept = (value: string): Checked => ({ ok: true, value })
const refuse = (problem: string): Checked => ({ ok: false, problem })

/**
 * Characters no name, path or repository URL may hold (after trimming),
 * because they are invisible or make text display differently from what it
 * is: C0 controls, DEL and C1 controls; zero-width characters (U+200B–U+200D,
 * U+FEFF); the bidirectional marks LRM, RLM and ALM; bidirectional
 * embeddings, overrides and isolates (U+202A–U+202E, U+2066–U+2069); and the
 * line and paragraph separators.
 */
// oxlint-disable-next-line no-control-regex
const FORBIDDEN = /[\u0000-\u001f\u007f-\u009f\u200b-\u200f\ufeff\u061c\u202a-\u202e\u2066-\u2069\u2028\u2029]/

/** Whitespace as Go's unicode.IsSpace sees it (JavaScript's \s, plus NEL). */
const SPACE = /[\s\u0085]/

/** Bytes, which are characters here: names are ASCII (`auth.MaxNameLen`). */
export const IDENTITY_NAME_MAX = 63

const IDENTITY_NAME = /^[A-Za-z0-9][A-Za-z0-9._-]*$/

/**
 * Checks a device or worker name as the server does (`auth.ValidateName`):
 * 1 to IDENTITY_NAME_MAX of A–Z, a–z, 0–9, ".", "_" and "-", starting with
 * a letter or digit. Nothing is trimmed: the name goes into the join
 * command as typed, so leading or trailing spaces get a message of their
 * own. An empty name is refused too (the add device dialog takes it to
 * mean the host name and does not check it).
 */
export function checkIdentityName(raw: string): Checked {
  if (raw.length > IDENTITY_NAME_MAX) return refuse(i18n.t("validation.identityNameTooLong", { max: IDENTITY_NAME_MAX }))
  // Spaces pasted along with a host name are easy to miss: say so.
  if (raw !== "" && raw.trim() !== raw) return refuse(i18n.t("validation.identityNameSpaces"))
  if (!IDENTITY_NAME.test(raw)) return refuse(i18n.t("validation.identityNameChars"))
  return accept(raw)
}

/**
 * Device and worker names compare ignoring ASCII case, as the server's do
 * (`store.IdentityNameKey`): "Box" and "box" name the same device.
 */
export const sameIdentityName = (a: string, b: string) => identityNameKey(a) === identityNameKey(b)

const identityNameKey = (name: string) => name.replace(/[A-Z]/g, (c) => c.toLowerCase())

/** Checks a project name; the value is the trimmed name. */
export function checkProjectName(raw: string): Checked {
  const name = raw.trim()
  if (name === "") return refuse(i18n.t("validation.projectNameRequired"))
  if ([...name].length > PROJECT_NAME_MAX) {
    return refuse(i18n.t("validation.projectNameTooLong", { max: PROJECT_NAME_MAX }))
  }
  if (FORBIDDEN.test(name)) return refuse(i18n.t("validation.projectNameControlChars"))
  return accept(name)
}

/** Why a project name is refused, or null. */
export function validateProjectName(name: string): string | null {
  const checked = checkProjectName(name)
  return checked.ok ? null : checked.problem
}

/**
 * Each code point that has other cases, mapped to the first code point of
 * its simple case-folding orbit. The orbits come from Go's unicode tables
 * (case-fold.json, generated by the control plane's tests), so this folds
 * exactly as the server does: "ı" and "I" stay different, "K" and the
 * Kelvin sign are the same.
 */
const foldKey = new Map<string, string>(
  caseFold.orbits.flatMap((orbit) => {
    const runes = [...orbit]
    return runes.map((r): [string, string] => [r, runes[0]])
  }),
)

/**
 * Project names compare ignoring case for uniqueness, after trimming: code
 * point by code point, equal or in the same case-folding orbit, like Go's
 * strings.EqualFold.
 */
export function sameProjectName(a: string, b: string): boolean {
  const x = [...a.trim()]
  const y = [...b.trim()]
  return x.length === y.length && x.every((c, i) => c === y[i] || (foldKey.get(c) ?? c) === (foldKey.get(y[i]) ?? y[i]))
}

/** Whether `os` (a GOOS value) uses Windows paths; anything else is POSIX. */
const isWindows = (os: string) => os.toLowerCase() === "windows"

/**
 * Whether `path` is absolute on a device running `os`: on Windows a drive
 * letter with a separator ("C:\") or a UNC path naming a server and a share
 * ("\\server\share"); elsewhere a leading slash.
 */
export function isAbsolutePath(path: string, os: string): boolean {
  if (!isWindows(os)) return path.startsWith("/")
  if (/^[A-Za-z]:[\\/]/.test(path)) return true
  if (!path.startsWith("\\\\")) return false
  const rest = path.slice(2)
  const sep = rest.indexOf("\\")
  if (sep <= 0) return false
  const share = rest.slice(sep + 1)
  return share !== "" && share[0] !== "\\"
}

/**
 * Trims whitespace and trailing separators, keeping a bare root intact. On
 * POSIX, an absolute path also loses repeated slashes and "." segments
 * ("/a//./b" is "/a/b"); ".." is kept, since resolving it depends on
 * symbolic links on the device. On Windows, separators become backslashes
 * and the drive letter upper case, so "d:/work" and "D:\work" are the same
 * location.
 */
export function normalizePath(path: string, os: string): string {
  const trimmed = path.trim()
  if (!isWindows(os)) {
    if (!trimmed.startsWith("/")) return trimmed.replace(/\/+$/, "") // relative: refused later
    return `/${trimmed.split("/").filter((s) => s !== "" && s !== ".").join("/")}`
  }
  const windows = trimmed
    .replaceAll("/", "\\")
    .replace(/^([A-Za-z]):/, (_, drive: string) => `${drive.toUpperCase()}:`)
  const stripped = windows.replace(/\\+$/, "")
  if (/^[A-Z]:$/.test(stripped)) return `${stripped}\\`
  return stripped
}

/** A Windows path in the device or long-path namespace (\\?\…, \\.\…). */
function windowsDevicePath(path: string): boolean {
  if (!path.startsWith("\\\\")) return false
  const server = path.slice(2).split("\\")[0]
  return server === "?" || server === "."
}

const examplePath = (os: string) => (isWindows(os) ? "C:\\src\\app" : "/home/you/src/app")

/**
 * Checks a location's path for a device running `os`; the value is the
 * normalized path. An empty or blank path stays empty: the device's default
 * directory for the project.
 */
export function checkPath(raw: string, os: string): Checked {
  const trimmed = raw.trim()
  if (trimmed === "") return accept("")
  if (FORBIDDEN.test(trimmed)) return refuse(i18n.t("validation.pathControlChars"))
  const path = normalizePath(trimmed, os)
  if (isWindows(os) && windowsDevicePath(path)) return refuse(i18n.t("validation.pathDeviceNamespace"))
  if (!isAbsolutePath(path, os)) {
    return refuse(i18n.t("validation.pathNotAbsolute", { example: examplePath(os) }))
  }
  if (utf8Length(path) > PATH_MAX_BYTES) {
    return refuse(i18n.t("validation.pathTooLong", { max: PATH_MAX_BYTES }))
  }
  return accept(path)
}

/**
 * Checks a repository URL; the value is the trimmed URL, otherwise as given.
 * Empty means none. Accepted: https://host/path, http://host/path,
 * ssh://[user@]host[:port]/path, git://host/path, file:///abs/path and
 * scp-like user@host:path (see git-url.ts).
 */
export function checkRepoUrl(raw: string): Checked {
  const url = raw.trim()
  if (url === "") return accept("")
  if (utf8Length(url) > REPO_URL_MAX_BYTES) {
    return refuse(i18n.t("validation.repoUrlTooLong", { max: REPO_URL_MAX_BYTES }))
  }
  if (SPACE.test(url) || FORBIDDEN.test(url)) return refuse(i18n.t("validation.repoUrlChars"))
  const ok = !url.startsWith("-") && (url.includes("://") ? urlLike(url) : scpLike(url))
  return ok ? accept(url) : refuse(i18n.t("validation.repoUrlInvalid"))
}

/** The directory a default location resolves to on its device. */
export const defaultPath = (projectId: string) => DEFAULT_PATH_PREFIX + projectId

export function sameLocation(a: Location, b: Location): boolean {
  return a.device === b.device && a.path === b.path
}
