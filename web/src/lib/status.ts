import type { Session, SessionState } from "./types"

export const sessionStateLabels: Record<SessionState, string> = {
  running: "Running",
  awaiting_approval: "Needs approval",
  queued: "Queued",
  idle: "Idle",
  interrupted: "Interrupted",
  broken: "Broken",
  stopped: "Stopped",
}

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
export function isVisibleInTree(session: Pick<Session, "state">): boolean {
  return session.state !== "stopped"
}

/**
 * The most urgent state among the visible sessions, or undefined when the
 * project has no visible sessions.
 */
export function rollupState(
  sessions: readonly Pick<Session, "state">[],
): SessionState | undefined {
  let best: SessionState | undefined
  for (const { state } of sessions) {
    if (!isVisibleInTree({ state })) continue
    if (best === undefined || urgency[state] < urgency[best]) best = state
  }
  return best
}
