import { useSyncExternalStore } from "react"

import { i18n, initI18n } from "./i18n"
import {
  browserLanguages,
  LANGUAGE_STORAGE_KEY,
  readPreference,
  resolveLanguage,
  writePreference,
  type LanguagePreference,
} from "./language"

/** What the language choice reads from and writes to; the browser by default. */
export interface I18nEnvironment {
  storage: Pick<Storage, "getItem" | "setItem">
  /** The browser's preferred languages, most preferred first. */
  languages: () => readonly string[]
  /** Receives `languagechange` and `storage` events. */
  events: EventTarget
  document: { documentElement: { lang: string }; title: string }
}

const browserEnvironment = (): I18nEnvironment => ({
  storage: {
    getItem: (key) => localStorage.getItem(key),
    setItem: (key, value) => localStorage.setItem(key, value),
  },
  languages: browserLanguages,
  events: window,
  document,
})

let env: I18nEnvironment | null = null
let preference: LanguagePreference = "system"
const listeners = new Set<() => void>()

function apply(next: LanguagePreference) {
  preference = next
  for (const listener of listeners) listener()
  if (env) void i18n.changeLanguage(resolveLanguage(next, env.languages()))
}

/**
 * Picks the language from the stored preference (or the browser) and
 * initializes i18n with it. Runs before the first render, so the page never
 * shows the other language first. Afterwards it follows the browser's
 * languages while on "system", and a choice made in another tab. Returns a
 * function that stops following.
 */
export function startI18n(environment: I18nEnvironment = browserEnvironment()): () => void {
  env = environment
  const { document, events } = environment
  preference = readPreference(environment.storage)
  initI18n(resolveLanguage(preference, environment.languages()))

  const syncDocument = () => {
    document.documentElement.lang = i18n.language
    document.title = i18n.t("app.name")
  }
  syncDocument()
  i18n.on("languageChanged", syncDocument)

  const onLanguageChange = () => {
    if (preference === "system") apply("system")
  }
  const onStorage = (event: Event) => {
    const key = (event as StorageEvent).key
    // A null key means another tab cleared the storage.
    if (key === LANGUAGE_STORAGE_KEY || key === null) apply(readPreference(environment.storage))
  }
  events.addEventListener("languagechange", onLanguageChange)
  events.addEventListener("storage", onStorage)

  return () => {
    i18n.off("languageChanged", syncDocument)
    events.removeEventListener("languagechange", onLanguageChange)
    events.removeEventListener("storage", onStorage)
    if (env === environment) env = null
  }
}

export function setLanguagePreference(next: LanguagePreference): void {
  if (env) writePreference(next, env.storage)
  apply(next)
}

const subscribe = (listener: () => void) => {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

/** The language preference, as the switch shows it. */
export function useLanguagePreference(): LanguagePreference {
  return useSyncExternalStore(subscribe, () => preference)
}
