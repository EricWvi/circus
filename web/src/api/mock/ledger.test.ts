import { describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"
import { fakeCommitId, seedLedgers } from "@/mock/ledger"

import { createMockBackend } from "./backend"
import {
  decisionFileName,
  decisionOf,
  historyOf,
  ledgerOf,
  type MockDecisionState,
  type MockLedgerCommit,
} from "./ledger"
import { SEED_OWNER_ID, seedState } from "./state"

const d = (number: number, dir: string, title: string, body = `body of ${number}`): MockDecisionState => ({
  number,
  dir,
  title,
  body,
})

const commits: MockLedgerCommit[] = [
  { commit: fakeCommitId("a"), atUnixMs: 1000, author: "import", summary: "Import", codeCommit: "", changes: [d(0, "", "Root"), d(9, "web", "Nine"), d(10, "web", "Ten"), d(2, "arch", "Two")] },
  { commit: fakeCommitId("b"), atUnixMs: 2000, author: "s1", summary: "Move two", codeCommit: "c0de", changes: [d(2, "arch/storage", "Two")] },
  { commit: fakeCommitId("c"), atUnixMs: 3000, author: "s2", summary: "Rename and edit two", codeCommit: "", changes: [d(2, "arch/storage", "Two again", "new body")] },
]

async function rejection(run: () => unknown): Promise<ApiError> {
  try {
    await run()
  } catch (error) {
    expect(error).toBeInstanceOf(ApiError)
    return error as ApiError
  }
  throw new Error("did not fail")
}

describe("decisionFileName", () => {
  it("derives the name from the title", () => {
    expect(decisionFileName(6, "M3 worker 设计的七项取舍")).toBe("D6-M3-worker-设计的七项取舍.md")
    expect(decisionFileName(1, '  a/b\\c:d*e?f"g<h>i|j\tk  ')).toBe("D1-a-b-c-d-e-f-g-h-i-j-k.md")
    expect(decisionFileName(2, "..--Hello -- world.--")).toBe("D2-Hello-world.md")
    expect(decisionFileName(3, "字".repeat(70))).toBe(`D3-${"字".repeat(60)}.md`)
  })
})

describe("ledgerOf", () => {
  it("lists the head by directory, then file name, with when each last changed", () => {
    const ledger = ledgerOf(commits)
    expect(ledger.headCommit).toBe(fakeCommitId("c"))
    expect(ledger.nextNumber).toBe(11)
    expect(ledger.entries.map((e) => [e.path, e.updatedAtUnixMs])).toEqual([
      ["D0-Root.md", 1000],
      ["arch/storage/D2-Two-again.md", 3000],
      ["web/D10-Ten.md", 1000],
      ["web/D9-Nine.md", 1000],
    ])
  })

  it("is empty without commits", () => {
    expect(ledgerOf([])).toEqual({ headCommit: "", entries: [], nextNumber: 0 })
  })
})

describe("decisionOf", () => {
  it("reads the head or an older commit", () => {
    expect(decisionOf(commits, 2)).toMatchObject({ title: "Two again", dir: "arch/storage", body: "new body", commit: fakeCommitId("c"), committedAtUnixMs: 3000 })
    expect(decisionOf(commits, 2, fakeCommitId("a"))).toMatchObject({ title: "Two", dir: "arch", path: "arch/D2-Two.md", body: "body of 2", committedAtUnixMs: 1000 })
  })

  it("refuses missing decisions, unknown commits and malformed input", async () => {
    expect((await rejection(() => decisionOf(commits, 7))).code).toBe("not_found")
    expect((await rejection(() => decisionOf(commits, 2, fakeCommitId("zzz")))).code).toBe("not_found")
    expect(await rejection(() => decisionOf(commits, 2, "abc"))).toMatchObject({ code: "invalid_argument", field: "commit" })
    expect(await rejection(() => decisionOf(commits, -1))).toMatchObject({ code: "invalid_argument", field: "number" })
  })
})

describe("historyOf", () => {
  it("lists the versions newest first with what each changed", () => {
    const { versions, nextPageToken } = historyOf(commits, 2)
    expect(nextPageToken).toBe("")
    expect(versions.map(({ summary, added, contentChanged, moved, retitled, codeCommit }) => ({ summary, added, contentChanged, moved, retitled, codeCommit }))).toEqual([
      { summary: "Rename and edit two", added: false, contentChanged: true, moved: false, retitled: true, codeCommit: "" },
      { summary: "Move two", added: false, contentChanged: false, moved: true, retitled: false, codeCommit: "c0de" },
      { summary: "Import", added: true, contentChanged: false, moved: false, retitled: false, codeCommit: "" },
    ])
    expect(versions[0]).toMatchObject({ path: "arch/storage/D2-Two-again.md", title: "Two again", author: "s2" })
  })

  it("pages", async () => {
    const first = historyOf(commits, 2, 2)
    expect(first.versions.map((v) => v.summary)).toEqual(["Rename and edit two", "Move two"])
    const second = historyOf(commits, 2, 2, first.nextPageToken)
    expect(second).toMatchObject({ versions: [{ summary: "Import" }], nextPageToken: "" })
    expect(await rejection(() => historyOf(commits, 2, 101))).toMatchObject({ code: "invalid_argument", field: "page_size" })
    expect(await rejection(() => historyOf(commits, 2, 0, "nope"))).toMatchObject({ code: "invalid_argument", field: "page_token" })
    expect((await rejection(() => historyOf(commits, 5))).code).toBe("not_found")
  })
})

describe("the demo ledgers", () => {
  it("hold a few directories, one nested, and history that pages", () => {
    const circus = seedLedgers(0).get("circus") ?? []
    const ledger = ledgerOf(circus)
    expect(ledger.entries.length).toBe(16)
    expect(new Set(ledger.entries.map((e) => e.dir))).toEqual(new Set(["", "architecture", "architecture/storage", "session", "web"]))
    expect(circus.map((c) => c.atUnixMs)).toEqual([...circus.map((c) => c.atUnixMs)].sort((a, b) => a - b))
    expect(historyOf(circus, 0, 20).nextPageToken).not.toBe("")
    const flags = (n: number) => historyOf(circus, n).versions.map((v) => [v.added, v.contentChanged, v.moved, v.retitled])
    expect(flags(3)).toEqual([[false, false, true, false], [true, false, false, false]])
    expect(flags(6)[1]).toEqual([false, true, false, true])
  })
})

describe("the mock's ledger service", () => {
  const backend = (owner = SEED_OWNER_ID) => {
    const state = seedState()
    state.cookie = owner
    return createMockBackend({ state, latency: () => 0 }).client
  }

  it("serves the signed-in user's projects only", async () => {
    const client = backend()
    expect((await client.ledger.getLedger({ projectId: "circus" })).entries.length).toBe(16)
    expect((await client.ledger.getLedger({ projectId: "toolbox" })).entries).toEqual([])
    expect((await rejection(() => backend("user-dev").ledger.getLedger({ projectId: "circus" }))).code).toBe("not_found")
    expect((await rejection(() => client.ledger.getDecision({ projectId: "nope", number: 1 }))).code).toBe("not_found")
  })

  it("drops a deleted project's ledger", async () => {
    const state = seedState()
    state.cookie = SEED_OWNER_ID
    for (const s of state.sessions) s.state = "stopped"
    const client = createMockBackend({ state, latency: () => 0 }).client
    await client.projects.deleteProject({ projectId: "circus" })
    expect((await rejection(() => client.ledger.getLedger({ projectId: "circus" }))).code).toBe("not_found")
  })
})
