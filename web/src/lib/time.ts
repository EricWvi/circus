import { currentLanguage, i18n } from "@/i18n/i18n"

const MINUTE = 60_000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

const relativeFormats = new Map<string, Intl.RelativeTimeFormat>()

/**
 * One formatter per language and style, made on first use: lists format
 * their times on every refresh, and making a formatter costs far more than
 * using one.
 */
function relativeFormat(language: string, options: Intl.RelativeTimeFormatOptions): Intl.RelativeTimeFormat {
  const key = `${language} ${options.style ?? "long"} ${options.numeric ?? "always"}`
  let format = relativeFormats.get(key)
  if (!format) {
    format = new Intl.RelativeTimeFormat(language, options)
    relativeFormats.set(key, format)
  }
  return format
}

/**
 * Compact relative time in the active language: "now", "5m", "3h", "2d" (in
 * Chinese 刚刚, 5 分钟前, 3 小时前, 2 天前), and a short date after a week.
 */
export function formatRelative(
  unixMs: number,
  now: number = Date.now(),
  language: string = currentLanguage(),
): string {
  const diff = Math.max(0, now - unixMs)
  const lng = language
  if (diff < MINUTE) return i18n.t("time.now", { lng })
  if (diff < HOUR) return i18n.t("time.minutes", { n: Math.floor(diff / MINUTE), lng })
  if (diff < DAY) return i18n.t("time.hours", { n: Math.floor(diff / HOUR), lng })
  if (diff < 7 * DAY) return i18n.t("time.days", { n: Math.floor(diff / DAY), lng })
  return new Date(unixMs).toLocaleDateString(language, { month: "short", day: "numeric" })
}

/**
 * How long ago, as a phrase for a sentence: "just now", "5m ago", "2h ago",
 * "yesterday" (in Chinese 刚刚, 5 分钟前, 2 小时前, 昨天).
 */
export function formatTimeAgo(
  unixMs: number,
  now: number = Date.now(),
  language: string = currentLanguage(),
): string {
  const diff = Math.max(0, now - unixMs)
  if (diff < MINUTE) return i18n.t("time.justNow", { lng: language })
  const format = relativeFormat(language, { style: "narrow", numeric: "auto" })
  if (diff < HOUR) return spaceDigits(format.format(-Math.floor(diff / MINUTE), "minute"))
  if (diff < DAY) return spaceDigits(format.format(-Math.floor(diff / HOUR), "hour"))
  return spaceDigits(format.format(-Math.floor(diff / DAY), "day"))
}

/** A full date and time for hover text: "Nov 15, 2026, 12:00 PM" (2026年11月15日 12:00). */
export function formatDateTime(unixMs: number, language: string = currentLanguage()): string {
  return new Intl.DateTimeFormat(language, { dateStyle: "medium", timeStyle: "short" }).format(unixMs)
}

/**
 * How long until a future moment, as a phrase for a sentence: "in 59
 * minutes", "in 1 hour" (in Chinese 59 分钟后, 1 小时后). Minutes round up, so
 * the phrase never reaches zero before the moment itself.
 */
export function formatTimeUntil(
  unixMs: number,
  now: number = Date.now(),
  language: string = currentLanguage(),
): string {
  const minutes = Math.max(1, Math.ceil((unixMs - now) / MINUTE))
  const format = relativeFormat(language, { numeric: "always" })
  if (minutes < 60) return spaceDigits(format.format(minutes, "minute"))
  if (minutes < 24 * 60) return spaceDigits(format.format(Math.floor(minutes / 60), "hour"))
  return spaceDigits(format.format(Math.floor(minutes / (24 * 60)), "day"))
}

/**
 * Puts a space between digits and Chinese characters, as the Chinese UI text
 * does: Intl writes "5分钟前", the catalog "5 分钟前".
 */
function spaceDigits(text: string): string {
  return text.replace(/(\d)(?=[\u4e00-\u9fff])/g, "$1 ").replace(/([\u4e00-\u9fff])(?=\d)/g, "$1 ")
}

/**
 * A time of day for a status line: "14:05" today, with the date before it
 * on another day ("Nov 20, 14:05"; in Chinese 11 月 20 日 14:05).
 */
export function formatClock(unixMs: number, now: number = Date.now(), language: string = currentLanguage()): string {
  const sameDay = new Date(unixMs).toDateString() === new Date(now).toDateString()
  const options: Intl.DateTimeFormatOptions = { hour: "2-digit", minute: "2-digit", hourCycle: "h23" }
  if (!sameDay) Object.assign(options, { month: "short", day: "numeric" })
  return spaceDigits(new Intl.DateTimeFormat(language, options).format(unixMs))
}
