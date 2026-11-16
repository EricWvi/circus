import { useState } from "react"

import type { Device, Location } from "@/api/types"
import {
  hasErrors,
  mapServerError,
  noErrors,
  toLocations,
  validateForm,
  type FormErrors,
  type LocationField,
  type LocationRow,
  type RowErrors,
} from "@/lib/location-form"

/**
 * State of a project name + locations form. Local errors show once a field
 * was touched or the form was submitted; server errors show until the field
 * they belong to is edited.
 */
export function useProjectForm({
  initialName,
  initialRows,
  devices,
  otherProjects,
}: {
  initialName: string
  initialRows: () => LocationRow[]
  devices: Device[] | undefined
  otherProjects: readonly { name: string }[]
}) {
  const [name, setNameState] = useState(initialName)
  const [rows, setRowsState] = useState(initialRows)
  const [touched, setTouched] = useState(() => new Set<string>())
  const [submitted, setSubmitted] = useState(false)
  const [server, setServer] = useState<FormErrors>(noErrors)

  const local = validateForm({ name, rows }, devices ?? [], otherProjects)
  const shows = (id: string) => submitted || touched.has(id)

  const rowErrors: Record<string, RowErrors> = {}
  for (const row of rows) {
    const errors: RowErrors = {}
    for (const field of ["device", "path"] as const) {
      const message =
        (shows(`${row.key}.${field}`) ? local.rows[row.key]?.[field] : undefined) ??
        server.rows[row.key]?.[field]
      if (message) errors[field] = message
    }
    if (errors.device || errors.path) rowErrors[row.key] = errors
  }

  return {
    name,
    rows,
    errors: {
      name: (shows("name") ? local.name : undefined) ?? server.name,
      rows: rowErrors,
      form: server.form,
    },
    setName(value: string) {
      setNameState(value)
      setServer((s) => ({ ...s, name: undefined, form: undefined }))
    },
    setRows(next: LocationRow[]) {
      // Edited fields drop their server errors; a reorder or removal drops all.
      setServer((s) => {
        const kept: Record<string, RowErrors> = {}
        for (const row of next) {
          const before = rows.find((r) => r.key === row.key)
          const errors = s.rows[row.key]
          if (!before || !errors || rows.length !== next.length) continue
          kept[row.key] = {
            ...(before.device === row.device && { device: errors.device }),
            ...(before.path === row.path && { path: errors.path }),
          }
        }
        return { ...s, rows: kept, form: undefined }
      })
      setRowsState(next)
    },
    touch(id: "name" | `${string}.${LocationField}`) {
      setTouched((t) => (t.has(id) ? t : new Set(t).add(id)))
    },
    /** The request to send, or null when the form shows errors instead. */
    submit(): { name: string; locations: Location[] } | null {
      setSubmitted(true)
      if (devices === undefined) {
        setServer({ rows: {}, form: "Devices aren't available yet. Try again in a moment." })
        return null
      }
      setServer(noErrors())
      if (hasErrors(local)) return null
      return { name: name.trim(), locations: toLocations(rows) }
    },
    /** Gives rows without a device the default one, leaving everything else. */
    fillDevice(device: string) {
      if (device === "" || !rows.some((r) => r.device === "")) return
      setRowsState(rows.map((r) => (r.device === "" ? { ...r, device } : r)))
    },
    serverError(error: unknown) {
      setServer(mapServerError(error, rows))
    },
    /** Starts over from saved values, e.g. after a successful save. */
    reset(nextName: string, nextRows: LocationRow[]) {
      setNameState(nextName)
      setRowsState(nextRows)
      setTouched(new Set())
      setSubmitted(false)
      setServer(noErrors())
    },
  }
}
