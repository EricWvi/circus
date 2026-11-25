// Computes line diffs off the main thread: a diff of two large, very
// different files can take its whole time limit, which would freeze the page.

import { lineDiff } from "./line-diff"

interface Request {
  id: number
  base: string
  changeset: string
}

self.onmessage = (event: MessageEvent<Request>) => {
  const { id, base, changeset } = event.data
  let lines: ReturnType<typeof lineDiff>
  try {
    lines = lineDiff(base, changeset)
  } catch {
    // Answered as too different: the page shows both versions.
    lines = null
  }
  self.postMessage({ id, lines })
}
