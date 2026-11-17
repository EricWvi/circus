import type { SessionState } from "./types"

// Lower number = more urgent. Drives the status shown next to a collapsed
// project and the order of attention in the sidebar.
const urgency: Record<SessionState, number> = {
  awaiting_approval: 0,
  broken: 1,
  running: 2,
  queued: 3,
  interrupted: 4,
  idle: 5,
  stopped: 6,
}

/** Sessions that appear in the sidebar tree; stopped ones live on the project page. */
export function isVisibleInTree(session: { state: SessionState }): boolean {
  return session.state !== "stopped"
}

/**
 * The most urgent state among the visible sessions, or undefined when the
 * project has no visible sessions.
 */
export function rollupState(
  sessions: readonly { state: SessionState }[],
): SessionState | undefined {
  let best: SessionState | undefined
  for (const { state } of sessions) {
    if (!isVisibleInTree({ state })) continue
    if (best === undefined || urgency[state] < urgency[best]) best = state
  }
  return best
}
