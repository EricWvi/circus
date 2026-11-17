// Finding the fonts installed on this device, and telling monospace ones
// apart, for the code font setting.

/** Width of `text` set in a CSS font-family list. */
export type Measure = (text: string, fontFamily: string) => number

/** Narrow, wide and punctuation glyphs: equal widths only in monospace fonts. */
const MONOSPACE_PROBES = ["i", "W", "m", "l", ".", "0"]

/** Glyphs whose widths differ between almost any two fonts. */
const AVAILABILITY_PROBE = "mmmmmmmmmmlli1WQ@#&%"

/** Fallbacks to measure against: a font that is present changes at least one. */
const GENERIC_FAMILIES = ["monospace", "serif", "sans-serif"]

/** Whether all widths are equal, allowing for subpixel rounding. */
export function sameWidths(widths: number[], tolerance = 0.02): boolean {
  if (widths.length === 0 || widths.some((w) => !(w > 0))) return false
  const min = Math.min(...widths)
  const max = Math.max(...widths)
  return max - min <= min * tolerance
}

/** A family name quoted as a CSS string. */
export const cssFamily = (family: string) => `"${family.replace(/["\\]/g, "\\$&")}"`

/**
 * Whether `family` sets the probe glyphs at one width. Each glyph is
 * measured with two different fallbacks behind the font: if the widths
 * differ, the font lacks that glyph (an icon or symbol font) and the
 * fallback drew it, so it is not a monospace text font.
 */
export function isMonospace(measure: Measure, family: string): boolean {
  const quoted = cssFamily(family)
  const own = MONOSPACE_PROBES.map((glyph) => measure(glyph, `${quoted}, monospace`))
  const viaSerif = MONOSPACE_PROBES.map((glyph) => measure(glyph, `${quoted}, serif`))
  if (own.some((width, i) => width !== viaSerif[i])) return false
  return sameWidths(own)
}

/**
 * Whether `family` is installed. `document.fonts.check` says yes for any
 * name that no web font claims, so measure instead: a missing family falls
 * back to the generic family after it, so its text is exactly as wide as
 * the generic family's alone, for every generic family tried.
 */
export function isFontAvailable(measure: Measure, family: string): boolean {
  const quoted = cssFamily(family)
  return GENERIC_FAMILIES.some(
    (generic) =>
      measure(AVAILABILITY_PROBE, `${quoted}, ${generic}`) !== measure(AVAILABILITY_PROBE, generic),
  )
}

/** Measures on a canvas; null outside a browser. */
export function canvasMeasure(): Measure | null {
  if (typeof document === "undefined") return null
  const context = document.createElement("canvas").getContext("2d")
  if (!context) return null
  return (text, fontFamily) => {
    context.font = `32px ${fontFamily}`
    return context.measureText(text).width
  }
}

/** One font family on this device. */
export interface LocalFamily {
  family: string
  monospace: boolean
}

/** Families measured between two pauses, so the page stays responsive. */
const CLASSIFY_CHUNK = 40

const nextTask = () => new Promise<void>((resolve) => setTimeout(resolve, 0))

/**
 * Families, unique and sorted: monospace ones first, then by name. Measures
 * in chunks and yields between them, as a device can have hundreds.
 */
export async function groupFamilies(
  names: Iterable<string>,
  measure: Measure,
  pause: () => Promise<void> = nextTask,
): Promise<LocalFamily[]> {
  const unique = [...new Set([...names].map((n) => n.trim()).filter(Boolean))]
  const families: LocalFamily[] = []
  for (let i = 0; i < unique.length; i += CLASSIFY_CHUNK) {
    if (i > 0) await pause()
    for (const family of unique.slice(i, i + CLASSIFY_CHUNK)) {
      families.push({ family, monospace: isMonospace(measure, family) })
    }
  }
  return families.sort(
    (a, b) =>
      Number(b.monospace) - Number(a.monospace) ||
      a.family.localeCompare(b.family, undefined, { sensitivity: "base" }),
  )
}

/** CSS generic families and keywords: valid in a stack, but not font names. */
const GENERIC_KEYWORDS = new Set([
  "serif",
  "sans-serif",
  "monospace",
  "cursive",
  "fantasy",
  "math",
  "emoji",
  "fangsong",
  "system-ui",
  "ui-serif",
  "ui-sans-serif",
  "ui-monospace",
  "ui-rounded",
  "inherit",
  "initial",
  "unset",
  "revert",
  "revert-layer",
  "default",
])

export const isGenericFamily = (name: string) => GENERIC_KEYWORDS.has(name.trim().toLowerCase())

/** The part of the Local Font Access API this uses (Chromium only). */
export type QueryLocalFonts = () => Promise<{ family: string }[]>

const queryLocalFonts = (): QueryLocalFonts | null => {
  if (typeof window === "undefined") return null
  const query = (window as unknown as { queryLocalFonts?: QueryLocalFonts }).queryLocalFonts
  return typeof query === "function" ? query.bind(window) : null
}

export const canListLocalFonts = () => queryLocalFonts() !== null

export type LocalFontsResult =
  | { state: "ok"; families: LocalFamily[] }
  | { state: "unsupported" }
  | { state: "denied" }

/**
 * Asks the browser for the installed fonts. Must run in a user gesture; the
 * browser may ask for permission first. `query` and `measure` default to
 * the browser's.
 */
export async function listLocalFonts(
  query: QueryLocalFonts | null = queryLocalFonts(),
  measure: Measure | null = canvasMeasure(),
): Promise<LocalFontsResult> {
  if (!query || !measure) return { state: "unsupported" }
  let fonts: { family: string }[]
  try {
    fonts = await query()
  } catch {
    // NotAllowedError (permission refused) or SecurityError (no gesture,
    // blocked by policy).
    return { state: "denied" }
  }
  // Chromium answers with no fonts at all, rather than an error, when the
  // prompt is dismissed or can't be shown; a real device always has some.
  if (fonts.length === 0) return { state: "denied" }
  return { state: "ok", families: await groupFamilies(fonts.map((f) => f.family), measure) }
}
