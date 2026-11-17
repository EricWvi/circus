import { describe, expect, it, vi } from "vitest"

import {
  cssFamily,
  groupFamilies,
  isFontAvailable,
  isGenericFamily,
  isMonospace,
  listLocalFonts,
  sameWidths,
  type Measure,
} from "./local-fonts"

/**
 * A fake canvas: monospace fonts give every glyph 10 units, proportional
 * ones vary by glyph; fonts not in `installed` fall back to the generic
 * family at the end of the list.
 */
function fakeMeasure(installed: Record<string, "mono" | "proportional" | "icons">): Measure {
  const generic: Record<string, "mono" | "proportional"> = {
    monospace: "mono",
    serif: "proportional",
    "sans-serif": "proportional",
  }
  const narrow = new Set("il.1")
  return (text, fontFamily) => {
    const names = fontFamily.split(",").map((n) => n.trim().replace(/^"|"$/g, ""))
    // An icon font has no letters, so they come from the next family.
    const name = names.find((n) => (n in installed && installed[n] !== "icons") || n in generic)!
    const kind = installed[name] ?? generic[name]
    // Distinct installed fonts get distinct metrics.
    const scale = name in installed ? 1.1 : name === "serif" ? 0.9 : 1
    return [...text].reduce((sum, c) => sum + (kind === "mono" ? 10 : narrow.has(c) ? 4 : 12) * scale, 0)
  }
}

describe("sameWidths", () => {
  it("allows subpixel differences only", () => {
    expect(sameWidths([9.6, 9.6, 9.61])).toBe(true)
    expect(sameWidths([4, 12])).toBe(false)
    expect(sameWidths([])).toBe(false)
    expect(sameWidths([0, 0])).toBe(false)
  })
})

describe("isMonospace", () => {
  const measure = fakeMeasure({ "JetBrains Mono": "mono", Inter: "proportional", FontAwesome: "icons" })
  it("tells monospace and proportional fonts apart", () => {
    expect(isMonospace(measure, "JetBrains Mono")).toBe(true)
    expect(isMonospace(measure, "Inter")).toBe(false)
  })
  it("doesn't count a font whose letters come from the monospace fallback", () => {
    expect(isMonospace(measure, "FontAwesome")).toBe(false)
  })
})

describe("isFontAvailable", () => {
  const measure = fakeMeasure({ "JetBrains Mono": "mono", Inter: "proportional" })
  it("finds installed fonts, monospace or not", () => {
    expect(isFontAvailable(measure, "JetBrains Mono")).toBe(true)
    expect(isFontAvailable(measure, "Inter")).toBe(true)
  })
  it("reports a missing font, which renders as the fallbacks do", () => {
    expect(isFontAvailable(measure, "No Such Font")).toBe(false)
  })
})

describe("groupFamilies", () => {
  it("deduplicates and lists monospace families first, each by name", async () => {
    const measure = fakeMeasure({ Hack: "mono", "fira code": "mono", Inter: "proportional", Arial: "proportional" })
    const names = ["Inter", "Hack", "Arial", "fira code", "Hack", " Inter ", ""]
    expect(await groupFamilies(names, measure)).toEqual([
      { family: "fira code", monospace: true },
      { family: "Hack", monospace: true },
      { family: "Arial", monospace: false },
      { family: "Inter", monospace: false },
    ])
  })

  it("pauses between chunks", async () => {
    const pause = vi.fn(() => Promise.resolve())
    const names = Array.from({ length: 100 }, (_, i) => `Font ${i}`)
    const families = await groupFamilies(names, fakeMeasure({}), pause)
    expect(families).toHaveLength(100)
    expect(pause).toHaveBeenCalledTimes(2)
  })
})

describe("isGenericFamily", () => {
  it("knows the CSS generic families and keywords", () => {
    for (const name of ["monospace", "Serif", " sans-serif ", "system-ui", "ui-monospace", "inherit"]) {
      expect(isGenericFamily(name)).toBe(true)
    }
    expect(isGenericFamily("Monaco")).toBe(false)
  })
})

describe("listLocalFonts", () => {
  const measure = fakeMeasure({ Hack: "mono" })
  it("lists the families the browser returns", async () => {
    const result = await listLocalFonts(() => Promise.resolve([{ family: "Hack" }, { family: "Hack" }]), measure)
    expect(result).toEqual({ state: "ok", families: [{ family: "Hack", monospace: true }] })
  })
  it("reads no fonts (a dismissed prompt) as denied", async () => {
    expect(await listLocalFonts(() => Promise.resolve([]), measure)).toEqual({ state: "denied" })
  })
  it("reads a rejection as denied", async () => {
    const refuse = () => Promise.reject(new DOMException("no", "NotAllowedError"))
    expect(await listLocalFonts(refuse, measure)).toEqual({ state: "denied" })
  })
  it("is unsupported without the API or a canvas", async () => {
    expect(await listLocalFonts(null, measure)).toEqual({ state: "unsupported" })
    expect(await listLocalFonts(() => Promise.resolve([]), null)).toEqual({ state: "unsupported" })
  })
})

describe("cssFamily", () => {
  it("quotes and escapes", () => {
    expect(cssFamily('A "B" \\C')).toBe('"A \\"B\\" \\\\C"')
  })
})
