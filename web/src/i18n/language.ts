// Which language the UI uses: a stored preference, or the browser's
// languages when the preference is "system".

export const languages = ["en", "zh-CN"] as const
export type Language = (typeof languages)[number]
export type LanguagePreference = "system" | Language

export const LANGUAGE_STORAGE_KEY = "circus-language"

/** Each language's name in that language, as the switch shows it; never translated. */
export const languageNames: Record<Language, string> = {
  en: "English",
  "zh-CN": "中文",
}

const isPreference = (value: unknown): value is LanguagePreference =>
  value === "system" || languages.includes(value as Language)

/**
 * The language for the browser's preferred languages, most preferred first:
 * the first entry that is English or Chinese decides (any `zh` variant reads
 * as Simplified Chinese); without one, English.
 */
export function systemLanguage(preferred: readonly string[]): Language {
  for (const tag of preferred) {
    const lower = tag.toLowerCase()
    if (lower === "zh" || lower.startsWith("zh-")) return "zh-CN"
    if (lower === "en" || lower.startsWith("en-")) return "en"
  }
  return "en"
}

/** The browser's preferred languages, or none outside a browser. */
export function browserLanguages(): readonly string[] {
  if (typeof navigator === "undefined") return []
  if (navigator.languages?.length) return navigator.languages
  return navigator.language ? [navigator.language] : []
}

export function resolveLanguage(
  preference: LanguagePreference,
  preferred: readonly string[],
): Language {
  return preference === "system" ? systemLanguage(preferred) : preference
}

/** The stored preference; "system" when there is none or storage is unavailable. */
export function readPreference(storage?: Pick<Storage, "getItem">): LanguagePreference {
  try {
    const value = (storage ?? localStorage).getItem(LANGUAGE_STORAGE_KEY)
    if (isPreference(value)) return value
  } catch {
    // Storage can be unavailable (private mode, blocked site data).
  }
  return "system"
}

export function writePreference(
  preference: LanguagePreference,
  storage?: Pick<Storage, "setItem">,
): void {
  try {
    ;(storage ?? localStorage).setItem(LANGUAGE_STORAGE_KEY, preference)
  } catch {
    // Ignore; the choice still applies for this page load.
  }
}
