// Assembles the client the app talks to. This is the only place that knows
// which backend serves what; pages only see `CircusClient` through context.
//
// VITE_CIRCUS_BACKEND selects it:
//   mock   (default) everything is served by the in-browser mock.
//   server auth goes to the real control plane over Connect (same origin;
//          in development the Vite server proxies it); projects, devices and
//          sessions stay on the mock, which the control plane doesn't serve yet.

import { createServerAuth } from "./connect/auth"
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

  const mock = startMockBackend({ externalAuth: true })
  return {
    ...mock,
    client: {
      ...mock.client,
      auth: createServerAuth(window.location.origin),
      oidc: browserOidcFlow,
    },
  }
}
