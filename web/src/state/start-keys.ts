// The request key of a session start, per project, until the start
// succeeds. A start that failed and is sent again reuses the key, so the
// server creates its session once even if the failed request got through;
// the key outlives the project page, so this holds after leaving the page
// and coming back too. The server matches a retry by location index only,
// and provider, so a start in another location, or in a different location
// now at the same index (edited elsewhere meanwhile), or with another
// provider, takes a key of its own.

import { randomUuid } from "@/lib/uuid"

interface StartKey {
  key: string
  locationIndex: number
  /** The location's device and path, and the provider asked for. */
  location: string
}

const pending = new Map<string, StartKey>()

/**
 * The key for starting a session at `location`, the one at `locationIndex`
 * of the project, with `providerId` as the request names it (undefined:
 * the default).
 */
export function startKey(
  projectId: string,
  locationIndex: number,
  location: { device: string; path: string },
  providerId?: string,
): string {
  const where = JSON.stringify([location.device, location.path, providerId ?? null])
  const current = pending.get(projectId)
  if (current && current.locationIndex === locationIndex && current.location === where) return current.key
  const next = { key: randomUuid(), locationIndex, location: where }
  pending.set(projectId, next)
  return next.key
}

/**
 * The start is over (its session exists) or its key can't be used again
 * (the server says it belongs to another request): the next start is a new one.
 */
export function forgetStartKey(projectId: string): void {
  pending.delete(projectId)
}

/** Forgets every key; for tests. */
export function forgetAllStartKeys(): void {
  pending.clear()
}
