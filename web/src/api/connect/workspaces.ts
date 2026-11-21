import {
  PrepareWorkspaceResult_Action,
  WorkspaceError as ProtoWorkspaceError,
} from "@circus/proto/circus/v1/device_pb"
import {
  WorkspaceRemoval_Outcome,
  type WorkspaceRemoval as ProtoWorkspaceRemoval,
} from "@circus/proto/circus/v1/project_pb"
import {
  WorkspaceStatus_State,
  WorkspaceStatus_Waiting,
  type WorkspaceStatus as ProtoWorkspaceStatus,
} from "@circus/proto/circus/v1/workspace_pb"

import type {
  WorkspaceAction,
  WorkspaceError,
  WorkspaceRemoval,
  WorkspaceState,
  WorkspaceStatus,
  WorkspaceWaiting,
} from "@/api/types"

const states: Record<WorkspaceStatus_State, WorkspaceState> = {
  [WorkspaceStatus_State.UNSPECIFIED]: "unspecified",
  [WorkspaceStatus_State.PENDING]: "pending",
  [WorkspaceStatus_State.PREPARING]: "preparing",
  [WorkspaceStatus_State.READY]: "ready",
  [WorkspaceStatus_State.FAILED]: "failed",
}

const waitings: Record<WorkspaceStatus_Waiting, WorkspaceWaiting> = {
  [WorkspaceStatus_Waiting.UNSPECIFIED]: "unspecified",
  [WorkspaceStatus_Waiting.DEVICE_OFFLINE]: "device_offline",
  [WorkspaceStatus_Waiting.DAEMON_OUTDATED]: "daemon_outdated",
  [WorkspaceStatus_Waiting.RETRY]: "retry",
}

const actions: Record<PrepareWorkspaceResult_Action, WorkspaceAction> = {
  [PrepareWorkspaceResult_Action.UNSPECIFIED]: "unspecified",
  [PrepareWorkspaceResult_Action.CREATED]: "created",
  [PrepareWorkspaceResult_Action.USED_EMPTY]: "used_empty",
  [PrepareWorkspaceResult_Action.CLONED]: "cloned",
  [PrepareWorkspaceResult_Action.USED_REPOSITORY]: "used_repository",
}

const errors: Record<ProtoWorkspaceError, WorkspaceError> = {
  [ProtoWorkspaceError.UNSPECIFIED]: "unspecified",
  [ProtoWorkspaceError.INVALID_REQUEST]: "invalid_request",
  [ProtoWorkspaceError.HOME_UNKNOWN]: "home_unknown",
  [ProtoWorkspaceError.UNSUPPORTED_OS]: "unsupported_os",
  [ProtoWorkspaceError.PATH_CONFLICT]: "path_conflict",
  [ProtoWorkspaceError.NOT_EMPTY]: "not_empty",
  [ProtoWorkspaceError.REMOTE_MISMATCH]: "remote_mismatch",
  [ProtoWorkspaceError.GIT_UNAVAILABLE]: "git_unavailable",
  [ProtoWorkspaceError.CLONE_FAILED]: "clone_failed",
  [ProtoWorkspaceError.NO_SPACE]: "no_space",
  [ProtoWorkspaceError.PERMISSION_DENIED]: "permission_denied",
  [ProtoWorkspaceError.TIMEOUT]: "timeout",
  [ProtoWorkspaceError.BUSY]: "busy",
  [ProtoWorkspaceError.CANCELLED]: "cancelled",
  [ProtoWorkspaceError.IO]: "io",
  [ProtoWorkspaceError.REFUSED]: "refused",
  [ProtoWorkspaceError.DEVICE_REVOKED]: "device_revoked",
  [ProtoWorkspaceError.INTERRUPTED]: "interrupted",
  [ProtoWorkspaceError.DAEMON_FAULT]: "daemon_fault",
}

const outcomes: Record<WorkspaceRemoval_Outcome, WorkspaceRemoval["outcome"]> = {
  [WorkspaceRemoval_Outcome.UNSPECIFIED]: "unspecified",
  [WorkspaceRemoval_Outcome.REMOVED]: "removed",
  [WorkspaceRemoval_Outcome.KEPT]: "kept",
  [WorkspaceRemoval_Outcome.FAILED]: "failed",
}

/** Values a newer server may send read as unspecified. */
export const toWorkspaceError = (error: ProtoWorkspaceError): WorkspaceError => errors[error] ?? "unspecified"

export function toWorkspace(ws: ProtoWorkspaceStatus): WorkspaceStatus {
  return {
    state: states[ws.state] ?? "unspecified",
    waiting: waitings[ws.waiting] ?? "unspecified",
    device: ws.device,
    path: ws.path,
    repoUrl: ws.repoUrl,
    background: ws.background,
    action: actions[ws.action] ?? "unspecified",
    error: toWorkspaceError(ws.error),
    errorDetail: ws.errorDetail,
    attemptStartedAtUnixMs: Number(ws.attemptStartedAtUnixMs),
    updatedAtUnixMs: Number(ws.updatedAtUnixMs),
  }
}

export const toWorkspaceRemoval = (r: ProtoWorkspaceRemoval): WorkspaceRemoval => ({
  device: r.device,
  outcome: outcomes[r.outcome] ?? "unspecified",
  path: r.path,
  error: toWorkspaceError(r.error),
  errorDetail: r.errorDetail,
})
