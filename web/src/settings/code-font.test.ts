import { afterEach, describe, expect, it } from "vitest"

import {
  applyCodeFont,
  CJK_FALLBACK_STACK,
  CODE_FONT_STORAGE_KEY,
  codeFontStack,
  DEFAULT_CODE_FONT,
  DEFAULT_CODE_FONT_STACK,
  parseCodeFont,
  resetCodeFont,
  setCodeFont,
  setCodeFontFamily,
  setCodeFontSize,
  startCodeFont,
  type CodeFontEnvironment,
} from "./code-font"

function fakeStyle() {
  const props = new Map<string, string>()
  return {
    props,
    setProperty: (name: string, value: string) => void props.set(name, value),
    removeProperty: (name: string) => {
      const old = props.get(name) ?? ""
      props.delete(name)
      return old
    },
  }
}

function environment({ stored, broken = false }: { stored?: string; broken?: boolean } = {}) {
  const values = new Map<string, string>(stored ? [[CODE_FONT_STORAGE_KEY, stored]] : [])
  const guard = () => {
    if (broken) throw new DOMException("blocked", "SecurityError")
  }
  const style = fakeStyle()
  const env = {
    storage: {
      getItem: (key: string) => (guard(), values.get(key) ?? null),
      setItem: (key: string, value: string) => (guard(), void values.set(key, value)),
      removeItem: (key: string) => (guard(), void values.delete(key)),
    },
    events: new EventTarget(),
    style,
  } satisfies CodeFontEnvironment
  return { env, values, style: style.props }
}

const storageEvent = (key: string | null) => Object.assign(new Event("storage"), { key })

let stop = () => {}
afterEach(() => {
  resetCodeFont()
  stop()
})

describe("parseCodeFont", () => {
  it("reads stored settings", () => {
    expect(parseCodeFont('{"family":"JetBrains Mono","size":16}')).toEqual({
      family: "JetBrains Mono",
      size: 16,
    })
  })

  it("falls back to the defaults for anything unreadable", () => {
    for (const raw of [null, "", "{", "null", "42", '"x"', "[]"]) {
      expect(parseCodeFont(raw)).toEqual(DEFAULT_CODE_FONT)
    }
  })

  it("repairs each field on its own", () => {
    expect(parseCodeFont('{"family":"  Fira Code ","size":"big"}')).toEqual({
      family: "Fira Code",
      size: 14,
    })
    expect(parseCodeFont('{"family":42,"size":99}')).toEqual({ family: null, size: 24 })
    expect(parseCodeFont('{"family":"","size":3}')).toEqual({ family: null, size: 10 })
    expect(parseCodeFont('{"size":15.6}')).toEqual({ family: null, size: 16 })
  })

  it("keeps quotes and backslashes, drops only control characters", () => {
    expect(parseCodeFont(JSON.stringify({ family: `Bob's "Mono"\\ X\n` })).family).toBe(
      `Bob's "Mono"\\ X`,
    )
    expect(parseCodeFont(JSON.stringify({ family: "x".repeat(201) })).family).toBeNull()
  })
})

