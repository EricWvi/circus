// Input rules for model providers (proto/circus/v1/provider.proto), shared
// by the mock server and the provider form. The server is the authority;
// the form runs the same checks first for instant feedback.

import { i18n } from "@/i18n/i18n"

export const PROVIDER_NAME_MAX = 64
/** UTF-8 bytes. */
export const BASE_URL_MAX_BYTES = 2048
/** A host name's bytes (ASCII). */
export const HOST_MAX_BYTES = 253
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
 * bytes. Spellings a browser would repair ("https:host", backslashes,
 * unescaped "{" in the path, an international domain name) are refused,
 * since the server parses the text as sent.
 */
export function baseUrlProblem(url: string): string | null {
  if (url === "") return null
  if (utf8Length(url) > BASE_URL_MAX_BYTES) return i18n.t("validation.baseUrlTooLong", { max: BASE_URL_MAX_BYTES })
  // Printable ASCII only, without spaces or backslashes: anything else is percent-encoded.
  if (!/^[\x21-\x7e]*$/.test(url) || url.includes("\\")) {
    return nonAsciiHost(url) ? i18n.t("validation.baseUrlHostAscii") : i18n.t("validation.baseUrlChars")
  }
  const scheme = /^https?:\/\//i.exec(url)
  if (!scheme) return i18n.t("validation.baseUrlInvalid")
  const rest = url.slice(scheme[0].length)
  const authority = rest.split(/[/?#]/)[0]
  if (authority.includes("@")) return i18n.t("validation.baseUrlUserInfo")
  if (url.includes("?") || url.includes("#")) return i18n.t("validation.baseUrlQuery")
  const host = validAuthority(authority)
  if (host === null) return i18n.t("validation.baseUrlInvalid")
  const hostProblem = host.startsWith("[") ? null : hostNameProblem(host)
  if (hostProblem) return hostProblem
  const path = rest.slice(authority.length)
  // Every escape is a "%" and two hex digits, as Go's url.Parse requires.
  if (/%(?![0-9A-Fa-f]{2})/.test(path)) return i18n.t("validation.baseUrlInvalid")
  if (/["<>[\]^`{|}]/.test(path)) return i18n.t("validation.baseUrlPathChars")
  try {
    if (new URL(url).hostname === "") return i18n.t("validation.baseUrlInvalid")
  } catch {
    return i18n.t("validation.baseUrlInvalid")
  }
  return null
}

/** Whether a non-ASCII character sits in the authority of a URL with "://". */
function nonAsciiHost(url: string): boolean {
  const start = url.indexOf("://")
  if (start < 0) return false
  // oxlint-disable-next-line no-control-regex
  return /[^\x00-\x7f]/.test(url.slice(start + 3).split(/[/?#]/)[0])
}

/**
 * The host of a host, optionally with a port of 1 to 65535: a name of
 * letters, digits, ".", "-" and "_" (no escapes), or an IPv6 address in
 * brackets; null when the authority is not one.
 */
function validAuthority(authority: string): string | null {
  const m = /^(\[[0-9A-Fa-f:.]+\]|[A-Za-z0-9._-]+)(?::(\d+))?$/.exec(authority)
  if (!m) return null
  if (m[2] === undefined) return m[1]
  const port = Number(m[2])
  return m[2].length <= 5 && port >= 1 && port <= 65535 ? m[1] : null
}

/**
 * Why a host name is refused, or null: at most 253 bytes of dot-separated
 * labels of 1 to 63 bytes, neither starting nor ending with "-"; when the last label is a
 * number (decimal, or "0x" and hex digits, which URL parsers read as part
 * of an IPv4 address), the name must be an IPv4 address of four decimal
 * numbers 0 to 255 without leading zeros.
 */
function hostNameProblem(host: string): string | null {
  if (host.length > HOST_MAX_BYTES) return i18n.t("validation.baseUrlHostTooLong", { max: HOST_MAX_BYTES })
  const labels = host.split(".")
  for (const label of labels) {
    if (label === "") return i18n.t("validation.baseUrlHostEmptyLabel")
    if (label.length > 63) return i18n.t("validation.baseUrlHostLongLabel", { max: 63 })
    if (label.startsWith("-") || label.endsWith("-")) return i18n.t("validation.baseUrlHostHyphen")
  }
  const last = labels[labels.length - 1]
  if (/^(\d+|0[xX][0-9A-Fa-f]*)$/.test(last) && !isIPv4(host)) return i18n.t("validation.baseUrlHostNumeric")
  return null
}

/** Four decimal numbers 0 to 255 without leading zeros (Go's netip). */
function isIPv4(host: string): boolean {
  const parts = host.split(".")
  return parts.length === 4 && parts.every((p) => /^(0|[1-9]\d{0,2})$/.test(p) && Number(p) <= 255)
}

/**
 * Why a key is refused, or null. Empty is refused only when `required`;
 * otherwise at most 4096 bytes of printable ASCII without spaces, as an
 * HTTP header carries it.
 */
export function keyProblem(key: string, required: boolean): string | null {
  if (key === "") return required ? i18n.t("validation.keyRequired") : null
  if (utf8Length(key) > KEY_MAX_BYTES) return i18n.t("validation.keyTooLong", { max: KEY_MAX_BYTES })
  if (!/^[\x21-\x7e]*$/.test(key)) return i18n.t("validation.keyChars")
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
