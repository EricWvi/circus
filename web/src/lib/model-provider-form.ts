// Form state and checks for creating and editing a model provider.
//
// Secrets are write-only: editing starts with the key and every variable
// value empty. An empty key keeps the stored one; a variable whose value is
// left empty keeps the value stored under its name, if there is one. Once
// the stored secrets can't be read, nothing can be kept: the key is
// required and every value is sent as typed.

import { nanoid } from "nanoid"

import { errorMessage, isApiError } from "@/api/errors"
import {
  baseUrlProblem,
  envNameProblem,
  keyProblem,
  MAX_ENV_BYTES,
  MAX_ENV_VARS,
  MAX_PROVIDERS,
  maxSessionsProblem,
  modelProblem,
  providerNameProblem,
  utf8Length,
} from "@/api/provider-rules"
import type { ModelProvider, ModelProviderInput, ProviderScheme } from "@/api/types"
import { sameProjectName } from "@/api/validation"
import { i18n } from "@/i18n/i18n"

export interface EnvRow {
  /** Stable React key; not sent. */
  key: string
  name: string
  /** Empty keeps the stored value of the same name, when there is one. */
  value: string
  /**
   * The user chose an empty value on purpose: sent as "", whatever is
   * stored. Without it an empty input means "keep" (or is missing).
   */
  empty?: boolean
}

export interface ProviderFormValues {
  name: string
  baseUrl: string
  /** "unspecified" for a scheme this client doesn't know: the user must choose. */
  scheme: ProviderScheme | "unspecified"
  /** Empty keeps the stored key, when editing. */
  key: string
  fableModel: string
  opusModel: string
  sonnetModel: string
  haikuModel: string
  env: readonly EnvRow[]
  workerIds: readonly string[]
  /** As typed; empty means unlimited. */
  maxSessions: string
  isDefault: boolean
}

export type ProviderField =
  | "name"
  | "baseUrl"
  | "scheme"
  | "key"
  | "fableModel"
  | "opusModel"
  | "sonnetModel"
  | "haikuModel"
  | "env"
  | "workerIds"
  | "maxSessions"

export type EnvRowErrors = Partial<Record<"name" | "value", string>>

export interface ProviderFormErrors {
  fields: Partial<Record<ProviderField, string>>
  /** By row key. */
  env: Record<string, EnvRowErrors>
  /** An error no single field can show. */
  form?: string
}

export const noProviderErrors = (): ProviderFormErrors => ({ fields: {}, env: {} })

export const hasProviderErrors = (e: ProviderFormErrors) =>
  e.form !== undefined ||
  Object.values(e.fields).some((m) => m !== undefined) ||
  Object.values(e.env).some((r) => r.name !== undefined || r.value !== undefined)

/** "••••abcd": a secret's last four characters, as the server returns them. */
export const maskedSecret = (last4: string) => (last4 === "" ? "••••" : `••••${last4}`)

export const newEnvRow = (): EnvRow => ({ key: nanoid(), name: "", value: "" })

/** The form for a new provider, or for editing `provider` (secrets empty). */
export function providerFormValues(provider?: ModelProvider): ProviderFormValues {
  if (!provider) {
    return {
      name: "",
      baseUrl: "",
      scheme: "x_api_key",
      key: "",
      fableModel: "",
      opusModel: "",
      sonnetModel: "",
      haikuModel: "",
      env: [],
      workerIds: [],
      maxSessions: "",
      isDefault: false,
    }
  }
  return {
    name: provider.name,
    baseUrl: provider.baseUrl,
    scheme: provider.scheme,
    key: "",
    fableModel: provider.fableModel,
    opusModel: provider.opusModel,
    sonnetModel: provider.sonnetModel,
    haikuModel: provider.haikuModel,
    env: provider.env.map((v) => ({ key: nanoid(), name: v.name, value: "" })),
    workerIds: [...provider.workerIds],
    maxSessions: provider.maxSessions === 0 ? "" : String(provider.maxSessions),
    isDefault: provider.isDefault,
  }
}

/**
 * The stored variable a row keeps the value of: the one of the same name,
 * while the secrets can be read. Undefined for a new name.
 */
export function storedVariable(row: Pick<EnvRow, "name">, editing: ModelProvider | undefined) {
  if (!editing || editing.secretsUnreadable) return undefined
  return editing.env.find((v) => v.name === row.name.trim())
}

/** Rows left blank (no name, no value) are dropped, not sent. */
const isBlank = (row: EnvRow) => row.name.trim() === "" && row.value === "" && !row.empty

