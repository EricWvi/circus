// Browser entry for the mock control plane. Only the composition root
// (main.tsx, through backend.ts) imports this; pages talk to the client
// through context.

import { createMockBackend, type MockBackend } from "./backend"
import { stressLedgers } from "@/mock/ledger"

import { parsePersisted, serialize, STORAGE_KEY } from "./persist"
import { addQueueDemo, addWorkerDemo, freshState, seedState, type MockState } from "./state"

export type { MockBackend } from "./backend"

function load(): MockState | null {
  try {
    return parsePersisted(localStorage.getItem(STORAGE_KEY))
  } catch {
    return null
  }
}

let warned = false

function save(state: MockState) {
  try {
    localStorage.setItem(STORAGE_KEY, serialize(state))
  } catch (error) {
    // Storage can be unavailable or full; the state still lives for this page load.
    if (!warned) console.warn("Mock state could not be saved; changes last until reload.", error)
    warned = true
  }
}

/**
 * Starts the mock backend from persisted state. `?mock=fresh` resets it to
 * an uninitialized server and `?mock=reset` restores the demo seed;
 * `?mock=nooidc` turns the OIDC provider off until the next page load and
 * `?mock=stress` adds decisions at the ledger's size limits to `palace`;
 * `?mock=queue` adds sessions that wait for each reason a session can, and
 * `?mock=worker` sessions whose worker lost its connection (it doesn't come
 * back: their leases end a minute later).
 * Modes combine with commas (`?mock=reset,nooidc`); the query parameter is
 * removed afterwards. Sessions live in this tab's memory only.
 */
export function startMockBackend(): MockBackend {
  const url = new URL(window.location.href)
  const mock = url.searchParams.get("mock")
  const modes = new Set(mock?.split(",") ?? [])
  const mode = modes.has("fresh") ? "fresh" : modes.has("reset") ? "reset" : null
  let state: MockState
  if (mode === "fresh") state = freshState()
  else if (mode === "reset") state = seedState()
  else state = load() ?? seedState()

  if (mock !== null) {
    url.searchParams.delete("mock")
    window.history.replaceState(window.history.state, "", url)
  }
  save(state)
  if (modes.has("queue")) addQueueDemo(state)
  if (modes.has("worker")) addWorkerDemo(state)

  if (state.setupToken !== null && !state.users.some((u) => u.role === "super")) {
    const link = new URL(`/setup?token=${encodeURIComponent(state.setupToken)}`, url.origin)
    console.info(`Circus is not set up yet. Open this link to create the super user:\n\n  ${link}\n`)
  }

  const backend = createMockBackend({
    state,
    onChange: save,
    note: (message) => console.info(message),
    oidc: !modes.has("nooidc"),
    ...(modes.has("stress") ? { ledgers: stressLedgers() } : {}),
  })

  // Another tab changed the store: adopt its state so this tab's next write
  // doesn't overwrite it.
  window.addEventListener("storage", (event) => {
    if (event.key !== STORAGE_KEY) return
    const next = parsePersisted(event.newValue)
    if (next) backend.adopt(next)
  })

  return backend
}
