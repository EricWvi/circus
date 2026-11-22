// Input rules for model providers (proto/circus/v1/provider.proto), shared
// by the mock server and the provider form. The server is the authority;
// the form runs the same checks first for instant feedback.

import { i18n } from "@/i18n/i18n"

export const PROVIDER_NAME_MAX = 64
/** UTF-8 bytes. */
export const BASE_URL_MAX_BYTES = 2048
export const KEY_MAX_BYTES = 4096
export const MODEL_MAX_BYTES = 256
export const ENV_NAME_MAX_BYTES = 128
export const MAX_ENV_VARS = 64
/** Names and values together. */
export const MAX_ENV_BYTES = 64 * 1024
export const MAX_PROVIDER_WORKERS = 256
export const MAX_PROVIDER_SESSIONS = 1024
/** Per user. */
export const MAX_PROVIDERS = 32

/** The variables the endpoint sets; a custom variable can't have these names. */
export const ENDPOINT_VARIABLES: readonly string[] = [
  "ANTHROPIC_BASE_URL",
  "ANTHROPIC_API_KEY",
  "ANTHROPIC_AUTH_TOKEN",
  "ANTHROPIC_DEFAULT_OPUS_MODEL",
  "ANTHROPIC_DEFAULT_SONNET_MODEL",
  "ANTHROPIC_DEFAULT_HAIKU_MODEL",
]

/** Names reserved for circus and the worker's own environment. */
const RESERVED_VARIABLES: readonly string[] = [
  "CLAUDE_CONFIG_DIR",
  "HOME",
  "PATH",
  "CLAUDE_CODE_OAUTH_TOKEN",
  // The worker sets it itself when it starts the CLI with a provider.
  "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST",
]

export const utf8Length = (text: string) => new TextEncoder().encode(text).length

// C0, DEL and C1 controls (Go's unicode.IsControl).
// oxlint-disable-next-line no-control-regex
const CONTROL = /[\u0000-\u001f\u007f-\u009f]/
/** Whitespace as Go's unicode.IsSpace sees it (JavaScript's \s, plus NEL). */
const SPACE = /[\s\u0085]/

/** Why a provider name is refused, or null. The name is trimmed first. */
export function providerNameProblem(raw: string): string | null {
  const name = raw.trim()
  if (name === "") return i18n.t("validation.providerNameRequired")
  if ([...name].length > PROVIDER_NAME_MAX) return i18n.t("validation.providerNameTooLong", { max: PROVIDER_NAME_MAX })
  if (CONTROL.test(name)) return i18n.t("validation.providerNameControlChars")
  return null
}

/**
 * Why a base URL is refused, or null. Empty is fine (Claude Code's default
 * endpoint); otherwise an absolute http or https URL as written (the scheme,
 * "//" and a host), without user info, query or fragment, at most 2048
 * bytes. Spellings a browser would repair ("https:host", backslashes) are
 * refused, since the server parses the text as sent.
 */
export function baseUrlProblem(url: string): string | null {
  if (url === "") return null
  if (utf8Length(url) > BASE_URL_MAX_BYTES) return i18n.t("validation.baseUrlTooLong", { max: BASE_URL_MAX_BYTES })
  // Printable ASCII only, without spaces or backslashes: anything else is percent-encoded.
  if (!/^[\x21-\x7e]*$/.test(url) || url.includes("\\")) return i18n.t("validation.baseUrlChars")
  const scheme = /^https?:\/\//i.exec(url)
  if (!scheme) return i18n.t("validation.baseUrlInvalid")
  const authority = url.slice(scheme[0].length).split(/[/?#]/)[0]
  if (authority.includes("@")) return i18n.t("validation.baseUrlUserInfo")
  if (url.includes("?") || url.includes("#")) return i18n.t("validation.baseUrlQuery")
  if (!validAuthority(authority)) return i18n.t("validation.baseUrlInvalid")
  // Every escape is a "%" and two hex digits, as Go's url.Parse requires.
  if (/%(?![0-9A-Fa-f]{2})/.test(url)) return i18n.t("validation.baseUrlInvalid")
  try {
    if (new URL(url).hostname === "") return i18n.t("validation.baseUrlInvalid")
  } catch {
    return i18n.t("validation.baseUrlInvalid")
  }
  return null
}

/**
 * A host, optionally with a port of 1 to 65535: a name or IPv4 address
 * without escapes, or an IPv6 address in brackets.
 */
function validAuthority(authority: string): boolean {
  const m = /^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9._~-]+)(?::(\d+))?$/.exec(authority)
  if (!m) return false
  if (m[2] === undefined) return true
  const port = Number(m[2])
  return m[2].length <= 5 && port >= 1 && port <= 65535
}

/** Why a key is refused, or null. Empty is refused only when `required`. */
export function keyProblem(key: string, required: boolean): string | null {
  if (key === "") return required ? i18n.t("validation.keyRequired") : null
  if (utf8Length(key) > KEY_MAX_BYTES) return i18n.t("validation.keyTooLong", { max: KEY_MAX_BYTES })
  if (CONTROL.test(key)) return i18n.t("validation.keyChars")
  return null
}

/** Why a model id is refused, or null. Empty is fine. */
export function modelProblem(model: string): string | null {
  if (utf8Length(model) > MODEL_MAX_BYTES) return i18n.t("validation.modelTooLong", { max: MODEL_MAX_BYTES })
  if (SPACE.test(model) || CONTROL.test(model)) return i18n.t("validation.modelChars")
  return null
}

/** Why a custom variable's name is refused (on its own), or null. */
export function envNameProblem(name: string): string | null {
  if (name === "") return i18n.t("validation.envNameRequired")
  if (utf8Length(name) > ENV_NAME_MAX_BYTES) return i18n.t("validation.envNameTooLong", { max: ENV_NAME_MAX_BYTES })
  if (!/^[A-Z_][A-Z0-9_]*$/.test(name)) return i18n.t("validation.envNamePattern")
  if (name.startsWith("CIRCUS_") || RESERVED_VARIABLES.includes(name)) return i18n.t("validation.envNameReserved", { name })
  if (ENDPOINT_VARIABLES.includes(name)) return i18n.t("validation.envNameEndpoint", { name })
  return null
}

/** Why a session limit is refused, or null: 0 (unlimited) to 1024. */
export function maxSessionsProblem(max: number): string | null {
  if (!Number.isInteger(max) || max < 0 || max > MAX_PROVIDER_SESSIONS) {
    return i18n.t("validation.maxSessionsRange", { max: MAX_PROVIDER_SESSIONS })
  }
  return null
}
