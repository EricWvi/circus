import { useLayoutEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"

import type { Device, Location } from "@/api/types"
import { i18n } from "@/i18n/i18n"
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

/** Why the last submit failed, kept as the cause so its text follows the language. */
type ServerProblem =
  | { kind: "error"; error: unknown; form: { name: string; rows: readonly LocationRow[] } }
  | { kind: "devices_not_ready" }

interface ServerState {
  problem: ServerProblem
  /** Errors no longer shown: "name", "form", "rows" (all) or "<row key>.<field>". */
  dismissed: ReadonlySet<string>
}

/** The server errors to show now, in the active language. */
function serverErrors(state: ServerState | null): FormErrors {
  if (!state) return noErrors()
  const { problem, dismissed } = state
  const all =
    problem.kind === "devices_not_ready"
      ? { rows: {}, form: i18n.t("projects.devicesNotReady") }
      : mapServerError(problem.error, problem.form)
  const shown: FormErrors = { rows: {} }
  if (!dismissed.has("name")) shown.name = all.name
  if (!dismissed.has("form")) shown.form = all.form
  if (dismissed.has("rows")) return shown
  for (const [key, errors] of Object.entries(all.rows)) {
    const kept: RowErrors = {}
    for (const field of ["device", "path"] as const) {
      if (errors[field] && !dismissed.has(`${key}.${field}`)) kept[field] = errors[field]
    }
    if (kept.device || kept.path) shown.rows[key] = kept
  }
  return shown
}

type FormValues = { name: string; rows: readonly LocationRow[] }

/**
 * The server errors that no longer apply after the form went from `before`
 * to `after`: the edited fields and any form-wide error. An added or removed
 * row drops every row error.
 */
export function changedFieldIds(before: FormValues, after: FormValues): string[] {
  const ids: string[] = []
  if (before.name !== after.name) ids.push("name")
  if (before.rows.length !== after.rows.length) ids.push("rows")
  else {
    for (const row of after.rows) {
      const old = before.rows.find((r) => r.key === row.key)
      if (!old) {
        ids.push("rows")
        break
      }
      if (old.device !== row.device) ids.push(`${row.key}.device`)
      if (old.path !== row.path) ids.push(`${row.key}.path`)
    }
  }
  if (ids.length > 0) ids.push("form")
  return ids
}

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
  // Messages are made during render; re-render when the language changes.
  useTranslation()
  const [name, setNameState] = useState(initialName)
  const [rows, setRowsState] = useState(initialRows)
  const [touched, setTouched] = useState(() => new Set<string>())
  const [submitted, setSubmitted] = useState(false)
  const [serverState, setServerState] = useState<ServerState | null>(null)
  // What the last submit sent, and what the form holds now: a server error
  // that arrives after more edits must not show on the fields edited since.
  const submittedValues = useRef<FormValues>({ name, rows })
  const currentValues = useRef<FormValues>({ name, rows })
  useLayoutEffect(() => {
    currentValues.current = { name, rows }
  }, [name, rows])
  const server = serverErrors(serverState)
  const dismiss = (ids: readonly string[]) =>
    setServerState((s) => s && { ...s, dismissed: new Set([...s.dismissed, ...ids]) })

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
      dismiss(changedFieldIds({ name, rows }, { name: value, rows }))
    },
    setRows(next: LocationRow[]) {
      dismiss(changedFieldIds({ name, rows }, { name, rows: next }))
      setRowsState(next)
    },
    touch(id: "name" | `${string}.${LocationField}`) {
      setTouched((t) => (t.has(id) ? t : new Set(t).add(id)))
    },
    /** The request to send, or null when the form shows errors instead. */
    submit(): { name: string; locations: Location[] } | null {
      setSubmitted(true)
      if (devices === undefined) {
        setServerState({ problem: { kind: "devices_not_ready" }, dismissed: new Set() })
        return null
      }
      setServerState(null)
      if (hasErrors(local)) return null
      submittedValues.current = { name, rows }
      return { name: name.trim(), locations: toLocations(rows) }
    },
    /** Gives rows without a device the default one, leaving everything else. */
    fillDevice(device: string) {
      if (device === "" || !rows.some((r) => r.device === "")) return
      setRowsState(rows.map((r) => (r.device === "" ? { ...r, device } : r)))
    },
    serverError(error: unknown) {
      const sent = submittedValues.current
      setServerState({
        problem: { kind: "error", error, form: sent },
        dismissed: new Set(changedFieldIds(sent, currentValues.current)),
      })
    },
    /** Starts over from saved values, e.g. after a successful save. */
    reset(nextName: string, nextRows: LocationRow[]) {
      setNameState(nextName)
      setRowsState(nextRows)
      setTouched(new Set())
      setSubmitted(false)
      setServerState(null)
    },
  }
}
