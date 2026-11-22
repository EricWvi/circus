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

  it("space Chinese from Latin letters, digits and placeholders, but not from Chinese punctuation", () => {
    // Placeholders whose values are Chinese phrases ("3 分钟前", "1 小时后", a
    // whole sentence) join the text, listed by key so a new text using one of
    // them is checked like any other.
    const chinesePhrases: Record<string, readonly string[]> = {
      "devices.lastSeen": ["ago"],
      "devices.add.expires": ["in"],
      "folders.startFailed": ["reason"],
      "ledger.updated": ["ago"],
    }
    const han = "[\\u3400-\\u4dbf\\u4e00-\\u9fff\\uf900-\\ufaff]"
    const latin = "(?:[A-Za-z0-9]|\\{\\{\\s*([^},\\s]+)[^}]*\\}\\})"
    const touching = new RegExp(`${han}${latin}|${latin}${han}`, "g")
    const punctuation = "，。：；？！、（）“”‘’「」《》【】…—"
    const spacedPunctuation = new RegExp(`[${punctuation}] | [${punctuation}]`)
    // Component tags render no text of their own: check what is shown.
    const shown = (text: string) => text.replace(/<\/?[A-Za-z0-9]+\s*\/?>/g, "")
    const problems: string[] = []
    const used = new Set<string>()
    for (const [key, raw] of chinese) {
      const text = shown(raw)
      for (const m of text.matchAll(touching)) {
        const placeholder = m[1] ?? m[2]
        if (placeholder !== undefined && chinesePhrases[key]?.includes(placeholder)) used.add(`${key}:${placeholder}`)
        else problems.push(`${key}: ${m[0]}`)
      }
      if (spacedPunctuation.test(text)) problems.push(`${key}: space next to punctuation`)
    }
    expect(problems).toEqual([])
    // Every exemption is still needed.
    const listed = Object.entries(chinesePhrases).flatMap(([key, names]) => names.map((n) => `${key}:${n}`))
    expect(listed.filter((e) => !used.has(e))).toEqual([])
  })
})
