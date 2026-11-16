import { isMockState, type MockState } from "./state"

export const STORAGE_KEY = "circus.mock"
/** Bump whenever the shape of MockState changes; older blobs are dropped. */
export const VERSION = 3

/** The persisted state, or null for anything missing, stale or malformed. */
export function parsePersisted(raw: string | null): MockState | null {
  if (!raw) return null
  try {
    const persisted: unknown = JSON.parse(raw)
    if (typeof persisted !== "object" || persisted === null) return null
    const { version, state } = persisted as { version?: unknown; state?: unknown }
    return version === VERSION && isMockState(state) ? state : null
  } catch {
    return null
  }
}

export function serialize(state: MockState): string {
  return JSON.stringify({ version: VERSION, state })
}
