// Form state and checks for editing a project's name, repository URL and
// locations. Shared by the new-project dialog and project settings.

import { nanoid } from "nanoid"

import { errorMessage, isApiError } from "@/api/errors"
import type { Device, Location, Project, ProjectLocation } from "@/api/types"
import {
  checkPath,
  checkRepoUrl,
  DEFAULT_PATH_PREFIX,
  MAX_LOCATIONS,
  normalizePath,
  sameProjectName,
  validateProjectName,
} from "@/api/validation"
import { i18n } from "@/i18n/i18n"

export interface LocationRow {
  /** Stable React key; not sent to the server. */
  key: string
  device: string
  /** Empty for the device's default directory. */
  path: string
  /**
   * The saved location's device was revoked. If a device with that name is
   * listed again, saving binds the location to it. Dropped once the row
   * gets another device.
   */
  revoked?: boolean
}

/** A saved location, as the form compares rows with it. */
export type SavedLocation = Location & { deviceRevoked?: boolean }

export type LocationField = "device" | "path"
export type RowErrors = Partial<Record<LocationField, string>>

export interface FormValues {
  name: string
  repoUrl: string
  rows: readonly LocationRow[]
}

export interface FormErrors {
  name?: string
  repoUrl?: string
  /** By row key. */
  rows: Record<string, RowErrors>
  /** An error no single field can show. */
  form?: string
}

export const noErrors = (): FormErrors => ({ rows: {} })

export const hasErrors = (errors: FormErrors) =>
  errors.name !== undefined ||
  errors.repoUrl !== undefined ||
  errors.form !== undefined ||
  Object.values(errors.rows).some((r) => r.device !== undefined || r.path !== undefined)

/** Online devices first, then by name. */
export function sortDevices(devices: readonly Device[]): Device[] {
  return [...devices].sort(
    (a, b) => Number(b.online) - Number(a.online) || a.name.localeCompare(b.name),
  )
}

/** The device a new row starts with: the first online one, else the first. */
export function defaultDevice(devices: readonly Device[]): string {
  return sortDevices(devices)[0]?.name ?? ""
}

/** A short, readable OS name. */
export function osLabel(os: string): string {
  return { darwin: "macOS", linux: "Linux", windows: "Windows" }[os] ?? os
}

/** An empty row on the default device: the device's default directory. */
export const newRow = (devices: readonly Device[]): LocationRow => ({
  key: nanoid(),
  device: defaultDevice(devices),
  path: "",
})

/** Whether an earlier row already is `row`'s device's default location. */
export const hasEarlierDefault = (rows: readonly LocationRow[], index: number) =>
  rows
    .slice(0, index)
    .some((r) => r.device === rows[index].device && r.path.trim() === "")

/**
 * The row "Add location" appends: on the first device (online first) that
 * has no default location in the form yet, as its default directory. When
 * every device has one, on the first device, needing an explicit path
 * (`needsPath`).
 */
export function addedRow(
  rows: readonly LocationRow[],
  devices: readonly Device[],
): { row: LocationRow; needsPath: boolean } {
  const sorted = sortDevices(devices)
  const free = sorted.find((d) => !rows.some((r) => r.device === d.name && r.path.trim() === ""))
  return {
    row: { key: nanoid(), device: (free ?? sorted[0])?.name ?? "", path: "" },
    needsPath: !free,
  }
}

/** Placeholder hinting at an explicit path in the style of a device's OS. */
export function pathPlaceholder(os: string | undefined): string {
  if (os === "windows") return "C:\\…"
  if (os === "darwin") return "/Users/…"
  return "/home/…"
}

/**
 * A default directory short enough to read in a path field: the middle of
 * a long project id becomes "…", so both ends stay visible
 * ("~/circus/projects/01a1e8…c2be3d6").
 */
export function shortDefaultPath(path: string, keep: { head: number; tail: number } = { head: 6, tail: 7 }) {
  if (!path.startsWith(DEFAULT_PATH_PREFIX)) return path
  const id = path.slice(DEFAULT_PATH_PREFIX.length)
  if (id.length <= keep.head + keep.tail + 1) return path
  return `${DEFAULT_PATH_PREFIX}${id.slice(0, keep.head)}…${id.slice(-keep.tail)}`
}

