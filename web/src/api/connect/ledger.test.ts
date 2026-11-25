import { create } from "@bufbuild/protobuf"
import { BinaryWriter, WireType } from "@bufbuild/protobuf/wire"
import { Code, ConnectError, createRouterTransport, type ServiceImpl } from "@connectrpc/connect"
import {
  Changeset_State,
  GetSessionLedgerResponseSchema,
  LedgerService,
  SessionLedgerStatus_State,
} from "@circus/proto/circus/v1/ledger_pb"
import { describe, expect, it } from "vitest"

import { isLedgerBusy } from "@/api/errors"

import { createConnectLedger } from "./ledger"

function ledger(impl: Partial<ServiceImpl<typeof LedgerService>>) {
  return createConnectLedger(
    createRouterTransport(({ service }) => service(LedgerService, impl), { transport: { useBinaryFormat: false } }),
  )
}

describe("ledger over Connect", () => {
  it("maps the ledger", async () => {
    const res = await ledger({
      getLedger: (req) => {
        expect(req.projectId).toBe("p1")
        return {
          headCommit: "abc",
          nextNumber: 13,
          entries: [{ number: 12, title: "T", dir: "web", path: "web/D12-T.md", updatedAtUnixMs: 1_700_000_000_000n }],
        }
      },
    }).getLedger({ projectId: "p1" })
    expect(res).toEqual({
      headCommit: "abc",
      nextNumber: 13,
      entries: [{ number: 12, title: "T", dir: "web", path: "web/D12-T.md", updatedAtUnixMs: 1_700_000_000_000 }],
    })
  })

  it("asks for a decision now or at a commit", async () => {
    const seen: string[] = []
    const client = ledger({
      getDecision: (req) => {
        seen.push(`${req.projectId}:${req.number}:${req.commit}`)
        return {
          decision: { number: req.number, title: "T", dir: "", path: "D0-T.md", body: "**b**", commit: "c1", committedAtUnixMs: 5n },
        }
      },
    })
    expect(await client.getDecision({ projectId: "p", number: 0 })).toEqual({
      number: 0,
      title: "T",
      dir: "",
      path: "D0-T.md",
      body: "**b**",
      commit: "c1",
      committedAtUnixMs: 5,
    })
    await client.getDecision({ projectId: "p", number: 3, commit: "c0" })
    expect(seen).toEqual(["p:0:", "p:3:c0"])
  })

  it("fails a decision response without a decision", async () => {
    await expect(ledger({ getDecision: () => ({}) }).getDecision({ projectId: "p", number: 1 })).rejects.toMatchObject({
      code: "internal",
    })
  })

  it("pages the history", async () => {
    const client = ledger({
      listDecisionHistory: (req) => {
        expect([req.pageSize, req.pageToken]).toEqual(req.pageToken === "" ? [0, ""] : [20, "next"])
        return {
          versions: [
            {
              commit: "c2",
              committedAtUnixMs: 9n,
              author: "orchestrator",
              summary: "Move it",
              path: "a/D1-x.md",
              title: "x",
              added: false,
              contentChanged: false,
              moved: true,
              retitled: false,
              codeCommit: "f00",
            },
          ],
          nextPageToken: req.pageToken === "" ? "next" : "",
        }
      },
    })
    const first = await client.listDecisionHistory({ projectId: "p", number: 1 })
    expect(first.nextPageToken).toBe("next")
    expect(first.versions[0]).toEqual({
      commit: "c2",
      committedAtUnixMs: 9,
      author: "orchestrator",
      summary: "Move it",
      path: "a/D1-x.md",
      title: "x",
      added: false,
      contentChanged: false,
      moved: true,
      retitled: false,
      codeCommit: "f00",
    })
    expect((await client.listDecisionHistory({ projectId: "p", number: 1, pageSize: 20, pageToken: "next" })).nextPageToken).toBe("")
  })

  it("maps a session's ledger: status, open changeset, entries, problems and merged changesets", async () => {
    const changeset = {
      changesetId: "c1",
      state: Changeset_State.OPEN,
      baseCommit: "b",
      createdAtUnixMs: 1n,
      updatedAtUnixMs: 2n,
      sessionId: "s1",
    }
    const res = await ledger({
      getSessionLedger: (req) => {
        expect(req.sessionId).toBe("s1")
        return create(GetSessionLedgerResponseSchema, {
          status: { state: SessionLedgerStatus_State.READY, path: "/l", changesetId: "c1", changeCount: 1, updatedAtUnixMs: 3n },
          changeset,
          entries: [{ path: "a/D3-x.md", basePath: "b/D3-x.md", number: "D3", title: "X", moved: true, problems: ["bad"] }],
          problems: [{ path: "", message: "too many" }],
          warnings: [{ path: "n.md", message: "not a decision" }],
          fastForward: true,
          merged: [
            { changeset: { ...changeset, changesetId: "c0", state: Changeset_State.MERGED, mergedCommit: "m" }, numbers: { "D?a": 92 } },
            // A merged entry without its changeset is dropped.
            { numbers: { "D?b": 93 } },
          ],
        })
      },
    }).getSessionLedger({ sessionId: "s1" })
    expect(res).toEqual({
      status: {
        state: "ready",
        path: "/l",
        error: "unspecified",
        errorDetail: "",
        collectedAtUnixMs: 0,
        collectionDue: false,
        changesetId: "c1",
        changeCount: 1,
        problemCount: 0,
        updatedAtUnixMs: 3,
        applying: "none",
      },
      changeset: {
        changesetId: "c1",
        state: "open",
        baseCommit: "b",
        createdAtUnixMs: 1,
        updatedAtUnixMs: 2,
        mergedCommit: "",
        discardReason: "",
      },
      entries: [
        {
          path: "a/D3-x.md",
          basePath: "b/D3-x.md",
          number: "D3",
          title: "X",
          baseTitle: "",
          added: false,
          contentChanged: false,
          moved: true,
          retitled: false,
          deleted: false,
          problems: ["bad"],
        },
      ],
      problems: [{ path: "", message: "too many" }],
      warnings: [{ path: "n.md", message: "not a decision" }],
      fastForward: true,
      merged: [
        {
          changeset: {
            changesetId: "c0",
            state: "merged",
            baseCommit: "b",
            createdAtUnixMs: 1,
            updatedAtUnixMs: 2,
            mergedCommit: "m",
            discardReason: "",
          },
          numbers: { "D?a": 92 },
        },
      ],
    })
    // Without a snapshot or an open changeset.
    expect(await ledger({ getSessionLedger: () => ({}) }).getSessionLedger({ sessionId: "s1" })).toEqual({
      entries: [],
      problems: [],
      warnings: [],
      fastForward: false,
      merged: [],
    })
  })

  it("reads a diff of one path", async () => {
    const diff = await ledger({
      getSessionLedgerDiff: (req) => {
        expect(req).toMatchObject({ sessionId: "s1", path: "web/D1-x.md" })
        return { baseExists: true, base: "a", baseTruncated: true, changesetExists: true, changeset: "b" }
      },
    }).getSessionLedgerDiff({ sessionId: "s1", path: "web/D1-x.md" })
    expect(diff).toEqual({
      baseExists: true,
      base: "a",
      baseTruncated: true,
      changesetExists: true,
      changeset: "b",
      changesetTruncated: false,
    })
  })

  it("merges the changeset the user saw, and names why a merge is refused", async () => {
    const refusal = (type: string) => {
      const w = new BinaryWriter()
      w.tag(1, WireType.LengthDelimited).fork()
      w.tag(1, WireType.LengthDelimited).string(type)
      w.tag(3, WireType.LengthDelimited).string(`${type} description`)
      w.join()
      return w.finish()
    }
    const client = ledger({
      mergeSessionLedger: (req) => {
        if (req.expectedChangesetId !== "c1") {
          const error = new ConnectError("changed", Code.FailedPrecondition)
          error.details.push({ type: "google.rpc.PreconditionFailure", value: refusal("CHANGESET_CHANGED") })
          throw error
        }
        return { changeset: { changesetId: "c1", state: Changeset_State.MERGED }, numbers: { "D?a": 92, "D?b": 93 } }
      },
    })
    expect(await client.mergeSessionLedger({ sessionId: "s1", expectedChangesetId: "c1" })).toMatchObject({
      changeset: { changesetId: "c1", state: "merged" },
      numbers: { "D?a": 92, "D?b": 93 },
    })
    await expect(client.mergeSessionLedger({ sessionId: "s1", expectedChangesetId: "c0" })).rejects.toMatchObject({
      code: "failed_precondition",
      preconditionType: "CHANGESET_CHANGED",
    })
    // A malformed detail, or none, carries no type.
    const bare = ledger({
      mergeSessionLedger: () => {
        const error = new ConnectError("no", Code.FailedPrecondition)
        error.details.push({ type: "google.rpc.PreconditionFailure", value: new Uint8Array([0x0a, 0xff]) })
        throw error
      },
    })
    const error = await bare.mergeSessionLedger({ sessionId: "s1", expectedChangesetId: "c1" }).catch((e: unknown) => e)
    expect(error).toMatchObject({ code: "failed_precondition" })
    expect((error as { preconditionType?: string }).preconditionType).toBeUndefined()
  })

  it("fails a merge answer without its changeset", async () => {
    await expect(
      ledger({ mergeSessionLedger: () => ({}) }).mergeSessionLedger({ sessionId: "s1", expectedChangesetId: "c1" }),
    ).rejects.toMatchObject({ code: "internal" })
  })
})

