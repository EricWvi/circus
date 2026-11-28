import { useLayoutEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"

import type { Device, Location } from "@/api/types"
import { i18n } from "@/i18n/i18n"
import {
  derivedProjectName,
  hasErrors,
  mapServerError,
  noErrors,
  toLocations,
  validateForm,
  type FormErrors,
  type FormValues,
  type LocationField,
  type LocationRow,
  type RowErrors,
  type SavedLocation,
} from "@/lib/location-form"

/** Why the last submit failed, kept as the cause so its text follows the language. */
type ServerProblem =
  | { kind: "error"; error: unknown; form: FormValues }
  | { kind: "devices_not_ready" }

interface ServerState {
  problem: ServerProblem
  /**
   * Errors no longer shown: "name", "repoUrl", "form", "rows" (all) or
   * "<row key>.<field>".
   */
  dismissed: ReadonlySet<string>
}

/** The server errors to show now, in the active language. */
function serverErrors(state: ServerState | null, devices: readonly Device[]): FormErrors {
  if (!state) return noErrors()
  const { problem, dismissed } = state
  const all =
    problem.kind === "devices_not_ready"
      ? { rows: {}, form: i18n.t("projects.devicesNotReady") }
      : mapServerError(problem.error, problem.form, devices)
  const shown: FormErrors = { rows: {} }
  if (!dismissed.has("name")) shown.name = all.name
  if (!dismissed.has("repoUrl")) shown.repoUrl = all.repoUrl
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

/**
 * The server errors that no longer apply after the form went from `before`
 * to `after`: the edited fields and any form-wide error. An added or removed
 * row drops every row error.
 */
export function changedFieldIds(before: FormValues, after: FormValues): string[] {
  const ids: string[] = []
  if (before.name !== after.name) ids.push("name")
  if (before.repoUrl !== after.repoUrl) ids.push("repoUrl")
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

/** What a valid form asks for. */
export interface ProjectRequest {
  name: string
  repoUrl: string
  locations: Location[]
}

/**
 * State of a project form: name, repository URL and locations. Local errors
 * show once a field was touched or the form was submitted; server errors
 * show until the field they belong to is edited.
 */
export function useProjectForm({
  initial,
  devices,
  otherProjects,
  savedLocations,
  deriveName = false,
}: {
  initial: () => FormValues
  devices: Device[] | undefined
  otherProjects: readonly { name: string }[]
  /**
   * The project's saved locations, when editing one: rows are checked only
   * once they differ (see `validateForm`).
   */
  savedLocations?: readonly SavedLocation[]
  /**
   * The name is filled with one derived from the locations or the
   * repository URL (see `derivedProjectName`), following them as they
   * change until the name is typed in. Clearing the name starts over:
   * leaving the field, or the next change to the locations or the URL
   * after that, fills it again. Nothing is filled in while the field has
   * focus (see `nameEntered` and `nameLeft`).
   */
  deriveName?: boolean
}) {
  // Messages are made during render; re-render when the language changes.
  useTranslation()
  const [values, setValues] = useState(initial)
  const { name, repoUrl, rows } = values
  const [touched, setTouched] = useState(() => new Set<string>())
  const [submitted, setSubmitted] = useState(false)
  const [serverState, setServerState] = useState<ServerState | null>(null)
  // What the last submit sent, and what the form holds now: a server error
  // that arrives after more edits must not show on the fields edited since.
  const submittedValues = useRef<FormValues>(values)
  const currentValues = useRef<FormValues>(values)
  useLayoutEffect(() => {
    currentValues.current = values
  }, [values])
  const server = serverErrors(serverState, devices ?? [])
  const dismiss = (ids: readonly string[]) =>
    setServerState((s) => s && { ...s, dismissed: new Set([...s.dismissed, ...ids]) })
  // Whether the name follows the locations and the repository URL: until
  // it is typed in, and again once cleared.
  const [autoName, setAutoName] = useState(deriveName)
  // The name field has focus: what it holds stays under the user's cursor,
  // even when a folder's repository URL arrives meanwhile.
  const nameFocused = useRef(false)

  // What an empty name would be filled with now.
  const derivedName = autoName ? derivedProjectName(values, otherProjects) : undefined
  const local = validateForm(values, devices ?? [], otherProjects, savedLocations)
  // An empty name isn't missing while there is one to fill in.
  if (derivedName !== undefined && name.trim() === "") local.name = undefined
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

  const change = (next: FormValues) => {
    // Filled as the form changes, never as the other projects do: the name
    // doesn't move under the user, or once the project it names was created.
    if (
      autoName &&
      !nameFocused.current &&
      (next.rows !== values.rows || next.repoUrl !== values.repoUrl)
    ) {
      next = { ...next, name: derivedProjectName(next, otherProjects) ?? "" }
    }
    dismiss(changedFieldIds(values, next))
    setValues(next)
  }

  const touch = (id: "name" | "repoUrl" | `${string}.${LocationField}`) =>
    setTouched((t) => (t.has(id) ? t : new Set(t).add(id)))

  return {
    name,
    repoUrl,
    rows,
    values,
    errors: {
      name: (shows("name") ? local.name : undefined) ?? server.name,
      repoUrl: (shows("repoUrl") ? local.repoUrl : undefined) ?? server.repoUrl,
      rows: rowErrors,
      form: server.form,
    },
    setName(value: string) {
      if (deriveName) setAutoName(value.trim() === "")
      change({ ...values, name: value })
    },
    /** The name field took focus. */
    nameEntered() {
      nameFocused.current = true
    },
    /**
     * The name field was left. A name not typed in (with `deriveName`)
     * catches up with the form, e.g. a cleared one is filled again.
     */
    nameLeft() {
      nameFocused.current = false
      const next = autoName ? (derivedName ?? "") : name
      if (next !== name) change({ ...values, name: next })
      if (next !== "") touch("name")
    },
    setRepoUrl: (value: string) => change({ ...values, repoUrl: value }),
    setRows: (next: readonly LocationRow[]) => change({ ...values, rows: next }),
    touch,
    /** The request to send, or null when the form shows errors instead. */
    submit(): ProjectRequest | null {
      setSubmitted(true)
      if (devices === undefined) {
        setServerState({ problem: { kind: "devices_not_ready" }, dismissed: new Set() })
        return null
      }
      setServerState(null)
      if (hasErrors(local)) return null
      submittedValues.current = values
      return { name: name.trim() || (derivedName ?? ""), repoUrl: repoUrl.trim(), locations: toLocations(rows) }
    },
    /** Gives rows without a device the default one, leaving everything else. */
    fillDevice(device: string) {
      if (device === "" || !rows.some((r) => r.device === "")) return
      setValues({ ...values, rows: rows.map((r) => (r.device === "" ? { ...r, device } : r)) })
    },
    serverError(error: unknown) {
      const sent = submittedValues.current
      setServerState({
        problem: { kind: "error", error, form: sent },
        dismissed: new Set(changedFieldIds(sent, currentValues.current)),
      })
    },
    /** Starts over from saved values, e.g. after a successful save. */
    reset(next: FormValues) {
      setValues(next)
      setTouched(new Set())
      setSubmitted(false)
      setServerState(null)
    },
  }
}
