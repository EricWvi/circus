import { createClient, type Transport } from "@connectrpc/connect"
import { LedgerSnapshotError as ProtoSnapshotError } from "@circus/proto/circus/v1/device_pb"
import {
  Changeset_State,
  LedgerService as LedgerServiceDesc,
  SessionLedgerStatus_Applying,
  SessionLedgerStatus_State,
  type Changeset as ProtoChangeset,
  type Decision as ProtoDecision,
  type DecisionVersion as ProtoDecisionVersion,
  type LedgerEntry as ProtoLedgerEntry,
  type LedgerProblem as ProtoProblem,
  type SessionLedgerEntry as ProtoSessionEntry,
  type SessionLedgerStatus as ProtoSessionLedgerStatus,
} from "@circus/proto/circus/v1/ledger_pb"

import type {
  ChangesetState,
  Decision,
  DecisionVersion,
  LedgerChangeset,
  LedgerEntry,
  LedgerApplying,
  LedgerProblem,
  LedgerService,
  LedgerSnapshotError,
  SessionLedgerEntry,
  SessionLedgerState,
  SessionLedgerStatus,
} from "@/api/types"

import { call, malformed } from "./call"

const toEntry = (e: ProtoLedgerEntry): LedgerEntry => ({
  number: e.number,
  title: e.title,
  dir: e.dir,
  path: e.path,
  updatedAtUnixMs: Number(e.updatedAtUnixMs),
})

function toDecision(d: ProtoDecision | undefined): Decision {
  if (!d) throw malformed("GetDecision")
  return {
    number: d.number,
    title: d.title,
    dir: d.dir,
    path: d.path,
    body: d.body,
    commit: d.commit,
    committedAtUnixMs: Number(d.committedAtUnixMs),
  }
}

const toVersion = (v: ProtoDecisionVersion): DecisionVersion => ({
  commit: v.commit,
  committedAtUnixMs: Number(v.committedAtUnixMs),
  author: v.author,
  summary: v.summary,
  path: v.path,
  title: v.title,
  added: v.added,
  contentChanged: v.contentChanged,
  moved: v.moved,
  retitled: v.retitled,
  codeCommit: v.codeCommit,
})

const snapshotStates: Record<SessionLedgerStatus_State, SessionLedgerState> = {
  [SessionLedgerStatus_State.UNSPECIFIED]: "unspecified",
  [SessionLedgerStatus_State.PENDING]: "pending",
  [SessionLedgerStatus_State.PREPARING]: "preparing",
  [SessionLedgerStatus_State.READY]: "ready",
  [SessionLedgerStatus_State.FAILED]: "failed",
}

const applyings: Record<SessionLedgerStatus_Applying, LedgerApplying> = {
  [SessionLedgerStatus_Applying.UNSPECIFIED]: "none",
  [SessionLedgerStatus_Applying.MERGE]: "merge",
  [SessionLedgerStatus_Applying.DISCARD]: "discard",
}

const snapshotErrors: Record<ProtoSnapshotError, LedgerSnapshotError> = {
  [ProtoSnapshotError.UNSPECIFIED]: "unspecified",
  [ProtoSnapshotError.INVALID_REQUEST]: "invalid_request",
  [ProtoSnapshotError.STATE_DIR_UNKNOWN]: "state_dir_unknown",
  [ProtoSnapshotError.UNSUPPORTED_OS]: "unsupported_os",
  [ProtoSnapshotError.PATH_CONFLICT]: "path_conflict",
  [ProtoSnapshotError.PERMISSION_DENIED]: "permission_denied",
  [ProtoSnapshotError.NO_SPACE]: "no_space",
  [ProtoSnapshotError.ABSENT]: "absent",
  [ProtoSnapshotError.STALE]: "stale",
  [ProtoSnapshotError.BUSY]: "busy",
  [ProtoSnapshotError.IO]: "io",
  [ProtoSnapshotError.TIMEOUT]: "timeout",
}

const changesetStates: Record<Changeset_State, ChangesetState> = {
  [Changeset_State.UNSPECIFIED]: "unspecified",
  [Changeset_State.OPEN]: "open",
  [Changeset_State.MERGED]: "merged",
  [Changeset_State.DISCARDED]: "discarded",
}

