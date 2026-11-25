import { describe, expect, it } from "vitest"

import type { CircusClient, RawSessionEvent } from "@/api/types"

import { createMockBackend } from "./backend"
import { NOT_A_DECISION } from "./session-ledger"
import { SEED_OWNER_ID, seedState, type MockState } from "./state"

function mock(state: MockState = seedState()) {
  state.cookie = SEED_OWNER_ID
  return { state, ...createMockBackend({ state, latency: () => 0, sessionTiming: { stepMs: 1, chunkMs: 1 } }) }
}

/** Watches a session until `done` holds for the events received, or a second passes. */
async function watchUntil(
  client: CircusClient,
  sessionId: string,
  done: (events: RawSessionEvent[]) => boolean,
  afterSeq = 0,
): Promise<RawSessionEvent[]> {
  const abort = new AbortController()
  const timer = setTimeout(() => abort.abort(), 1_000)
  const events: RawSessionEvent[] = []
  try {
    for await (const event of client.sessions.watchSession({ sessionId, afterSeq, signal: abort.signal })) {
      events.push(event)
      if (done(events)) break
    }
  } finally {
    clearTimeout(timer)
    abort.abort()
  }
  return events
}

const ledgerEvents = (events: RawSessionEvent[]) =>
  events.filter((e) => e.kind === "ledger").map((e) => ({ ...JSON.parse(e.payloadJson), durable: e.seq > 0 }))

const refusal = (type: string) => ({ code: "failed_precondition", preconditionType: type })

