import { useLayoutEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"

import type { ModelProvider, ModelProviderInput } from "@/api/types"
import {
  hasProviderErrors,
  mapProviderServerError,
  providerFormValues,
  sentRows,
  toProviderInput,
  validateProviderForm,
  type EnvRow,
  type EnvRowErrors,
  type ProviderField,
  type ProviderFormErrors,
  type ProviderFormValues,
} from "@/lib/model-provider-form"

interface ServerState {
  error: unknown
  /** What the failed request sent. */
  sent: ProviderFormValues
  /** Errors no longer shown: a field, "form", or "<row key>.name|value". */
  dismissed: ReadonlySet<string>
}

/** The ids of what changed between two versions of the form; any change dismisses the form-wide error. */
function changedIds(before: ProviderFormValues, after: ProviderFormValues): string[] {
  const ids: string[] = []
  for (const field of ["name", "baseUrl", "scheme", "key", "fableModel", "opusModel", "sonnetModel", "haikuModel", "maxSessions"] as const) {
    if (before[field] !== after[field]) ids.push(field)
  }
  if (before.workerIds !== after.workerIds) ids.push("workerIds")
  if (before.env !== after.env) {
    ids.push("env")
    for (const row of after.env) {
      const old = before.env.find((r) => r.key === row.key)
      // A row that moved changes the indexes the server's error used.
      if (!old || before.env.indexOf(old) !== after.env.indexOf(row)) ids.push(`${row.key}.name`, `${row.key}.value`)
      else {
        if (old.name !== row.name) ids.push(`${row.key}.name`)
        if (old.value !== row.value || old.empty !== row.empty) ids.push(`${row.key}.value`)
      }
    }
  }
  if (ids.length > 0) ids.push("form")
  return ids
}

/**
 * State of the provider form. Local errors show once a field was touched
 * or the form was submitted; server errors show until what they are about
 * is edited. `editing` is the provider as the form loaded it.
 */
export function useProviderForm({
  editing,
  others,
}: {
  editing: ModelProvider | undefined
  others: readonly { name: string }[]
}) {
  // Messages are made during render; re-render when the language changes.
  useTranslation()
  const [values, setValues] = useState(() => providerFormValues(editing))
  const [touched, setTouched] = useState(() => new Set<string>())
  const [submitted, setSubmitted] = useState(false)
  const [server, setServer] = useState<ServerState | null>(null)
  // What the last submit sent, and what the form holds now: a server error
  // that arrives after more edits must not show on the fields edited since.
  const submittedValues = useRef(values)
  const current = useRef(values)
  useLayoutEffect(() => {
    current.current = values
  }, [values])

  const context = { others, editing }
  const local = validateProviderForm(values, context)
  const fromServer = server ? mapProviderServerError(server.error, server.sent, context) : null
  const shows = (id: string) => submitted || touched.has(id)
  const serverMessage = (id: string, message: string | undefined) =>
    message !== undefined && !server?.dismissed.has(id) ? message : undefined

  const fields: ProviderFormErrors["fields"] = {}
  for (const field of Object.keys({ ...local.fields, ...fromServer?.fields }) as ProviderField[]) {
    fields[field] = (shows(field) ? local.fields[field] : undefined) ?? serverMessage(field, fromServer?.fields[field])
  }
  const env: Record<string, EnvRowErrors> = {}
  for (const row of values.env) {
    const errors: EnvRowErrors = {}
    for (const part of ["name", "value"] as const) {
      const id = `${row.key}.${part}`
      const message = (shows(id) ? local.env[row.key]?.[part] : undefined) ?? serverMessage(id, fromServer?.env[row.key]?.[part])
      if (message) errors[part] = message
    }
    if (errors.name || errors.value) env[row.key] = errors
  }
  const errors: ProviderFormErrors = { fields, env, form: serverMessage("form", fromServer?.form) }

  const change = (next: ProviderFormValues) => {
    const ids = changedIds(values, next)
    setServer((s) => s && { ...s, dismissed: new Set([...s.dismissed, ...ids]) })
    setValues(next)
  }
  const set = <K extends keyof ProviderFormValues>(field: K, value: ProviderFormValues[K]) =>
    change({ ...values, [field]: value })

  return {
    values,
    errors,
    set,
    setEnv: (rows: readonly EnvRow[]) => change({ ...values, env: rows }),
    touch(id: string) {
      setTouched((t) => (t.has(id) ? t : new Set(t).add(id)))
    },
    /** The request to send, or null when the form shows errors instead. */
    submit(): ModelProviderInput | null {
      setSubmitted(true)
      setServer(null)
      if (hasProviderErrors(local)) return null
      // Dropped blank rows are gone from the form too, so indexes match.
      const kept = sentRows(values)
      const sent = kept.length === values.env.length ? values : { ...values, env: kept }
      if (sent !== values) setValues(sent)
      current.current = sent
      submittedValues.current = sent
      return toProviderInput(sent, editing)
    },
    serverError(error: unknown) {
      const sent = submittedValues.current
      setServer({ error, sent, dismissed: new Set(changedIds(sent, current.current)) })
    },
    /** Starts over from `provider` (or empty), e.g. after a reload. */
    reset(provider?: ModelProvider) {
      setValues(providerFormValues(provider))
      setTouched(new Set())
      setSubmitted(false)
      setServer(null)
    },
  }
}
