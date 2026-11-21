// Assembles the client the app talks to. This is the only place that knows
// which backend serves what; pages only see `CircusClient` through context.
//
// VITE_CIRCUS_BACKEND selects it:
//   mock   (default) everything is served by the in-browser mock.
//   server everything goes to the real control plane over Connect (same
//          origin; in development the Vite server proxies it).

import { createConnectAuth } from "./connect/auth"
import { createConnectMachines } from "./connect/machines"
import { createConnectProjects } from "./connect/projects"
import { createConnectSessions } from "./connect/sessions"
import { createServerTransport } from "./connect/transport"
import { startMockBackend } from "./mock"
import { browserOidcFlow } from "./oidc"
import type { CircusClient } from "./types"

export type BackendKind = "mock" | "server"

export function backendKind(value: string | undefined): BackendKind {
  if (value === undefined || value === "" || value === "mock") return "mock"
  if (value === "server") return "server"
  throw new Error(`VITE_CIRCUS_BACKEND must be "mock" or "server", not "${value}".`)
}

export interface Backend {
  client: CircusClient
  /**
   * Called when something outside this tab changed the data, the way a
   * server would push updates; the app refetches. Only the mock, whose
   * projects and devices live in localStorage, calls it.
   */
  onExternalChange(listener: () => void): () => void
}

/** Starts the backend selected by VITE_CIRCUS_BACKEND. */
export function startBackend(): Backend {
  if (backendKind(import.meta.env.VITE_CIRCUS_BACKEND) === "mock") return startMockBackend()

  const transport = createServerTransport(window.location.origin)
  return {
    client: {
      auth: createConnectAuth(transport),
      oidc: browserOidcFlow,
      projects: createConnectProjects(transport),
      machines: createConnectMachines(transport),
      sessions: createConnectSessions(transport),
    },
    onExternalChange: () => () => {},
  }
}