describe("mock session ledgers", () => {
  it("gives the demo sessions snapshots: changes of every kind, problems, a non-fast-forward one, none and a failure", async () => {
    const { client } = mock()
    const sessions = await client.sessions.listSessions({})
    const status = (id: string) => sessions.find((s) => s.sessionId === id)?.ledger

    expect(status("web-vite")).toMatchObject({ state: "ready", changeCount: 5, problemCount: 0, changesetId: "cs-web-vite" })
    expect(status("palace-new")).toMatchObject({ state: "ready", changeCount: 3, problemCount: 3 })
    expect(status("quic-spike")).toMatchObject({ state: "ready", changeCount: 2 })
    expect(status("libvips")).toMatchObject({ state: "ready", changeCount: 0, changesetId: "" })
    expect(status("fix-auth-refresh")).toMatchObject({ state: "failed", error: "permission_denied" })
    expect(status("dotfiles-go")).toBeUndefined()

    const web = await client.ledger.getSessionLedger({ sessionId: "web-vite" })
    expect(web.fastForward).toBe(true)
    const byNumber = (n: string) => web.entries.find((e) => e.number === n)
    expect(byNumber("D?a")).toMatchObject({ added: true, path: "web/D?a-Show-a-session's-Ledger-changes-next-to-its-conversation.md" })
    expect(byNumber("D3")).toMatchObject({ moved: true, contentChanged: false, retitled: false })
    expect(byNumber("D4")).toMatchObject({ retitled: true, moved: false, baseTitle: "Ledger 以 Git 仓库存储" })
    expect(byNumber("D5")).toMatchObject({ moved: true, retitled: true, contentChanged: true })
    expect(byNumber("D10")).toMatchObject({ contentChanged: true, moved: false })
    expect(byNumber("")).toBeUndefined()
    expect(web.entries.map((e) => e.path)).toEqual([...web.entries.map((e) => e.path)].sort())

    const palace = await client.ledger.getSessionLedger({ sessionId: "palace-new" })
    expect(palace.problems.map((p) => p.path)).toEqual(["", "ops/D?a-Health-checks-over-HTTP.md", "ops/notes.txt"])
    // A file that isn't a decision is a problem: a ledger holds only decision files.
    expect(palace.problems[2].message).toBe(NOT_A_DECISION)
    expect((await client.ledger.getSessionLedger({ sessionId: "quic-spike" })).fastForward).toBe(false)
  })

  it("serves a changed file next to its base, and not_found for another path", async () => {
    const { client } = mock()
    const web = await client.ledger.getSessionLedger({ sessionId: "web-vite" })
    const revised = web.entries.find((e) => e.number === "D10")!
    const diff = await client.ledger.getSessionLedgerDiff({ sessionId: "web-vite", path: revised.path })
    expect(diff).toMatchObject({ baseExists: true, changesetExists: true, baseTruncated: false, changesetTruncated: false })
    expect(diff.changeset).toContain("Decision diffs are shown as plain text too.")
    expect(diff.base).not.toContain("Decision diffs are shown as plain text too.")
    const added = await client.ledger.getSessionLedgerDiff({ sessionId: "palace-new", path: "ops/notes.txt" })
    expect(added).toMatchObject({ baseExists: false, base: "", changesetExists: true })
    await expect(client.ledger.getSessionLedgerDiff({ sessionId: "web-vite", path: "nope.md" })).rejects.toMatchObject({
      code: "not_found",
    })
  })

  it("merges: new decisions get the next numbers, the main line takes them, and a recorded event says so", async () => {
    const { client } = mock()
    const before = await client.ledger.getLedger({ projectId: "circus" })
    const res = await client.ledger.mergeSessionLedger({ sessionId: "web-vite", expectedChangesetId: "cs-web-vite" })
    expect(res.numbers).toEqual({ "D?a": before.nextNumber })
    expect(res.changeset.state).toBe("merged")

    const after = await client.ledger.getLedger({ projectId: "circus" })
    expect(after.nextNumber).toBe(before.nextNumber + 1)
    const added = await client.ledger.getDecision({ projectId: "circus", number: before.nextNumber })
    expect(added.title).toBe("Show a session's Ledger changes next to its conversation")
    expect(after.entries.find((e) => e.number === 3)?.dir).toBe("storage")
    expect(after.entries.find((e) => e.number === 4)?.title).toBe("The Ledger is stored as a Git repository")

    const session = await client.sessions.getSession({ sessionId: "web-vite" })
    expect(session.ledger).toMatchObject({ changeCount: 0, changesetId: "" })
    const ledger = await client.ledger.getSessionLedger({ sessionId: "web-vite" })
    expect(ledger.entries).toEqual([])
    expect(ledger.merged).toHaveLength(1)
    expect(ledger.merged[0].numbers).toEqual(res.numbers)

    const events = await watchUntil(client, "web-vite", (e) => e.some((x) => x.kind === "ledger"))
    expect(ledgerEvents(events)).toEqual([
      { event: "merged", changeset_id: "cs-web-vite", numbers: { "D?a": before.nextNumber }, durable: true },
    ])
    expect(events.find((e) => e.kind === "ledger")?.tsUnixMs).toBe(session.ledger?.updatedAtUnixMs)
  })

  it("refuses a merge for each reason the server does", async () => {
    const { client, state } = mock()
    const merge = (sessionId: string, expectedChangesetId = `cs-${sessionId}`) =>
      client.ledger.mergeSessionLedger({ sessionId, expectedChangesetId })
    await expect(merge("libvips")).rejects.toMatchObject(refusal("NO_CHANGES"))
    await expect(merge("web-vite", "cs-other")).rejects.toMatchObject(refusal("CHANGESET_CHANGED"))
    await expect(merge("palace-new")).rejects.toMatchObject(refusal("PROBLEMS"))
    await expect(merge("quic-spike")).rejects.toMatchObject(refusal("NOT_FAST_FORWARD"))

    const web = state.sessions.find((s) => s.sessionId === "web-vite")!
    web.state = "running"
    await expect(merge("web-vite")).rejects.toMatchObject(refusal("TURN_RUNNING"))
    web.state = "idle"
    web.ledger!.status.collectionDue = true
    state.devices.find((d) => d.name === web.device)!.online = false
    await expect(merge("web-vite")).rejects.toMatchObject(refusal("DEVICE_OFFLINE"))
  })

  it("leaves the project's other open changesets behind the main line after a merge", async () => {
    const { client, state } = mock()
    // Another circus session with changes that fast-forward until the merge.
    const other = state.sessions.find((s) => s.sessionId === "quic-spike")!
    other.ledger!.open!.fastForward = true
    other.ledger!.open!.changeset.baseCommit = (await client.ledger.getLedger({ projectId: "circus" })).headCommit
    // The change is announced live: watched from before the merge.
    const watching = watchUntil(client, "quic-spike", (e) => e.some((x) => x.kind === "ledger"))
    await new Promise((resolve) => setTimeout(resolve, 10))
    await client.ledger.mergeSessionLedger({ sessionId: "web-vite", expectedChangesetId: "cs-web-vite" })
    expect((await client.ledger.getSessionLedger({ sessionId: "quic-spike" })).fastForward).toBe(false)
    const events = await watching
    expect(ledgerEvents(events)).toEqual([{ event: "collected", changeset_id: "cs-quic-spike", durable: false }])
  })

  it("collects after a turn, live, and finds a decision the message asked for", async () => {
    const { client } = mock()
    const watching = watchUntil(client, "libvips", (e) => e.filter((x) => x.kind === "ledger").length >= 1)
    await client.sessions.recoverSession({ sessionId: "libvips" })
    await client.sessions.sendMessage({ sessionId: "libvips", clientMsgId: "m1", text: "Record a decision about caching" })
    const events = await watching
    expect(ledgerEvents(events)).toEqual([
      { event: "collected", changeset_id: expect.stringMatching(/^cs-libvips-/), durable: false },
    ])
    const ledger = await client.ledger.getSessionLedger({ sessionId: "libvips" })
    expect(ledger.entries).toEqual([
      expect.objectContaining({ number: "D?a", added: true, title: "Record: Record a decision about caching" }),
    ])
    expect(ledger.fastForward).toBe(true)
  })

  it("discards the open changes as an integrator would, with a recorded event", async () => {
    const { client } = mock()
    await client.sessions.sendMessage({ sessionId: "web-vite", clientMsgId: "m1", text: "Please discard these" })
    const events = await watchUntil(client, "web-vite", (e) => e.some((x) => x.kind === "ledger"))
    expect(ledgerEvents(events)).toEqual([
      { event: "discarded", changeset_id: "cs-web-vite", reason: "discarded by an integrator", durable: true },
    ])
    const ledger = await client.ledger.getSessionLedger({ sessionId: "web-vite" })
    expect(ledger.entries).toEqual([])
    expect(ledger.changeset).toBeUndefined()
    expect((await client.sessions.getSession({ sessionId: "web-vite" })).ledger).toMatchObject({ changeCount: 0 })
  })

  it("refuses to merge a file that isn't a decision", async () => {
    const { client, state } = mock()
    // Only the non-decision file is left as a problem.
    const palace = state.sessions.find((s) => s.sessionId === "palace-new")!.ledger!.open!
    palace.snapshotProblems = []
    palace.changes = palace.changes.filter((c) => c.entry.number === "" || c.entry.problems.length === 0)
    await expect(
      client.ledger.mergeSessionLedger({ sessionId: "palace-new", expectedChangesetId: "cs-palace-new" }),
    ).rejects.toMatchObject(refusal("PROBLEMS"))
  })

  it("gives a new session a ready snapshot without changes", async () => {
    const { client } = mock()
    const session = await client.sessions.createSession({ projectId: "circus", locationIndex: 0, clientRequestId: "r1" })
    expect(session.ledger).toMatchObject({ state: "ready", changeCount: 0, changesetId: "" })
  })

  it("refuses to delete a session with unmerged changes unless they are discarded", async () => {
    const { client } = mock()
    await expect(client.sessions.deleteSession({ sessionId: "quic-spike" })).rejects.toMatchObject(refusal("LEDGER_CHANGES"))
    expect((await client.sessions.listSessions({})).some((s) => s.sessionId === "quic-spike")).toBe(true)
    await client.sessions.deleteSession({ sessionId: "quic-spike", discardLedgerChanges: true })
    expect((await client.sessions.listSessions({})).some((s) => s.sessionId === "quic-spike")).toBe(false)
    await expect(client.sessions.getSession({ sessionId: "quic-spike" })).rejects.toMatchObject({ code: "not_found" })
  })

  it("deletes a stopped session without changes at once, and refuses one that isn't stopped", async () => {
    const { client } = mock()
    await expect(client.sessions.deleteSession({ sessionId: "web-vite" })).rejects.toMatchObject({
      code: "failed_precondition",
    })
    await client.sessions.stopSession({ sessionId: "libvips" })
    await client.sessions.deleteSession({ sessionId: "libvips" })
    await expect(client.sessions.getSession({ sessionId: "libvips" })).rejects.toMatchObject({ code: "not_found" })
  })
})
