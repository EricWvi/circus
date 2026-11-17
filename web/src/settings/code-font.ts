// The font and size of code: Markdown code blocks and inline code, tool
// inputs and outputs, the approval card's command — everything styled with
// `font-mono`, `pre` or `code`. Stored per browser, because the installed
// fonts differ from device to device.

import { useSyncExternalStore } from "react"

import { cssFamily } from "./local-fonts"

export const CODE_FONT_STORAGE_KEY = "circus-code-font"

/** The code size today's look uses (Tailwind `text-sm` at a 16px root). */
export const DEFAULT_CODE_FONT_SIZE = 14
export const MIN_CODE_FONT_SIZE = 10
export const MAX_CODE_FONT_SIZE = 24

/** Longest family name taken from storage or typed in. */
const MAX_FAMILY_LENGTH = 200

/**
 * Chinese UI fonts per platform, for text the Latin font has no glyphs for:
 * macOS (PingFang SC, Hiragino Sans GB on older versions), Windows
 * (Microsoft YaHei), Linux (Noto Sans CJK SC, or its Adobe twin Source Han
 * Sans SC). Naming the SC faces keeps Han characters in Simplified Chinese
 * forms even on English pages, where `lang` would make the browser pick a
 * Japanese face.
 */
export const CJK_FALLBACK_STACK =
  '"PingFang SC", "Hiragino Sans GB", "Microsoft YaHei", "Noto Sans CJK SC", "Source Han Sans SC"'

/**
 * Each platform's own code font. Names that only exist on one platform
 * come first, so no platform picks up another's font by accident:
 * `ui-monospace` (Safari: SF Mono), SF Mono by name (macOS with it
 * installed), Menlo (every macOS), Cascadia Mono (Windows 11), Consolas
 * (every Windows), then the Linux families, of which a desktop usually
 * has at least one. Chinese falls through to the CJK UI fonts, and
 * `monospace` is the last resort.
 */
export const DEFAULT_CODE_FONT_STACK = [
  "ui-monospace",
  '"SF Mono"',
  "SFMono-Regular",
  "Menlo",
  '"Cascadia Mono"',
  "Consolas",
  '"DejaVu Sans Mono"',
  '"Liberation Mono"',
  '"Noto Sans Mono"',
  CJK_FALLBACK_STACK,
  "monospace",
].join(", ")

export interface CodeFontSettings {
  /** A font family name; null is the platform default. */
  family: string | null
  /** Pixels, a whole number within MIN_CODE_FONT_SIZE..MAX_CODE_FONT_SIZE. */
  size: number
}

export const DEFAULT_CODE_FONT: CodeFontSettings = { family: null, size: DEFAULT_CODE_FONT_SIZE }

export const isDefaultCodeFont = (s: CodeFontSettings) =>
  s.family === null && s.size === DEFAULT_CODE_FONT_SIZE

/**
 * A family name as stored: trimmed, without control characters, and not
 * overly long; null when nothing usable is left. Quotes and backslashes
 * stay (some fonts have them); `cssFamily` escapes them for CSS.
 */
export function normalizeFamily(value: unknown): string | null {
  if (typeof value !== "string") return null
  // oxlint-disable-next-line no-control-regex -- stripping them is the point
  const clean = value.replace(/[\u0000-\u001f\u007f]/g, "").trim()
  if (clean === "" || clean.length > MAX_FAMILY_LENGTH) return null
  return clean
}

export const clampCodeFontSize = (size: number) =>
  Math.min(MAX_CODE_FONT_SIZE, Math.max(MIN_CODE_FONT_SIZE, Math.round(size)))

/** Settings from their stored JSON; anything unreadable is the default. */
export function parseCodeFont(raw: string | null): CodeFontSettings {
  if (raw === null) return DEFAULT_CODE_FONT
  let value: unknown
  try {
    value = JSON.parse(raw)
  } catch {
    return DEFAULT_CODE_FONT
  }
  if (typeof value !== "object" || value === null) return DEFAULT_CODE_FONT
  const { family, size } = value as Record<string, unknown>
  return {
    family: normalizeFamily(family),
    size:
      typeof size === "number" && Number.isFinite(size)
        ? clampCodeFontSize(size)
        : DEFAULT_CODE_FONT_SIZE,
  }
}

