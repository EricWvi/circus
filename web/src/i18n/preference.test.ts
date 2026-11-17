import { afterEach, describe, expect, it } from "vitest"

import { i18n } from "./i18n"
import { LANGUAGE_STORAGE_KEY } from "./language"
import { setLanguagePreference, startI18n, type I18nEnvironment } from "./preference"

function environment({
  stored,
  languages = ["en-US"],
  broken = false,
}: { stored?: string; languages?: string[]; broken?: boolean } = {}) {
  const values = new Map<string, string>(stored ? [[LANGUAGE_STORAGE_KEY, stored]] : [])
  const env = {
    storage: {
      getItem: (key: string) => {
        if (broken) throw new DOMException("blocked", "SecurityError")
        return values.get(key) ?? null
      },
      setItem: (key: string, value: string) => {
        if (broken) throw new DOMException("blocked", "SecurityError")
        values.set(key, value)
      },
    },
    languages: () => languages,
    events: new EventTarget(),
    document: { documentElement: { lang: "" }, title: "" },
  } satisfies I18nEnvironment
  return { env, values, setLanguages: (next: string[]) => (languages = next) }
}

let stop = () => {}
const start = (env: I18nEnvironment) => {
  stop = startI18n(env)
}

afterEach(async () => {
  stop()
  await i18n.changeLanguage("en")
})

/** A `storage` event as another tab's write fires it. */
const storageEvent = (key: string | null) => Object.assign(new Event("storage"), { key })

describe("startI18n", () => {
  it("maps the browser's languages on system", () => {
    const { env } = environment({ languages: ["zh-Hans-CN", "en"] })
    start(env)
    expect(i18n.language).toBe("zh-CN")
    expect(env.document.documentElement.lang).toBe("zh-CN")
    expect(env.document.title).toBe("Circus")
  })

  it("lets the stored preference win over the browser", () => {
    const { env } = environment({ stored: "en", languages: ["zh-CN"] })
    start(env)
    expect(i18n.language).toBe("en")
  })

  it("falls back to the browser when storage is broken", () => {
    const { env } = environment({ broken: true, languages: ["zh-TW"] })
    start(env)
    expect(i18n.language).toBe("zh-CN")
    expect(() => setLanguagePreference("en")).not.toThrow()
    expect(i18n.language).toBe("en")
  })

  it("follows the browser's languages only while on system", () => {
    const { env, setLanguages } = environment({ languages: ["en"] })
    start(env)
    setLanguages(["zh"])
    env.events.dispatchEvent(new Event("languagechange"))
    expect(i18n.language).toBe("zh-CN")

    setLanguagePreference("en")
    setLanguages(["zh-CN"])
    env.events.dispatchEvent(new Event("languagechange"))
    expect(i18n.language).toBe("en")
  })

  it("follows a choice made in another tab", () => {
    const { env, values } = environment({ languages: ["en"] })
    start(env)
    values.set(LANGUAGE_STORAGE_KEY, "zh-CN")
    env.events.dispatchEvent(storageEvent("unrelated"))
    expect(i18n.language).toBe("en")
    env.events.dispatchEvent(storageEvent(LANGUAGE_STORAGE_KEY))
    expect(i18n.language).toBe("zh-CN")
    expect(env.document.documentElement.lang).toBe("zh-CN")
  })
})

describe("setLanguagePreference", () => {
  it("stores the choice and applies it", () => {
    const { env, values } = environment({ languages: ["en"] })
    start(env)
    setLanguagePreference("zh-CN")
    expect(values.get(LANGUAGE_STORAGE_KEY)).toBe("zh-CN")
    expect(i18n.language).toBe("zh-CN")
    setLanguagePreference("system")
    expect(values.get(LANGUAGE_STORAGE_KEY)).toBe("system")
    expect(i18n.language).toBe("en")
  })
})
