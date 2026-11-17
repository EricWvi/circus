import { describe, expect, it } from "vitest"

import { en } from "./locales/en"
import { zhCN } from "./locales/zh-CN"

type Tree = { readonly [key: string]: string | Tree }

/** Every key path with its text, e.g. "auth.errors.generic". */
function flatten(tree: Tree, prefix = ""): Map<string, string> {
  const out = new Map<string, string>()
  for (const [key, value] of Object.entries(tree)) {
    const path = prefix ? `${prefix}.${key}` : key
    if (typeof value === "string") out.set(path, value)
    else for (const [k, v] of flatten(value, path)) out.set(k, v)
  }
  return out
}

/** The `{{name}}` placeholders and `<tag>` components a text uses. */
function placeholders(text: string): string[] {
  const names = [...text.matchAll(/\{\{\s*([^},\s]+)[^}]*\}\}/g)].map((m) => `{{${m[1]}}}`)
  const tags = [...text.matchAll(/<\/?([A-Za-z0-9]+)\s*\/?>/g)].map((m) => `<${m[0].startsWith("</") ? "/" : ""}${m[1]}>`)
  return [...names, ...tags].sort()
}

describe("catalogs", () => {
  const source = flatten(en)
  const chinese = flatten(zhCN)

  it("have the same keys", () => {
    expect([...chinese.keys()].sort()).toEqual([...source.keys()].sort())
  })

  it("use the same placeholders and components for each key", () => {
    for (const [key, text] of source) {
      expect({ key, placeholders: placeholders(chinese.get(key) ?? "") }).toEqual({
        key,
        placeholders: placeholders(text),
      })
    }
  })

  it("have no empty texts", () => {
    for (const [key, text] of [...source, ...chinese]) expect({ key, empty: text.trim() === "" }).toEqual({ key, empty: false })
  })
})
