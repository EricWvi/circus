import { QueryClientProvider, type QueryClient } from "@tanstack/react-query"
import { useEffect, type ReactNode } from "react"

import { ClientContext } from "./context"
import { followAuthChanges } from "./query-client"
import type { CircusClient } from "./types"

export function ApiProvider({
  client,
  queryClient,
  children,
}: {
  client: CircusClient
  queryClient: QueryClient
  children: ReactNode
}) {
  useEffect(() => followAuthChanges(queryClient), [queryClient])

  return (
    <ClientContext.Provider value={client}>
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    </ClientContext.Provider>
  )
}
