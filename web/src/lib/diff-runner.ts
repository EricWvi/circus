// Runs line diffs in a worker, so a diff that takes its whole time limit
// doesn't block the page. Where workers aren't available (tests, old
// browsers) or the worker fails, the diff runs on the main thread after a
// macrotask, still bounded by its time limit. A diff that fails answers
// null, as one that gives up does.

import { lineDiff, type DiffLine } from "./line-diff"

type Resolve = (lines: DiffLine[] | null) => void

let worker: Worker | null | undefined
let nextId = 0
const waiting = new Map<number, { base: string; changeset: string; resolve: Resolve }>()

/** `lineDiff`, or null (both versions whole) when it fails. */
function safeLineDiff(base: string, changeset: string): DiffLine[] | null {
  try {
    return lineDiff(base, changeset)
  } catch {
    return null
  }
}

const onMainThread = (base: string, changeset: string) =>
  new Promise<DiffLine[] | null>((resolve) => setTimeout(() => resolve(safeLineDiff(base, changeset)), 0))

function diffWorker(): Worker | null {
  if (worker !== undefined) return worker
  try {
    worker = typeof Worker === "undefined" ? null : new Worker(new URL("./line-diff.worker.ts", import.meta.url), { type: "module" })
  } catch {
    worker = null
  }
  if (!worker) return null
  worker.onmessage = (event: MessageEvent<{ id: number; lines: DiffLine[] | null }>) => {
    const request = waiting.get(event.data.id)
    waiting.delete(event.data.id)
    request?.resolve(event.data.lines)
  }
  worker.onerror = () => {
    // The worker is unusable: what waits on it runs here instead.
    worker?.terminate()
    worker = null
    const requests = [...waiting.values()]
    waiting.clear()
    for (const r of requests) void onMainThread(r.base, r.changeset).then(r.resolve)
  }
  return worker
}

/** A line diff of `base` against `changeset` off the main thread where possible; null when too different. */
export function diffOffMainThread(base: string, changeset: string): Promise<DiffLine[] | null> {
  const w = diffWorker()
  if (!w) return onMainThread(base, changeset)
  return new Promise((resolve) => {
    const id = nextId++
    waiting.set(id, { base, changeset, resolve })
    w.postMessage({ id, base, changeset })
  })
}