/**
 * A path short enough for a one-line note: the middle of a long path
 * becomes "…", keeping its start and its last folders
 * ("/home/eric/…/projects/circus/web").
 */
export function shortPath(path: string, keep: { head: number; tail: number } = { head: 12, tail: 28 }) {
  const chars = Array.from(path)
  if (chars.length <= keep.head + keep.tail + 1) return path
  return `${chars.slice(0, keep.head).join("")}…${chars.slice(-keep.tail).join("")}`
}

/** The form's rows for a project's saved locations. */
export const rowsOf = (project: { locations: readonly ProjectLocation[] }): LocationRow[] =>
  project.locations.map((l) => ({
    key: nanoid(),
    device: l.device,
    path: l.path,
    ...(l.deviceRevoked ? { revoked: true } : {}),
  }))

/**
 * A row whose saved device was revoked while a device with the same name is
 * listed again: saving binds the location to that new device.
 */
export const isReplaced = (row: LocationRow, devices: readonly Device[]) =>
  row.revoked === true && devices.some((d) => d.name === row.device)

/** The directory a location's sessions run in. */
export const locationDirectory = (l: ProjectLocation) => l.path || l.effectivePath

/**
 * Checks every row; a later duplicate of an earlier row gets the error. A
 * device has at most one default location (empty path). A device that isn't
 * listed can only come from a saved location: its device was removed.
 */
export function validateRows(
  rows: readonly LocationRow[],
  devices: readonly Device[],
): Record<string, RowErrors> {
  const errors: Record<string, RowErrors> = {}
  const seen = new Set<string>()
  for (const row of rows) {
    const device = devices.find((d) => d.name === row.device)
    const rowErrors: RowErrors = {}
    if (!device) {
      rowErrors.device = i18n.t(row.device === "" ? "validation.deviceRequired" : "validation.deviceRevoked")
    } else {
      const checked = checkPath(row.path, device.os)
      if (!checked.ok) {
        rowErrors.path = checked.problem
      } else {
        const id = `${device.name}\n${checked.value}`
        if (seen.has(id)) {
          rowErrors.path = i18n.t(
            checked.value === "" ? "validation.defaultLocationDuplicate" : "validation.locationDuplicate",
          )
        }
        seen.add(id)
      }
    }
    if (rowErrors.device || rowErrors.path) errors[row.key] = rowErrors
  }
  return errors
}

/** Checks a project name, including against the user's other projects. */
export function validateName(
  name: string,
  others: readonly { name: string }[],
): string | undefined {
  const problem = validateProjectName(name)
  if (problem) return problem
  if (others.some((p) => sameProjectName(p.name, name))) {
    return i18n.t("validation.projectNameTaken", { name: name.trim() })
  }
  return undefined
}

export function validateRepoUrl(repoUrl: string): string | undefined {
  const checked = checkRepoUrl(repoUrl)
  return checked.ok ? undefined : checked.problem
}

/**
 * Checks the whole form. With `savedLocations`, the rows are checked only
 * when they differ from those: unchanged locations aren't sent, so a row
 * whose device was revoked since doesn't block renaming the project.
 */
export function validateForm(
  form: FormValues,
  devices: readonly Device[],
  otherProjects: readonly { name: string }[],
  savedLocations?: readonly SavedLocation[],
): FormErrors {
  const checkRows = !savedLocations || locationsChanged(savedLocations, form.rows, devices)
  return {
    name: validateName(form.name, otherProjects),
    repoUrl: validateRepoUrl(form.repoUrl),
    rows: checkRows ? validateRows(form.rows, devices) : {},
  }
}

/** The request's locations, in row order. */
export const toLocations = (rows: readonly LocationRow[]): Location[] =>
  rows.map((r) => ({ device: r.device, path: r.path.trim() }))

/**
 * A one-line message for a failed project operation: like `errorMessage`,
 * but a missing project and the per-user limits say so.
 */
