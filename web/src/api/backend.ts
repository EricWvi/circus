// Assembles the client the app talks to. This is the only place that knows
// which backend serves what; pages only see `CircusClient` through context.
//
// VITE_CIRCUS_BACKEND selects it:
//   mock   (default) everything is served by the in-browser mock.
//   server auth, projects and devices go to the real control plane over
//          Connect (same origin; in development the Vite server proxies it);
//          sessions stay on the mock, which the control plane doesn't serve
//          yet. The mock's sessions use the server's projects through the
//          projects client.

import { createConnectAuth } from "./connect/auth"
import { createConnectMachines } from "./connect/machines"
import { createConnectProjects } from "./connect/projects"
import { createServerTransport } from "./connect/transport"
import { startMockBackend, type MockBackend } from "./mock"
import { browserOidcFlow } from "./oidc"

export type BackendKind = "mock" | "server"

export function backendKind(value: string | undefined): BackendKind {
  if (value === undefined || value === "" || value === "mock") return "mock"
  if (value === "server") return "server"
  throw new Error(`VITE_CIRCUS_BACKEND must be "mock" or "server", not "${value}".`)
}

/** Starts the backend selected by VITE_CIRCUS_BACKEND. */
export function startBackend(): MockBackend {
  if (backendKind(import.meta.env.VITE_CIRCUS_BACKEND) === "mock") return startMockBackend()

  const transport = createServerTransport(window.location.origin)
  const projects = createConnectProjects(transport)
  const mock = startMockBackend({ externalAuth: true, projects })
  return {
    ...mock,
    client: {
      auth: createConnectAuth(transport),
      oidc: browserOidcFlow,
      projects,
      machines: createConnectMachines(transport),
      sessions: mock.client.sessions,
    },
  }
}
