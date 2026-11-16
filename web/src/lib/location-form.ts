// Form state and checks for editing a project's name and locations. Shared
// by the new-project dialog and project settings.

import { nanoid } from "nanoid"

import { isApiError } from "@/api/errors"
import type { Device, Location } from "@/api/types"
import {
  isAbsolutePath,
  normalizePath,
  sameProjectName,
  validateProjectName,
} from "@/api/validation"

export interface LocationRow {
  /** Stable React key; not sent to the server. */
  key: string
  device: string
  path: string
}

export type LocationField = "device" | "path"
export type RowErrors = Partial<Record<LocationField, string>>

export interface FormErrors {
  name?: string
  /** By row key. */
  rows: Record<string, RowErrors>
  /** An error no single field can show. */
  form?: string
}

export const noErrors = (): FormErrors => ({ rows: {} })

export const hasErrors = (errors: FormErrors) =>
  errors.name !== undefined ||
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

/** Placeholder hinting at the path style of a device's OS. */
export function pathPlaceholder(os: string | undefined): string {
  if (os === "windows") return "C:\\…"
  if (os === "darwin") return "/Users/…"
  return "/home/…"
}

/** An empty row on the default device. */
export const newRow = (devices: readonly Device[]): LocationRow => ({
  key: nanoid(),
  device: defaultDevice(devices),
  path: "",
})

const examplePath = (os: string) => (os === "windows" ? "C:\\src\\app" : "/home/you/src/app")

/** Checks every row; a later duplicate of an earlier row gets the error. */
export function validateRows(
  rows: readonly LocationRow[],
  devices: readonly Device[],
): Record<string, RowErrors> {
  const errors: Record<string, RowErrors> = {}
  const seen = new Set<string>()
  for (const row of rows) {
    const device = devices.find((d) => d.name === row.device)
    const rowErrors: RowErrors = {}
    if (!device) rowErrors.device = row.device === "" ? "Choose a device." : "Unknown device."
    if (row.path.trim() === "") {
      rowErrors.path = "Enter a path."
    } else if (device) {
      const path = normalizePath(row.path, device.os)
      if (!isAbsolutePath(path, device.os)) {
        rowErrors.path = `Use an absolute path, like ${examplePath(device.os)}.`
      } else {
        const id = `${device.name}\n${path}`
        if (seen.has(id)) rowErrors.path = "This location is already listed."
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
    return `A project named "${name.trim()}" already exists.`
  }
  return undefined
}

export function validateForm(
  form: { name: string; rows: readonly LocationRow[] },
  devices: readonly Device[],
  otherProjects: readonly { name: string }[],
): FormErrors {
  return { name: validateName(form.name, otherProjects), rows: validateRows(form.rows, devices) }
}

/** The request's locations, in row order. */
export const toLocations = (rows: readonly LocationRow[]): Location[] =>
  rows.map((r) => ({ device: r.device, path: r.path.trim() }))

/**
 * Puts a server error where it belongs: on the name, on a location row's
 * device or path (by the error's field path), or as one line for the form.
 */
export function mapServerError(error: unknown, rows: readonly LocationRow[]): FormErrors {
  const message = error instanceof Error ? error.message : "Something went wrong. Try again."
  if (!isApiError(error) || !error.field) return { rows: {}, form: messageFor(error, message) }
  if (error.field === "name") return { rows: {}, name: message }
  const match = /^locations\[(\d+)\]\.(device|path)$/.exec(error.field)
  const row = match ? rows[Number(match[1])] : undefined
  if (match && row) return { rows: { [row.key]: { [match[2] as LocationField]: message } } }
  return { rows: {}, form: message }
}

function messageFor(error: unknown, message: string): string {
  if (isApiError(error, "unavailable")) return "Can't reach the server. Try again."
  return isApiError(error) ? message : "Something went wrong. Try again."
}

/**
 * Whether the form differs from the saved project. Paths compare the way the
 * server stores them (normalized for the device's OS), so "/a/" is not a
 * change from "/a".
 */
export function formChanged(
  saved: { name: string; locations: readonly Location[] },
  form: { name: string; rows: readonly LocationRow[] },
  devices: readonly Device[] = [],
): boolean {
  if (form.name.trim() !== saved.name) return true
  if (form.rows.length !== saved.locations.length) return true
  return form.rows.some((r, i) => {
    const os = devices.find((d) => d.name === r.device)?.os ?? "linux"
    const location = saved.locations[i]
    return r.device !== location.device || normalizePath(r.path, os) !== location.path
  })
}

/** Whether two values of a project's editable fields are the same. */
export const sameProjectFields = (
  a: { name: string; locations: readonly Location[] },
  b: { name: string; locations: readonly Location[] },
) => a.name === b.name && JSON.stringify(a.locations) === JSON.stringify(b.locations)

/**
 * The index of the chosen location if the project still has it, else the
 * project's last used location, clamped into range.
 */
export function pickLocation(
  project: { locations: readonly Location[]; lastUsedLocation: number },
  chosen: Location | null,
): number {
  const found = chosen
    ? project.locations.findIndex((l) => l.device === chosen.device && l.path === chosen.path)
    : -1
  if (found >= 0) return found
  return Math.max(0, Math.min(project.lastUsedLocation, project.locations.length - 1))
}