export function projectErrorMessage(error: unknown): string {
  if (isApiError(error, "not_found")) return i18n.t("projects.projectGone")
  if (isApiError(error, "resource_exhausted")) return i18n.t("projects.limitReached")
  if (isApiError(error, "aborted")) return i18n.t("projects.changedElsewhere")
  return errorMessage(error)
}

/**
 * Puts a server error where it belongs: on the name, the repository URL, a
 * location row's device or path (by the error's field path), or as one line
 * for the form. The message is chosen by code and field, preferring the
 * client's own check when it explains the refusal; the server's text is
 * never shown.
 */
export function mapServerError(
  error: unknown,
  form: FormValues,
  devices: readonly Device[] = [],
): FormErrors {
  if (!isApiError(error)) return { rows: {}, form: projectErrorMessage(error) }
  if (error.field === "name") {
    if (error.code === "already_exists") {
      return { rows: {}, name: i18n.t("validation.projectNameTaken", { name: form.name.trim() }) }
    }
    if (error.code === "invalid_argument") {
      return {
        rows: {},
        name: validateProjectName(form.name) ?? i18n.t("validation.projectNameRejected"),
      }
    }
  }
  if (error.code === "invalid_argument") {
    if (error.field === "repo_url") {
      return { rows: {}, repoUrl: validateRepoUrl(form.repoUrl) ?? i18n.t("validation.repoUrlRejected") }
    }
    if (error.field === "locations") {
      return { rows: {}, form: i18n.t("validation.locationsTooMany", { max: MAX_LOCATIONS }) }
    }
    if (error.field === "last_used_location") {
      return { rows: {}, form: i18n.t("projects.locationGone") }
    }
    const match = /^locations\[(\d+)\]\.(device|path)$/.exec(error.field ?? "")
    const row = match ? form.rows[Number(match[1])] : undefined
    if (match && row) {
      const field = match[2] as LocationField
      // The client's own check when it explains the refusal; otherwise the
      // server knows something the client doesn't.
      const local = validateRows(form.rows, devices)[row.key]?.[field]
      const message =
        local ?? i18n.t(field === "device" ? "validation.deviceUnknown" : "validation.pathRejected")
      return { rows: { [row.key]: { [field]: message } } }
    }
  }
  return { rows: {}, form: projectErrorMessage(error) }
}

/**
 * Whether the rows differ from saved locations. Paths compare the way the
 * server stores them (normalized for the device's OS), so "/a/" is not a
 * change from "/a". A saved location whose device was revoked, while a
 * device with that name is listed again, counts as changed: saving it binds
 * the new device.
 */
export function locationsChanged(
  saved: readonly SavedLocation[],
  rows: readonly LocationRow[],
  devices: readonly Device[] = [],
): boolean {
  if (rows.length !== saved.length) return true
  return rows.some((r, i) => {
    const device = devices.find((d) => d.name === r.device)
    const location = saved[i]
    return (
      r.device !== location.device ||
      normalizePath(r.path, device?.os ?? "linux") !== location.path ||
      (location.deviceRevoked === true && device !== undefined)
    )
  })
}

/** Whether the form differs from the saved project. */
export function formChanged(
  saved: { name: string; repoUrl: string; locations: readonly SavedLocation[] },
  form: FormValues,
  devices: readonly Device[] = [],
): boolean {
  return (
    form.name.trim() !== saved.name ||
    form.repoUrl.trim() !== saved.repoUrl ||
    locationsChanged(saved.locations, form.rows, devices)
  )
}

/**
 * The location a new session starts in: the chosen one if the project
 * still has it, else the project's last used location (clamped into
 * range), else the first one, skipping locations whose device was revoked.
 * -1 when no location can be used.
 */
export function pickLocation(
  project: Pick<Project, "locations" | "lastUsedLocation">,
  chosen: Location | null,
): number {
  const { locations } = project
  const usable = (i: number) => i >= 0 && i < locations.length && !locations[i].deviceRevoked
  if (chosen) {
    const found = locations.findIndex((l) => l.device === chosen.device && l.path === chosen.path)
    if (usable(found)) return found
  }
  const last = Math.max(0, Math.min(project.lastUsedLocation, locations.length - 1))
  if (usable(last)) return last
  return locations.findIndex((l) => !l.deviceRevoked)
}
