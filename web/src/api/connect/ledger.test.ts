import { createRouterTransport, type ServiceImpl } from "@connectrpc/connect"
import { LedgerService } from "@circus/proto/circus/v1/ledger_pb"
import { describe, expect, it } from "vitest"

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
})
