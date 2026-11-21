import { isMockState, seedSessions, type MockState } from "./state"

export const STORAGE_KEY = "circus.mock"
/** Bump whenever the shape of MockState changes; older blobs are dropped. */
export const VERSION = 7

/**
 * The persisted state, or null for anything missing, stale or malformed.
 * Sessions are not persisted: a page load starts with the demo sessions of
 * the demo projects that still exist.
 */
export function parsePersisted(raw: string | null): MockState | null {
  if (!raw) return null
  try {
    const persisted: unknown = JSON.parse(raw)
    if (typeof persisted !== "object" || persisted === null) return null
    const { version, state } = persisted as { version?: unknown; state?: unknown }
    if (version !== VERSION || !isMockState(state)) return null
    return { ...state, sessions: seedSessions(state) }
  } catch {
    return null
  }
}

/** Everything but the sessions, which live in this tab's memory. */
export function serialize(state: MockState): string {
  const { sessions: _, ...persisted } = state
  return JSON.stringify({ version: VERSION, state: persisted })
}
