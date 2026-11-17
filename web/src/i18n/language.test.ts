import { describe, expect, it } from "vitest"

import {
  LANGUAGE_STORAGE_KEY,
  readPreference,
  resolveLanguage,
  systemLanguage,
  writePreference,
} from "./language"

const storage = (values: Record<string, string> = {}) => ({
  getItem: (key: string) => values[key] ?? null,
  setItem: (key: string, value: string) => {
    values[key] = value
  },
  values,
})

const broken = {
  getItem: (): string | null => {
    throw new DOMException("blocked", "SecurityError")
  },
  setItem: (): void => {
    throw new DOMException("full", "QuotaExceededError")
  },
}

describe("systemLanguage", () => {
  it("picks Simplified Chinese for any Chinese tag", () => {
    for (const tag of ["zh", "zh-TW", "zh-Hans-CN", "zh-CN", "ZH-hk"]) {
      expect(systemLanguage([tag])).toBe("zh-CN")
    }
  })

  it("picks English for everything else", () => {
    for (const tags of [["en"], ["en-GB"], ["fr-FR"], ["ja", "de"], ["zhx"], []]) {
      expect(systemLanguage(tags)).toBe("en")
    }
  })

  it("follows the first English or Chinese entry", () => {
    expect(systemLanguage(["fr", "zh-TW", "en"])).toBe("zh-CN")
    expect(systemLanguage(["en-US", "zh-CN"])).toBe("en")
  })
})

describe("language preference", () => {
  it("wins over the browser's languages", () => {
    expect(resolveLanguage(readPreference(storage({ [LANGUAGE_STORAGE_KEY]: "en" })), ["zh-CN"])).toBe("en")
    expect(resolveLanguage(readPreference(storage({ [LANGUAGE_STORAGE_KEY]: "zh-CN" })), ["en-US"])).toBe(
      "zh-CN",
    )
  })

  it("falls back to the browser's languages for system, none or an unknown value", () => {
    expect(readPreference(storage({ [LANGUAGE_STORAGE_KEY]: "system" }))).toBe("system")
    expect(readPreference(storage())).toBe("system")
    expect(readPreference(storage({ [LANGUAGE_STORAGE_KEY]: "fr" }))).toBe("system")
    expect(resolveLanguage("system", ["zh-Hans-CN"])).toBe("zh-CN")
  })

  it("falls back to system when storage is broken", () => {
    expect(readPreference(broken)).toBe("system")
    expect(() => writePreference("en", broken)).not.toThrow()
  })

  it("stores the choice", () => {
    const s = storage()
    writePreference("zh-CN", s)
    expect(s.values[LANGUAGE_STORAGE_KEY]).toBe("zh-CN")
    expect(readPreference(s)).toBe("zh-CN")
  })
})