/** The CSS font-family list for a chosen family (or the default). */
export function codeFontStack(family: string | null): string {
  return family === null ? DEFAULT_CODE_FONT_STACK : `${cssFamily(family)}, ${DEFAULT_CODE_FONT_STACK}`
}

/** Where the settings take effect: the root element's inline style. */
export interface StyleTarget {
  setProperty(name: string, value: string): void
  removeProperty(name: string): string
}

/**
 * Sets the CSS variables that index.css hands to every code area. An
 * untouched size leaves `--code-font-size` to the stylesheet (0.875rem),
 * so the default follows the browser's base font size as before.
 */
export function applyCodeFont(s: CodeFontSettings, style: StyleTarget): void {
  style.setProperty("--code-font-family", codeFontStack(s.family))
  if (s.size === DEFAULT_CODE_FONT_SIZE) style.removeProperty("--code-font-size")
  else style.setProperty("--code-font-size", `${s.size}px`)
}

/** What the settings read from and write to; the browser by default. */
export interface CodeFontEnvironment {
  storage: Pick<Storage, "getItem" | "setItem" | "removeItem">
  /** Receives `storage` events. */
  events: EventTarget
  style: StyleTarget
}

const browserEnvironment = (): CodeFontEnvironment => ({
  storage: {
    getItem: (key) => localStorage.getItem(key),
    setItem: (key, value) => localStorage.setItem(key, value),
    removeItem: (key) => localStorage.removeItem(key),
  },
  events: window,
  style: document.documentElement.style,
})

let env: CodeFontEnvironment | null = null
let current: CodeFontSettings = DEFAULT_CODE_FONT
const listeners = new Set<() => void>()

function read(storage: CodeFontEnvironment["storage"]): CodeFontSettings {
  try {
    return parseCodeFont(storage.getItem(CODE_FONT_STORAGE_KEY))
  } catch {
    // Storage can be unavailable (private mode, blocked site data).
    return DEFAULT_CODE_FONT
  }
}

function show(next: CodeFontSettings) {
  current = next
  if (env) applyCodeFont(next, env.style)
  for (const listener of listeners) listener()
}

/**
 * Reads the stored settings and applies them. Runs before the first render,
 * so code never shows in the wrong font first. Afterwards it follows
 * changes made in other tabs. Returns a function that stops following.
 */
export function startCodeFont(environment: CodeFontEnvironment = browserEnvironment()): () => void {
  env = environment
  show(read(environment.storage))
  const onStorage = (event: Event) => {
    const key = (event as StorageEvent).key
    // A null key means another tab cleared the storage.
    if (key === CODE_FONT_STORAGE_KEY || key === null) show(read(environment.storage))
  }
  environment.events.addEventListener("storage", onStorage)
  return () => {
    environment.events.removeEventListener("storage", onStorage)
    if (env === environment) env = null
  }
}

/** Applies and stores new settings; values out of range are corrected. */
export function setCodeFont(next: CodeFontSettings): void {
  const settings: CodeFontSettings = {
    family: normalizeFamily(next.family),
    size: clampCodeFontSize(next.size),
  }
  if (env) {
    try {
      if (isDefaultCodeFont(settings)) env.storage.removeItem(CODE_FONT_STORAGE_KEY)
      else env.storage.setItem(CODE_FONT_STORAGE_KEY, JSON.stringify(settings))
    } catch {
      // Ignore; the choice still applies for this page load.
    }
  }
  show(settings)
}

export const resetCodeFont = () => setCodeFont(DEFAULT_CODE_FONT)

/** Changes only the family, keeping the size. */
export const setCodeFontFamily = (family: string | null) => setCodeFont({ ...current, family })

/** Changes only the size, keeping the family. */
export const setCodeFontSize = (size: number) => setCodeFont({ ...current, size })

const subscribe = (listener: () => void) => {
  listeners.add(listener)
  return () => listeners.delete(listener)
}

export function useCodeFont(): CodeFontSettings {
  return useSyncExternalStore(subscribe, () => current, () => current)
}
