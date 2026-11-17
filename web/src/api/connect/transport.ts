import type { Transport } from "@connectrpc/connect"
import { createConnectTransport } from "@connectrpc/connect-web"

import { connectErrorBodiesOnly } from "./errors"

/**
 * The Connect transport to the control plane at `baseUrl`, with JSON. Every
 * service of the client shares it.
 */
export function createServerTransport(baseUrl: string): Transport {
  return createConnectTransport({
    baseUrl,
    // Same origin, so cookies flow anyway; explicit for clarity.
    fetch: connectErrorBodiesOnly((input, init) => fetch(input, { ...init, credentials: "include" })),
  })
}
