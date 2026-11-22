import { isMockState, seedProviders, seedSessions, seedWorkers, type MockState } from "./state"

export const STORAGE_KEY = "circus.mock"
/** Bump whenever the shape of MockState changes; older blobs are dropped. */
export const VERSION = 7

/**
 * The persisted state, or null for anything missing, stale or malformed.
 * Sessions, model providers and workers are not persisted: a page load
 * starts with the demo ones (sessions of the demo projects that still exist).
 */
export function parsePersisted(raw: string | null): MockState | null {
  if (!raw) return null
  try {
    const persisted: unknown = JSON.parse(raw)
    if (typeof persisted !== "object" || persisted === null) return null
    const { version, state } = persisted as { version?: unknown; state?: unknown }
    if (version !== VERSION || !isMockState(state)) return null
    const loaded = state as Omit<MockState, "sessions" | "providers" | "workers">
    return { ...loaded, sessions: seedSessions(loaded), providers: seedProviders(), workers: seedWorkers() }
  } catch {
    return null
  }
}

/** Everything but the sessions, providers and workers, which live in this tab's memory. */
export function serialize(state: MockState): string {
  const { sessions: _, providers: __, workers: ___, ...persisted } = state
  return JSON.stringify({ version: VERSION, state: persisted })
}
