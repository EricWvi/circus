import { createContext, useContext } from "react"

import type { CircusClient } from "./types"

export const ClientContext = createContext<CircusClient | null>(null)

export function useClient(): CircusClient {
  const client = useContext(ClientContext)
  if (!client) throw new Error("useClient must be used within ApiProvider")
  return client
}
