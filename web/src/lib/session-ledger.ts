// What the session page shows of the session's ledger snapshot: the
// header entry's state, whether the changes can be merged, why a merge was
// refused. Pure, apart from the i18n lookups; line diffs are in
// `line-diff.ts`.

import { isApiError, errorMessage } from "@/api/errors"
import type { LedgerSnapshotError, SessionLedger, SessionLedgerStatus } from "@/api/types"
import { i18n } from "@/i18n/i18n"

/**
 * How the header entry shows the snapshot: hidden without one, muted while
 * it is being prepared or when it failed, with the number of changes once
 * ready (marked when there are problems).
 */
export type LedgerEntryView =
  | { kind: "none" }
  | { kind: "preparing" }
  | { kind: "failed"; reason: string }
  | { kind: "ready"; changes: number; problems: boolean }

export function ledgerEntryView(status: SessionLedgerStatus | undefined): LedgerEntryView {
  switch (status?.state) {
    case undefined:
    case "unspecified":
      return { kind: "none" }
    case "pending":
    case "preparing":
      return { kind: "preparing" }
    case "failed":
      return { kind: "failed", reason: snapshotFailure(status) }
    case "ready":
      return { kind: "ready", changes: status.changeCount, problems: status.problemCount > 0 }
  }
}

const snapshotErrorKeys = {
  unspecified: "sessionLedger.error.unspecified",
  invalid_request: "sessionLedger.error.invalid_request",
  state_dir_unknown: "sessionLedger.error.state_dir_unknown",
  unsupported_os: "sessionLedger.error.unsupported_os",
  path_conflict: "sessionLedger.error.path_conflict",
  permission_denied: "sessionLedger.error.permission_denied",
  no_space: "sessionLedger.error.no_space",
  absent: "sessionLedger.error.absent",
  stale: "sessionLedger.error.stale",
  busy: "sessionLedger.error.busy",
  io: "sessionLedger.error.io",
  timeout: "sessionLedger.error.timeout",
} as const satisfies Record<LedgerSnapshotError, string>

/** Why the snapshot failed, in one sentence. The device's detail is separate. */
export function snapshotFailure(status: Pick<SessionLedgerStatus, "error">): string {
  return i18n.t(snapshotErrorKeys[status.error] ?? snapshotErrorKeys.unspecified)
}

/**
 * What the merge control is: nothing without changes; a disabled button
 * with why; "waiting to be merged" when only an integrator can merge; or
 * an enabled button.
 */
export type MergeState =
  | { kind: "none" }
  | { kind: "disabled"; reason: string }
  | { kind: "waiting" }
  | { kind: "enabled"; changesetId: string }

export function mergeState(
  ledger: SessionLedger,
  { turnRunning, deviceOffline }: { turnRunning: boolean; deviceOffline: boolean },
): MergeState {
  const changesetId = ledger.changeset?.changesetId ?? ""
  if (ledger.status?.state !== "ready" || changesetId === "" || ledger.entries.length === 0) return { kind: "none" }
  if (ledger.problems.length > 0) return { kind: "disabled", reason: i18n.t("sessionLedger.merge.blockedProblems") }
  if (!ledger.fastForward) return { kind: "waiting" }
  if (turnRunning) return { kind: "disabled", reason: i18n.t("sessionLedger.merge.blockedRunning") }
  // The server would collect first, which needs the device.
  if (ledger.status.collectionDue && deviceOffline) {
    return { kind: "disabled", reason: i18n.t("sessionLedger.mergeError.DEVICE_OFFLINE") }
  }
  return { kind: "enabled", changesetId }
}

/** The reasons MergeSessionLedger refuses a merge, as the server names them. */
export type MergeRefusal =
  | "TURN_RUNNING"
  | "NOT_FAST_FORWARD"
  | "DEVICE_OFFLINE"
  | "PROBLEMS"
  | "NO_CHANGES"
  | "CHANGESET_CHANGED"

const mergeRefusalKeys = {
  TURN_RUNNING: "sessionLedger.mergeError.TURN_RUNNING",
  NOT_FAST_FORWARD: "sessionLedger.mergeError.NOT_FAST_FORWARD",
  DEVICE_OFFLINE: "sessionLedger.mergeError.DEVICE_OFFLINE",
  PROBLEMS: "sessionLedger.mergeError.PROBLEMS",
  NO_CHANGES: "sessionLedger.mergeError.NO_CHANGES",
  CHANGESET_CHANGED: "sessionLedger.mergeError.CHANGESET_CHANGED",
} as const satisfies Record<MergeRefusal, string>

/** Why a merge failed, by the server's answer. */
export function mergeErrorMessage(error: unknown): string {
  if (isApiError(error, "failed_precondition")) {
    const type = error.preconditionType ?? ""
    if (Object.hasOwn(mergeRefusalKeys, type)) return i18n.t(mergeRefusalKeys[type as MergeRefusal])
    return i18n.t("sessionLedger.mergeError.other")
  }
  if (isApiError(error, "not_found")) return i18n.t("session.actionError.gone")
  return errorMessage(error)
}

/** The numbers a merge gave new decisions, in order. */
export const assignedNumbers = (numbers: Record<string, number>): number[] =>
  Object.values(numbers).sort((a, b) => a - b)

/**
 * The changes the session's device may hold beyond what the server knows,
 * which deleting the session would discard: an open changeset with
 * changes, or a collection still due.
 */
export const holdsUnmergedChanges = (status: SessionLedgerStatus | undefined): boolean =>
  !!status && ((status.changesetId !== "" && status.changeCount > 0) || status.collectionDue)
