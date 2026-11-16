// Browser entry for the mock control plane. Only the composition root
// (main.tsx) imports this; pages talk to the client through context.

import { createMockBackend, type MockBackend } from "./backend"
import { parsePersisted, serialize, STORAGE_KEY } from "./persist"
import { freshState, seedState, settleInFlight, type MockState } from "./state"

export type { MockBackend } from "./backend"

function load(): MockState | null {
  try {
    return parsePersisted(localStorage.getItem(STORAGE_KEY))
  } catch {
    return null
  }
}

/**
 * This tab's id. sessionStorage survives a reload but is per tab, so on load
 * the tab recognizes the in-flight work its previous page left behind.
 */
function tabId(): string {
  const key = "circus.mock.tab"
  try {
    const existing = sessionStorage.getItem(key)
    if (existing) return existing
    const id = crypto.randomUUID()
    sessionStorage.setItem(key, id)
    return id
  } catch {
    // Without sessionStorage only the staleness rule applies.
    return crypto.randomUUID()
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
 * an uninitialized server and `?mock=reset` restores the demo seed; the
 * query parameter is removed afterwards.
 */
export function startMockBackend(): MockBackend {
  const url = new URL(window.location.href)
  const mode = url.searchParams.get("mock")
  const tab = tabId()
  let state: MockState
  if (mode === "fresh") state = freshState()
  else if (mode === "reset") state = seedState()
  else {
    const loaded = load()
    if (loaded) settleInFlight(loaded, tab)
    state = loaded ?? seedState()
  }

  if (mode !== null) {
    url.searchParams.delete("mock")
    window.history.replaceState(window.history.state, "", url)
  }
  save(state)

  if (state.setupToken !== null) {
    const link = new URL(`/setup?token=${encodeURIComponent(state.setupToken)}`, url.origin)
    console.info(`circus is not set up yet. Open this link to create the first user:\n\n  ${link}\n`)
  }

  const backend = createMockBackend({ state, onChange: save, tabId: tab })

  // Another tab changed the store: adopt its state so this tab's next write
  // doesn't overwrite it, and tell the conversations which transcripts moved.
  window.addEventListener("storage", (event) => {
    if (event.key !== STORAGE_KEY) return
    const next = parsePersisted(event.newValue)
    if (next) backend.adopt(next)
  })

  return backend
}
