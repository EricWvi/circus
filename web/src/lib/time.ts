import { currentLanguage, i18n } from "@/i18n/i18n"

const MINUTE = 60_000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

/**
 * Compact relative time in the active language: "now", "5m", "3h", "2d" (in
 * Chinese 刚刚, 5分钟前, 3小时前, 2天前), and a short date after a week.
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
