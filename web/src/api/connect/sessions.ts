import { createClient, type Transport } from "@connectrpc/connect"
import {
  ApprovalKind as ProtoApprovalKind,
  InputPriority as ProtoInputPriority,
  QueueReason as ProtoQueueReason,
  SessionEvent_Kind,
  SessionService as SessionServiceDesc,
  Session_State,
  TurnOutcome as ProtoTurnOutcome,
  type Session as ProtoSession,
  type SessionApproval as ProtoApproval,
  type SessionEvent as ProtoEvent,
} from "@circus/proto/circus/v1/session_pb"

import type {
  ApprovalKind,
  QueueReason,
  RawSessionEvent,
  SessionDetails,
  SessionEventKind,
  SessionService,
  SessionSummary,
} from "@/api/types"
import { sessionPermissionMode } from "@/lib/permission-mode"
import { viewState, type ServerSessionState, type TurnOutcome } from "@/lib/session-state"

import { call, malformed, stream } from "./call"
import { toSessionLedgerStatus } from "./ledger"
import { toWorkspace } from "./workspaces"

const states: Record<Session_State, ServerSessionState> = {
  [Session_State.UNSPECIFIED]: "unspecified",
  [Session_State.IDLE]: "idle",
  [Session_State.QUEUED]: "queued",
  [Session_State.RUNNING]: "running",
  [Session_State.STOPPED]: "stopped",
  [Session_State.BROKEN]: "broken",
}

const outcomes: Record<ProtoTurnOutcome, TurnOutcome> = {
  [ProtoTurnOutcome.UNSPECIFIED]: "unspecified",
  [ProtoTurnOutcome.COMPLETED]: "completed",
  [ProtoTurnOutcome.INTERRUPTED]: "interrupted",
  [ProtoTurnOutcome.ERROR]: "error",
  [ProtoTurnOutcome.STOPPED]: "stopped",
}

const queueReasons: Record<ProtoQueueReason, QueueReason> = {
  [ProtoQueueReason.UNSPECIFIED]: "unspecified",
  [ProtoQueueReason.PROVIDER_FULL]: "provider_full",
  [ProtoQueueReason.NO_ASSIGNED_WORKER]: "no_assigned_worker",
  [ProtoQueueReason.PROVIDER_DELETED]: "provider_deleted",
  [ProtoQueueReason.PROVIDER_SECRETS_UNREADABLE]: "provider_secrets_unreadable",
  [ProtoQueueReason.WORKER_OFFLINE]: "worker_offline",
}

/** The proto kind names, lower case without `KIND_`; a newer kind reads as unspecified. */
const kinds = Object.fromEntries(
  Object.entries(SessionEvent_Kind)
    .filter((entry): entry is [string, number] => typeof entry[1] === "number")
    .map(([name, value]) => [value, name.toLowerCase() as SessionEventKind]),
) as Record<number, SessionEventKind>

export function toSummary(session: ProtoSession): SessionSummary {
  const serverState = states[session.state] ?? "unspecified"
  return {
    sessionId: session.sessionId,
    projectId: session.projectId,
    title: session.title,
    serverState,
    turnCount: Number(session.turnCount),
    viewTruncated: session.viewTruncated,
    state: viewState({
      state: serverState,
      lastTurnOutcome: outcomes[session.lastTurnOutcome] ?? "unspecified",
      pendingApprovalCount: session.pendingApprovalCount,
    }),
    lastActivityAtUnixMs: Number(session.lastActivityAtUnixMs),
    pendingApprovalCount: session.pendingApprovalCount,
    createdAtUnixMs: Number(session.createdAtUnixMs),
    ...(session.workspace ? { workspace: toWorkspace(session.workspace) } : {}),
    providerId: session.providerId,
    // A reason this client doesn't know yet reads as none.
    queueReason: queueReasons[session.queueReason] ?? "unspecified",
    workerDisconnectedAtUnixMs: Number(session.workerDisconnectedAtUnixMs),
    ...(session.ledger ? { ledger: toSessionLedgerStatus(session.ledger) } : {}),
  }
}

/** A session with its config, which GetSession and the writes return. */
export function toDetails(session: ProtoSession | undefined, rpc: string): SessionDetails {
  if (!session?.config) throw malformed(rpc)
  const device = session.config.defaultDevice
  return {
    ...toSummary(session),
    device,
    cwd: session.config.cwds.find((c) => c.device === device)?.cwd ?? "",
    permissionMode: sessionPermissionMode(session.config.permissionMode),
  }
}