describe("the default stack", () => {
  const families = DEFAULT_CODE_FONT_STACK.split(", ")

  it("puts each platform's code font before the CJK fonts and the generic family", () => {
    const at = (name: string) => families.indexOf(name)
    for (const name of ["ui-monospace", "Menlo", '"Cascadia Mono"', "Consolas", '"DejaVu Sans Mono"']) {
      expect(at(name)).toBeGreaterThanOrEqual(0)
      expect(at(name)).toBeLessThan(at('"PingFang SC"'))
    }
    expect(at('"Cascadia Mono"')).toBeLessThan(at("Consolas"))
    expect(families.at(-1)).toBe("monospace")
  })

  it("names the Simplified Chinese faces, as the UI font stacks do", async () => {
    expect(DEFAULT_CODE_FONT_STACK).toContain(CJK_FALLBACK_STACK)
    // Vitest blanks CSS imports, even with ?raw, so read the file. The
    // specifier is a variable because the app's types leave out Node's.
    const fsModule = "node:fs"
    const fs = (await import(/* @vite-ignore */ fsModule)) as {
      readFileSync: (path: URL, encoding: "utf8") => string
    }
    const css = fs.readFileSync(new URL("../index.css", import.meta.url), "utf8")
    const uiStacks = [...css.matchAll(/--font-(?:sans|serif):([^;]*);/g)].map((m) => m[1])
    expect(uiStacks.length).toBeGreaterThan(0)
    for (const stack of uiStacks) expect(stack).toContain(CJK_FALLBACK_STACK)
    expect(css).toMatch(/--font-mono:\s*var\(--code-font-family/)
    expect(css).toMatch(/font-size:\s*var\(--code-font-size\)/)
  })

  it("puts a chosen family first and keeps the default behind it", () => {
    expect(codeFontStack(null)).toBe(DEFAULT_CODE_FONT_STACK)
    expect(codeFontStack("Iosevka Term")).toBe(`"Iosevka Term", ${DEFAULT_CODE_FONT_STACK}`)
    expect(codeFontStack(`Bob's "Mono"`)).toBe(`"Bob's \\"Mono\\"", ${DEFAULT_CODE_FONT_STACK}`)
  })
})

describe("applyCodeFont", () => {
  it("sets the family and leaves the default size to the stylesheet", () => {
    const style = fakeStyle()
    style.setProperty("--code-font-size", "18px")
    applyCodeFont(DEFAULT_CODE_FONT, style)
    expect(style.props.get("--code-font-family")).toBe(DEFAULT_CODE_FONT_STACK)
    expect(style.props.has("--code-font-size")).toBe(false)

    applyCodeFont({ family: "Hack", size: 18 }, style)
    expect(style.props.get("--code-font-family")).toBe(`"Hack", ${DEFAULT_CODE_FONT_STACK}`)
    expect(style.props.get("--code-font-size")).toBe("18px")
  })
})

describe("startCodeFont", () => {
  it("applies the stored settings at start", () => {
    const { env, style } = environment({ stored: '{"family":"Hack","size":12}' })
    stop = startCodeFont(env)
    expect(style.get("--code-font-family")).toBe(`"Hack", ${DEFAULT_CODE_FONT_STACK}`)
    expect(style.get("--code-font-size")).toBe("12px")
  })

  it("uses the defaults when storage is broken, and still applies changes", () => {
    const { env, style } = environment({ broken: true })
    stop = startCodeFont(env)
    expect(style.get("--code-font-family")).toBe(DEFAULT_CODE_FONT_STACK)
    expect(() => setCodeFont({ family: "Hack", size: 16 })).not.toThrow()
    expect(style.get("--code-font-size")).toBe("16px")
  })

  it("changes the family or the size alone", () => {
    const { env } = environment()
    stop = startCodeFont(env)
    setCodeFontSize(18)
    setCodeFontFamily(`Bob's "Mono"`)
    setCodeFontSize(30)
    expect(parseCodeFont(env.storage.getItem(CODE_FONT_STORAGE_KEY))).toEqual({
      family: `Bob's "Mono"`,
      size: 24,
    })
  })

  it("stores changes, and clears the entry when back to the defaults", () => {
    const { env, values } = environment()
    stop = startCodeFont(env)
    setCodeFont({ family: "Hack", size: 30 })
    expect(JSON.parse(values.get(CODE_FONT_STORAGE_KEY)!)).toEqual({ family: "Hack", size: 24 })
    resetCodeFont()
    expect(values.has(CODE_FONT_STORAGE_KEY)).toBe(false)
  })

  it("follows a change made in another tab", () => {
    const { env, values, style } = environment()
    stop = startCodeFont(env)
    values.set(CODE_FONT_STORAGE_KEY, '{"family":"Hack","size":20}')
    env.events.dispatchEvent(storageEvent("something-else"))
    expect(style.has("--code-font-size")).toBe(false)
    env.events.dispatchEvent(storageEvent(CODE_FONT_STORAGE_KEY))
    expect(style.get("--code-font-size")).toBe("20px")

    // Another tab cleared the storage.
    values.clear()
    env.events.dispatchEvent(storageEvent(null))
    expect(style.get("--code-font-family")).toBe(DEFAULT_CODE_FONT_STACK)
    expect(style.has("--code-font-size")).toBe(false)
  })
})
