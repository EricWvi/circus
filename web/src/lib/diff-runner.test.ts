import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import type * as LineDiff from "./line-diff"
import type { DiffLine } from "./line-diff"

/** A stand-in for the browser's Worker: records what it is sent; the test answers. */
class FakeWorker {
  static made: FakeWorker[] = []
  posted: { id: number; base: string; changeset: string }[] = []
  terminated = false
  onmessage: ((event: MessageEvent) => void) | null = null
  onerror: ((event: Event) => void) | null = null
  constructor() {
    FakeWorker.made.push(this)
  }
  postMessage(message: { id: number; base: string; changeset: string }) {
    this.posted.push(message)
  }
  terminate() {
    this.terminated = true
  }
  answer(id: number, lines: DiffLine[] | null) {
    this.onmessage?.({ data: { id, lines } } as MessageEvent)
  }
}

/** The runner fresh (it keeps its worker per module). */
async function runner() {
  vi.resetModules()
  return import("./diff-runner")
}

const added = (text: string): DiffLine[] => [{ kind: "added", text, newNo: 1 }]

beforeEach(() => {
  FakeWorker.made = []
  vi.stubGlobal("Worker", FakeWorker)
})
afterEach(() => {
  vi.unstubAllGlobals()
  vi.doUnmock("./line-diff")
})

describe("diffs in a worker", () => {
  it("matches each answer to its request by id, in whatever order they come", async () => {
    const { diffOffMainThread } = await runner()
    const first = diffOffMainThread("", "one")
    const second = diffOffMainThread("", "two")
    expect(FakeWorker.made).toHaveLength(1)
    const [worker] = FakeWorker.made
    const [a, b] = worker.posted
    expect([a.changeset, b.changeset]).toEqual(["one", "two"])
    expect(a.id).not.toBe(b.id)
    worker.answer(b.id, added("two"))
    worker.answer(a.id, added("one"))
    expect(await first).toEqual(added("one"))
    expect(await second).toEqual(added("two"))
    // An answer to nothing that waits is ignored.
    worker.answer(999, added("stray"))
  })

  it("passes on a worker's answer that the versions are too different", async () => {
    const { diffOffMainThread } = await runner()
    const pending = diffOffMainThread("a", "b")
    const [worker] = FakeWorker.made
    worker.answer(worker.posted[0].id, null)
    expect(await pending).toBeNull()
  })

  it("runs every waiting diff here when the worker fails, and later ones too", async () => {
    const { diffOffMainThread } = await runner()
    const first = diffOffMainThread("a\n", "b\n")
    const second = diffOffMainThread("", "c\n")
    const [worker] = FakeWorker.made
    worker.onerror?.(new Event("error"))
    expect(worker.terminated).toBe(true)
    expect(await first).toEqual([
      { kind: "removed", text: "a", oldNo: 1 },
      { kind: "added", text: "b", newNo: 1 },
    ])
    expect(await second).toEqual([{ kind: "added", text: "c", newNo: 1 }])
    expect(await diffOffMainThread("", "d\n")).toEqual([{ kind: "added", text: "d", newNo: 1 }])
    expect(FakeWorker.made).toHaveLength(1)
    expect(worker.posted).toHaveLength(2)
  })

  it("runs here when there is no Worker", async () => {
    vi.stubGlobal("Worker", undefined)
    const { diffOffMainThread } = await runner()
    expect(await diffOffMainThread("", "x\n")).toEqual([{ kind: "added", text: "x", newNo: 1 }])
  })

  it("answers too different when the diff fails here, instead of never answering", async () => {
    vi.stubGlobal("Worker", undefined)
    vi.doMock("./line-diff", async (original) => ({
      ...(await original<typeof LineDiff>()),
      lineDiff: () => {
        throw new Error("out of memory")
      },
    }))
    const { diffOffMainThread } = await runner()
    expect(await diffOffMainThread("a", "b")).toBeNull()
  })
})