/** A session's snapshot status; values this client doesn't know read as unspecified. */
export function toSessionLedgerStatus(s: ProtoSessionLedgerStatus): SessionLedgerStatus {
  return {
    state: snapshotStates[s.state] ?? "unspecified",
    path: s.path,
    error: snapshotErrors[s.error] ?? "unspecified",
    errorDetail: s.errorDetail,
    collectedAtUnixMs: Number(s.collectedAtUnixMs),
    collectionDue: s.collectionDue,
    changesetId: s.changesetId,
    changeCount: s.changeCount,
    problemCount: s.problemCount,
    updatedAtUnixMs: Number(s.updatedAtUnixMs),
    applying: applyings[s.applying] ?? "none",
  }
}

const toChangeset = (c: ProtoChangeset): LedgerChangeset => ({
  changesetId: c.changesetId,
  state: changesetStates[c.state] ?? "unspecified",
  baseCommit: c.baseCommit,
  createdAtUnixMs: Number(c.createdAtUnixMs),
  updatedAtUnixMs: Number(c.updatedAtUnixMs),
  mergedCommit: c.mergedCommit,
  discardReason: c.discardReason,
})

const toSessionEntry = (e: ProtoSessionEntry): SessionLedgerEntry => ({
  path: e.path,
  basePath: e.basePath,
  number: e.number,
  title: e.title,
  baseTitle: e.baseTitle,
  added: e.added,
  contentChanged: e.contentChanged,
  moved: e.moved,
  retitled: e.retitled,
  deleted: e.deleted,
  problems: [...e.problems],
})

const toProblem = (p: ProtoProblem): LedgerProblem => ({ path: p.path, message: p.message })

/** The ledger service of a real control plane, over Connect. */
export function createConnectLedger(transport: Transport): LedgerService {
  const client = createClient(LedgerServiceDesc, transport)
  return {
    getLedger: (req) =>
      call(async () => {
        const res = await client.getLedger({ projectId: req.projectId })
        return { headCommit: res.headCommit, entries: res.entries.map(toEntry), nextNumber: res.nextNumber }
      }),
    getDecision: (req) =>
      call(async () => {
        const res = await client.getDecision({ projectId: req.projectId, number: req.number, commit: req.commit ?? "" })
        return toDecision(res.decision)
      }),
    listDecisionHistory: (req) =>
      call(async () => {
        const res = await client.listDecisionHistory({
          projectId: req.projectId,
          number: req.number,
          pageSize: req.pageSize ?? 0,
          pageToken: req.pageToken ?? "",
        })
        return { versions: res.versions.map(toVersion), nextPageToken: res.nextPageToken }
      }),
    getSessionLedger: (req, options) =>
      call(async () => {
        const res = await client.getSessionLedger({ sessionId: req.sessionId }, { signal: options?.signal })
        return {
          ...(res.status ? { status: toSessionLedgerStatus(res.status) } : {}),
          ...(res.changeset ? { changeset: toChangeset(res.changeset) } : {}),
          entries: res.entries.map(toSessionEntry),
          problems: res.problems.map(toProblem),
          warnings: res.warnings.map(toProblem),
          fastForward: res.fastForward,
          merged: res.merged.flatMap((m) =>
            m.changeset ? [{ changeset: toChangeset(m.changeset), numbers: { ...m.numbers } }] : [],
          ),
        }
      }),
    getSessionLedgerDiff: (req, options) =>
      call(async () => {
        const res = await client.getSessionLedgerDiff({ sessionId: req.sessionId, path: req.path }, { signal: options?.signal })
        return {
          baseExists: res.baseExists,
          base: res.base,
          baseTruncated: res.baseTruncated,
          changesetExists: res.changesetExists,
          changeset: res.changeset,
          changesetTruncated: res.changesetTruncated,
        }
      }),
    mergeSessionLedger: (req) =>
      call(async () => {
        const res = await client.mergeSessionLedger({
          sessionId: req.sessionId,
          expectedChangesetId: req.expectedChangesetId,
        })
        if (!res.changeset) throw malformed("MergeSessionLedger")
        return { changeset: toChangeset(res.changeset), numbers: { ...res.numbers } }
      }),
  }
}
