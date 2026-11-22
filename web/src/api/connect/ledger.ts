import { createClient, type Transport } from "@connectrpc/connect"
import {
  LedgerService as LedgerServiceDesc,
  type Decision as ProtoDecision,
  type DecisionVersion as ProtoDecisionVersion,
  type LedgerEntry as ProtoLedgerEntry,
} from "@circus/proto/circus/v1/ledger_pb"

import type { Decision, DecisionVersion, LedgerEntry, LedgerService } from "@/api/types"

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
  }
}