export function toRawEvent(event: ProtoEvent): RawSessionEvent {
  return {
    seq: Number(event.seq),
    tsUnixMs: Number(event.tsUnixMs),
    kind: kinds[event.kind] ?? "unspecified",
    payloadJson: event.payloadJson,
    ephemeral: event.ephemeral,
    truncated: event.truncated,
  }
}

const approvalKinds: Record<ProtoApprovalKind, ApprovalKind> = {
  [ProtoApprovalKind.UNSPECIFIED]: "tool",
  [ProtoApprovalKind.TOOL]: "tool",
  [ProtoApprovalKind.QUESTION]: "question",
  [ProtoApprovalKind.PLAN]: "plan",
}

const toApproval = (a: ProtoApproval) => ({
  approvalId: a.approvalId,
  sessionId: a.sessionId,
  toolUseId: a.toolUseId,
  tool: a.tool,
  device: a.device,
  display: a.display,
  reason: a.reason,
  expiresAtUnixMs: Number(a.expiresAtUnixMs),
  kind: approvalKinds[a.kind] ?? "tool",
  inputJson: a.inputJson,
})

/** A user has at most 1000 sessions, which one page can hold. */
const PAGE_SIZE = 1000

/** The session service of a real control plane, over Connect. */
export function createConnectSessions(transport: Transport): SessionService {
  const client = createClient(SessionServiceDesc, transport)
  return {
    listSessions: (req) =>
      call(async () => {
        const out: SessionSummary[] = []
        let pageToken = ""
        do {
          const res = await client.listSessions({ projectId: req.projectId, pageSize: PAGE_SIZE, pageToken })
          out.push(...res.sessions.map(toSummary))
          pageToken = res.nextPageToken
        } while (pageToken !== "")
        return out
      }),
    createSession: (req) =>
      call(async () => {
        const res = await client.createSession({
          projectId: req.projectId,
          locationIndex: req.locationIndex,
          clientRequestId: req.clientRequestId,
          providerId: req.providerId,
          ...(req.permissionMode ? { config: { permissionMode: req.permissionMode } } : {}),
        })
        return toDetails(res.session, "CreateSession")
      }),
    getSession: (req) => call(async () => toDetails(await client.getSession(req), "GetSession")),
    sendMessage: (req) =>
      call(async () => {
        const res = await client.sendMessage({
          sessionId: req.sessionId,
          clientMsgId: req.clientMsgId,
          text: req.text,
          priority: req.priority === "now" ? ProtoInputPriority.NOW : ProtoInputPriority.NORMAL,
        })
        return { inputId: res.inputId, eventSeq: Number(res.eventSeq) }
      }),
    watchSession: ({ sessionId, afterSeq, signal }) =>
      stream(() => client.watchSession({ sessionId, afterSeq: BigInt(afterSeq) }, { signal }), toRawEvent),
    interrupt: (req) => call(async () => void (await client.interrupt(req))),
    stopSession: (req) => call(async () => void (await client.stopSession(req))),
    listApprovals: (req) => call(async () => (await client.listApprovals(req)).approvals.map(toApproval)),
    resolveApproval: (req) =>
      call(
        async () =>
          void (await client.resolveApproval({
            approvalId: req.approvalId,
            approve: req.approve,
            answers: req.answers ?? [],
          })),
      ),
    deleteSession: (req) =>
      call(
        async () =>
          void (await client.deleteSession({
            sessionId: req.sessionId,
            discardLedgerChanges: req.discardLedgerChanges ?? false,
          })),
      ),
    recoverSession: (req) =>
      call(async () => void (await client.recoverSession({ sessionId: req.sessionId, acceptTranscriptGap: true }))),
    updateSession: (req) =>
      call(async () =>
        toDetails(
          await client.updateSession({ sessionId: req.sessionId, title: req.title, providerId: req.providerId }),
          "UpdateSession",
        ),
      ),
    retryWorkspace: (req) =>
      call(async () => {
        const res = await client.retryWorkspace({ sessionId: req.sessionId })
        if (!res.workspace) throw malformed("RetryWorkspace")
        return toWorkspace(res.workspace)
      }),
  }
}
