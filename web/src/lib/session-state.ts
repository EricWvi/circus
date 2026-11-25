import type { SessionState } from "./types"

/** The control plane's session states. */
export type ServerSessionState = "unspecified" | "idle" | "queued" | "running" | "stopped" | "broken"

/** How the last ended turn ended. */
export type TurnOutcome = "unspecified" | "completed" | "interrupted" | "error" | "stopped"

/**
 * The state the UI shows for a session. Stopped and broken win; then what
 * the session does now: a pending approval, a running turn, or waiting to
 * run (queued, whose reason and actions matter more than how the last turn
 * ended). Only an idle session shows how its last turn ended: lost by the
 * worker (interrupted) or with an error (failed).
 */
export function viewState(session: {
  state: ServerSessionState
  lastTurnOutcome: TurnOutcome
  pendingApprovalCount: number
}): SessionState {
  switch (session.state) {
    case "stopped":
      return "stopped"
    case "broken":
      return "broken"
  }
  if (session.pendingApprovalCount > 0) return "awaiting_approval"
  if (session.state === "running") return "running"
  if (session.state === "queued") return "queued"
  if (session.lastTurnOutcome === "interrupted") return "interrupted"
  if (session.lastTurnOutcome === "error") return "failed"
  return "idle"
}

/**
 * Whether the session view adds a failure line of its own: the header says
 * the last turn failed (the server's outcome, `failed`), the stream has
 * that turn and nothing in it shows the failure (its RESULT was too large
 * to keep, or a worker-side abort the server counts as an error). The
 * header and the conversation then agree.
 */
export function showsTurnFailure(
  stream: { lastSeq: number; turnNo: number; turnFailureShown: boolean },
  session: { state: SessionState; turnCount: number },
  running: boolean,
): boolean {
  if (running || session.state !== "failed" || stream.lastSeq === 0) return false
  return stream.turnNo > 0 && stream.turnNo === session.turnCount && !stream.turnFailureShown
}

/** How long a pending approval request lasts on the server. */
export const APPROVAL_LIFETIME_MS = 10 * 60_000

/**
 * How far the browser's clock may be off the server's before it alone
 * decides that an approval expired.
 */
export const CLOCK_SKEW_MS = 5 * 60_000

/**
 * Whether an approval can no longer be decided because it is no longer
 * pending. The server's answer first: the session, read after the request
 * (its last activity, by the server's clock, is not older), counts no
 * pending approval. Otherwise the browser's clock, only once well past the
 * request's lifetime, so a clock running ahead doesn't end it early.
 */
export function approvalLapsed(
  approval: { requestedAtUnixMs: number },
  session: { pendingApprovalCount: number; lastActivityAtUnixMs: number },
  now: number,
): boolean {
  if (session.pendingApprovalCount === 0 && session.lastActivityAtUnixMs >= approval.requestedAtUnixMs) return true
  return now >= approval.requestedAtUnixMs + APPROVAL_LIFETIME_MS + CLOCK_SKEW_MS
}

/**
 * Whether a turn runs, as the view should show it (Working…, the Stop
 * button). The stream knows first when a turn starts or ends, so it decides
 * once it has delivered anything; until then the session's state does. A
 * stopped or broken session runs nothing, and a turn the server already
 * counts as over (it doesn't run, and its turn count has reached the turn)
 * is over even if its end never reached the stream: a worker that failed
 * before its result publishes none.
 */
export function turnRunning(
  stream: { lastSeq: number; turnOpen: boolean; turnNo: number },
  session: { serverState: ServerSessionState; turnCount: number },
): boolean {
  if (session.serverState === "stopped" || session.serverState === "broken") return false
  if (stream.lastSeq === 0) return session.serverState === "running"
  // Assumes one RESULT per turn: a RESULT ends the turn for the view even
  // while the server still says running (it reports the end a moment
  // later). A worker that went on after a RESULT in the same turn would
  // show as idle until its next TURN_STARTED.
  if (!stream.turnOpen) return false
  return session.serverState === "running" || session.turnCount < stream.turnNo
}