describe("the busy reply of a session ledger read", () => {
  const refusing = (error: ConnectError) =>
    ledger({
      getSessionLedger: () => {
        throw error
      },
    })
  const refusal = (error: ConnectError) => refusing(error).getSessionLedger({ sessionId: "s1" }).catch((e: unknown) => e)

  it("is a refusal marked LEDGER_BUSY, whatever its message, and no other", async () => {
    // The message alone says nothing: protocol meaning is in the ErrorInfo.
    expect(isLedgerBusy(await refusal(new ConnectError("ledger is busy", Code.ResourceExhausted)))).toBe(false)
    const reason = new TextEncoder().encode("LEDGER_BUSY")
    const marked = new ConnectError("slots taken", Code.ResourceExhausted)
    marked.details.push({ type: "google.rpc.ErrorInfo", value: new Uint8Array([0x0a, reason.length, ...reason]) })
    expect(await refusal(marked)).toMatchObject({ code: "resource_exhausted", reason: "LEDGER_BUSY" })
    expect(isLedgerBusy(await refusal(marked))).toBe(true)
    expect(isLedgerBusy(await refusal(new ConnectError("message too large", Code.ResourceExhausted)))).toBe(false)
  })
})

describe("abandoning a session ledger read", () => {
  it("passes the signal to the request, for the snapshot and for a diff", async () => {
    const aborted: string[] = []
    const hang = (name: string) => (_req: unknown, ctx: { signal: AbortSignal }) =>
      new Promise<never>((_resolve, reject) => {
        ctx.signal.addEventListener("abort", () => {
          aborted.push(name)
          reject(new ConnectError("abandoned", Code.Canceled))
        })
      })
    const client = ledger({ getSessionLedger: hang("snapshot"), getSessionLedgerDiff: hang("diff") })
    const snapshot = new AbortController()
    const diff = new AbortController()
    const reads = [
      client.getSessionLedger({ sessionId: "s1" }, { signal: snapshot.signal }).catch((e: unknown) => e),
      client.getSessionLedgerDiff({ sessionId: "s1", path: "D1-x.md" }, { signal: diff.signal }).catch((e: unknown) => e),
    ]
    await new Promise((resolve) => setTimeout(resolve, 10))
    snapshot.abort()
    diff.abort()
    expect(await Promise.all(reads)).toEqual([
      expect.objectContaining({ code: "canceled" }),
      expect.objectContaining({ code: "canceled" }),
    ])
    expect(aborted.sort()).toEqual(["diff", "snapshot"])
  })
})
