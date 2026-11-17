import { createInstance } from "i18next"
import { initReactI18next } from "react-i18next"

import { languages, type Language } from "./language"
import { en } from "./locales/en"
import { zhCN } from "./locales/zh-CN"

export const resources = {
  en: { translation: en },
  "zh-CN": { translation: zhCN },
} as const satisfies Record<Language, unknown>

/**
 * The app's i18next instance. Components translate with `useTranslation`;
 * code outside React (error mappings, validation, formatting) calls
 * `i18n.t` and runs at render time, so it follows language changes too.
 */
export const i18n = createInstance()

/**
 * Initializes the instance synchronously with the bundled catalogs, so the
 * first render is already in `language`.
 */
export function initI18n(language: Language): typeof i18n {
  if (i18n.isInitialized) {
    void i18n.changeLanguage(language)
    return i18n
  }
  void i18n.use(initReactI18next).init({
    resources,
    lng: language,
    fallbackLng: "en",
    supportedLngs: languages,
    initAsync: false,
    // React escapes rendered text already.
    interpolation: { escapeValue: false },
  })
  return i18n
}

/** The active language, as the `Intl` formatters take it. */
export const currentLanguage = (): Language =>
  i18n.language === "zh-CN" ? "zh-CN" : "en"
