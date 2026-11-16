const MINUTE = 60_000
const HOUR = 60 * MINUTE
const DAY = 24 * HOUR

/** Compact relative time: "now", "5m", "3h", "2d", or a short date. */
export function formatRelative(unixMs: number, now: number = Date.now()): string {
  const diff = Math.max(0, now - unixMs)
  if (diff < MINUTE) return "now"
  if (diff < HOUR) return `${Math.floor(diff / MINUTE)}m`
  if (diff < DAY) return `${Math.floor(diff / HOUR)}h`
  if (diff < 7 * DAY) return `${Math.floor(diff / DAY)}d`
  return new Date(unixMs).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
  })
}