/** The rows a request sends, in order: `env[i]` in a server error is the i-th of these. */
export const sentRows = (values: ProviderFormValues) => values.env.filter((r) => !isBlank(r))

/** The session limit typed, or NaN when it isn't a whole number. Empty is 0 (unlimited). */
export function parseMaxSessions(text: string): number {
  const trimmed = text.trim()
  if (trimmed === "") return 0
  return /^\d+$/.test(trimmed) ? Number(trimmed) : NaN
}

/** Why the variable list as a whole is refused, or undefined. */
function envListProblem(values: ProviderFormValues): string | undefined {
  const rows = sentRows(values)
  if (rows.length > MAX_ENV_VARS) return i18n.t("validation.envTooMany", { max: MAX_ENV_VARS })
  // Kept values are unknown here; the server counts them.
  const bytes = rows.reduce((n, r) => n + utf8Length(r.name.trim()) + utf8Length(r.value), 0)
  if (bytes > MAX_ENV_BYTES) return i18n.t("validation.envTooLarge", { max: MAX_ENV_BYTES / 1024 })
  return undefined
}

/**
 * Checks every row; a later duplicate of an earlier name gets the error. A
 * value left empty keeps the stored value of that name, so a name without
 * one (new, renamed, or stored but unreadable) needs its value typed.
 */
function validateEnvRows(values: ProviderFormValues, editing: ModelProvider | undefined): Record<string, EnvRowErrors> {
  const errors: Record<string, EnvRowErrors> = {}
  const seen = new Set<string>()
  for (const row of sentRows(values)) {
    const name = row.name.trim()
    const rowErrors: EnvRowErrors = {}
    let problem = envNameProblem(name) ?? undefined
    if (!problem && seen.has(name)) problem = i18n.t("validation.envNameDuplicate")
    seen.add(name)
    if (problem) rowErrors.name = problem
    if (row.value === "" && !row.empty && !storedVariable(row, editing)) {
      rowErrors.value = i18n.t(
        editing?.secretsUnreadable && editing.env.some((v) => v.name === name)
          ? "validation.envValueReenter"
          : "validation.envValueRequired",
      )
    }
    if (rowErrors.name || rowErrors.value) errors[row.key] = rowErrors
  }
  return errors
}

/** Checks the whole form, as the server would. */
export function validateProviderForm(
  values: ProviderFormValues,
  { others, editing }: { others: readonly { name: string }[]; editing?: ModelProvider },
): ProviderFormErrors {
  const fields: ProviderFormErrors["fields"] = {}
  fields.name = providerNameProblem(values.name) ?? undefined
  if (!fields.name && others.some((p) => sameProjectName(p.name, values.name))) {
    fields.name = i18n.t("validation.providerNameTaken", { name: values.name.trim() })
  }
  fields.baseUrl = baseUrlProblem(values.baseUrl.trim()) ?? undefined
  const keyRequired = !editing || editing.secretsUnreadable
  fields.key = keyProblem(values.key.trim(), keyRequired) ?? undefined
  for (const field of ["fableModel", "opusModel", "sonnetModel", "haikuModel"] as const) {
    fields[field] = modelProblem(values[field].trim()) ?? undefined
  }
  fields.env = envListProblem(values)
  fields.maxSessions = maxSessionsProblem(parseMaxSessions(values.maxSessions)) ?? undefined
  if (values.scheme === "unspecified") fields.scheme = i18n.t("validation.schemeRequired")
  return { fields, env: validateEnvRows(values, editing) }
}

/** The request for a valid form. */
export function toProviderInput(values: ProviderFormValues, editing?: ModelProvider): ModelProviderInput {
  if (values.scheme === "unspecified") throw new Error("no scheme chosen")
  return {
    name: values.name.trim(),
    baseUrl: values.baseUrl.trim(),
    scheme: values.scheme,
    key: values.key.trim(),
    fableModel: values.fableModel.trim(),
    opusModel: values.opusModel.trim(),
    sonnetModel: values.sonnetModel.trim(),
    haikuModel: values.haikuModel.trim(),
    env: sentRows(values).map((row) => {
      const name = row.name.trim()
      // Left empty: keeps the stored value of that name, if there is one.
      if (row.empty) return { name, value: "" }
      return row.value === "" && storedVariable(row, editing) ? { name } : { name, value: row.value }
    }),
    workerIds: [...values.workerIds],
    maxSessions: parseMaxSessions(values.maxSessions),
    isDefault: values.isDefault,
  }
}

