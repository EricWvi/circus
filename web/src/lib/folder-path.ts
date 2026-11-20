// Path handling for the folder picker. Its input holds a directory followed
// by a filter, like a file dialog's location bar: "/home/eric/sr" lists
// /home/eric and filters by "sr". Separators are "/" everywhere and also
// "\" on Windows. Paths are kept as the device writes them; the device
// resolves "~" and cleans them.

import { isApiError } from "@/api/errors"
import type { DirectoryEntry, DirectoryError } from "@/api/types"
import { i18n } from "@/i18n/i18n"

/** The home directory, as a request path. */
export const HOME = "~"

const separatorPattern = (os: string) => (os === "windows" ? /[\\/]/ : /\//)
const trailingSeparators = (os: string) => (os === "windows" ? /[\\/]+$/ : /\/+$/)

/** Whether a path is written the Windows way (a drive or a UNC prefix). */
const looksWindows = (path: string) => /^[A-Za-z]:/.test(path) || path.startsWith("\\\\")

/**
 * A directory as typed, made into a request path: trailing separators go,
 * except on a root ("/", "C:\").
 */
export function directoryKey(typed: string, os: string): string {
  const stripped = typed.replace(trailingSeparators(os), "")
  if (stripped === "") return typed === "" ? "" : typed[0]
  if (/^[A-Za-z]:$/.test(stripped) && os === "windows") return `${stripped}\\`
  return stripped
}

/**
 * Splits the picker's input into the directory to list (as a request path)
 * and the filter after the last separator. Without a separator there is no
 * directory: the whole input filters the directory already listed.
 */
export function splitInput(input: string, os: string): { dir: string | null; filter: string } {
  const pattern = separatorPattern(os)
  let last = -1
  for (let i = input.length - 1; i >= 0; i--) {
    if (pattern.test(input[i])) {
      last = i
      break
    }
  }
  if (last < 0) return { dir: null, filter: input }
  return { dir: directoryKey(input.slice(0, last + 1), os), filter: input.slice(last + 1) }
}

/** The path with one trailing separator, the way the input shows a directory. */
export function withSeparator(path: string): string {
  if (path === "" || path.endsWith("/") || path.endsWith("\\")) return path
  return path + (looksWindows(path) ? "\\" : "/")
}

/** A child of a directory. */
export const joinPath = (dir: string, name: string) => withSeparator(dir) + name

/**
 * The entries matching a filter, ignoring case: those starting with it
 * first, then those containing it, each in listing order.
 */
export function filterEntries(entries: readonly DirectoryEntry[], filter: string): DirectoryEntry[] {
  if (filter === "") return [...entries]
  const needle = filter.toLowerCase()
  const prefix: DirectoryEntry[] = []
  const inside: DirectoryEntry[] = []
  for (const entry of entries) {
    const name = entry.name.toLowerCase()
    if (name.startsWith(needle)) prefix.push(entry)
    else if (name.includes(needle)) inside.push(entry)
  }
  return [...prefix, ...inside]
}

export const pathErrorMessage = (error: DirectoryError) => i18n.t(`folders.pathError.${error}`)

/** What a failed listing request says, by its code. */
export function requestErrorMessage(error: unknown, device: string): string {
  if (isApiError(error, "failed_precondition")) return i18n.t("folders.requestError.offline", { device })
  if (isApiError(error, "unavailable")) return i18n.t("folders.requestError.unavailable", { device })
  if (isApiError(error, "resource_exhausted")) return i18n.t("folders.requestError.busy", { device })
  if (isApiError(error, "deadline_exceeded")) return i18n.t("folders.requestError.timeout", { device })
  if (isApiError(error, "not_found")) return i18n.t("folders.requestError.gone", { device })
  return i18n.t("errors.generic")
}
