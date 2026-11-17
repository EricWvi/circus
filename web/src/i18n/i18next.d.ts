import type { en } from "./locales/en"

// Types every `t()` key and its interpolation values against the English
// catalog, so a missing or misspelled key fails `typecheck`.
declare module "i18next" {
  interface CustomTypeOptions {
    defaultNS: "translation"
    resources: { translation: typeof en }
  }
}