/** Whether the form would change the provider; a typed secret always does. */
export function providerFormChanged(provider: ModelProvider, values: ProviderFormValues): boolean {
  if (values.scheme === "unspecified") return true
  const input = toProviderInput(values, provider)
  const sameEnv =
    input.env.length === provider.env.length &&
    input.env.every((v, i) => v.value === undefined && v.name === provider.env[i].name)
  return (
    input.name !== provider.name ||
    input.baseUrl !== provider.baseUrl ||
    input.scheme !== provider.scheme ||
    input.key !== "" ||
    input.fableModel !== provider.fableModel ||
    input.opusModel !== provider.opusModel ||
    input.sonnetModel !== provider.sonnetModel ||
    input.haikuModel !== provider.haikuModel ||
    !sameEnv ||
    input.workerIds.length !== provider.workerIds.length ||
    input.workerIds.some((id) => !provider.workerIds.includes(id)) ||
    input.maxSessions !== provider.maxSessions ||
    input.isDefault !== provider.isDefault
  )
}

/**
 * A one-line message for a failed provider operation: like `errorMessage`,
 * but a missing provider, the per-user limit and a concurrent edit say so.
 */
export function providerErrorMessage(error: unknown): string {
  if (isApiError(error, "not_found")) return i18n.t("providers.form.gone")
  if (isApiError(error, "resource_exhausted")) return i18n.t("providers.form.limitReached", { max: MAX_PROVIDERS })
  if (isApiError(error, "aborted")) return i18n.t("providers.form.changedElsewhere")
  return errorMessage(error)
}

const apiFields: Record<string, ProviderField> = {
  "api.base_url": "baseUrl",
  "api.scheme": "scheme",
  "api.key": "key",
  "api.fable_model": "fableModel",
  "api.opus_model": "opusModel",
  "api.sonnet_model": "sonnetModel",
  "api.haiku_model": "haikuModel",
}

/**
 * Puts a server error where it belongs: on a field, on a variable row (by
 * the error's `env[i]` index into the rows sent), or as one line for the
 * form. The message is the client's own check when it explains the
 * refusal; the server's text is never shown.
 */
export function mapProviderServerError(
  error: unknown,
  values: ProviderFormValues,
  context: { others: readonly { name: string }[]; editing?: ModelProvider },
): ProviderFormErrors {
  const only = (fields: ProviderFormErrors["fields"]): ProviderFormErrors => ({ fields, env: {} })
  if (!isApiError(error)) return { ...noProviderErrors(), form: providerErrorMessage(error) }
  const local = validateProviderForm(values, context)
  const field = error.field ?? ""
  if (field === "name" && error.code === "already_exists") {
    return only({ name: i18n.t("validation.providerNameTaken", { name: values.name.trim() }) })
  }
  if (error.code === "invalid_argument") {
    if (field === "name") return only({ name: local.fields.name ?? i18n.t("validation.providerNameRejected") })
    const api = apiFields[field]
    if (api) {
      const fallback = {
        baseUrl: "validation.baseUrlRejected",
        scheme: "errors.generic",
        key: "validation.keyRejected",
        fableModel: "validation.modelRejected",
        opusModel: "validation.modelRejected",
        sonnetModel: "validation.modelRejected",
        haikuModel: "validation.modelRejected",
      } as const
      const key = api as keyof typeof fallback
      // An empty key the server wants is one it couldn't keep.
      const message = local.fields[key] ?? (key === "key" && values.key.trim() === "" ? i18n.t("validation.keyRequired") : i18n.t(fallback[key]))
      return only({ [key]: message })
    }
    if (field === "env") {
      const tooMany = sentRows(values).length > MAX_ENV_VARS
      return only({
        env:
          local.fields.env ??
          (tooMany
            ? i18n.t("validation.envTooMany", { max: MAX_ENV_VARS })
            : i18n.t("validation.envTooLarge", { max: MAX_ENV_BYTES / 1024 })),
      })
    }
    const match = /^env\[(\d+)\]\.(name|value)$/.exec(field)
    const row = match ? sentRows(values)[Number(match[1])] : undefined
    if (match && row) {
      const which = match[2] as "name" | "value"
      const message =
        which === "name"
          ? (local.env[row.key]?.name ?? i18n.t("validation.envNameRejected"))
          : i18n.t("validation.envValueRejected")
      return { fields: {}, env: { [row.key]: { [which]: message } } }
    }
    if (field === "worker_ids" || field.startsWith("worker_ids[")) {
      return only({ workerIds: i18n.t("validation.workersRejected") })
    }
    if (field === "max_sessions") {
      return only({ maxSessions: local.fields.maxSessions ?? i18n.t("validation.maxSessionsRange", { max: 1024 }) })
    }
  }
  return { ...noProviderErrors(), form: providerErrorMessage(error) }
}
