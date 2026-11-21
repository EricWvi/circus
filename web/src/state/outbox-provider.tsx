import { useQueryClient } from "@tanstack/react-query"
import { useState, type ReactNode } from "react"

import { useClient } from "@/api/context"
import { invalidateSession } from "@/api/hooks"

import { createOutbox } from "./outbox"
import { OutboxContext } from "./outbox-context"

/** Holds the messages being sent. Mount it per signed-in user. */
export function OutboxProvider({ children }: { children: ReactNode }) {
  const client = useClient()
  const queryClient = useQueryClient()
  const [outbox] = useState(() =>
    createOutbox(client.sessions, {
      // A sent message moves the session's activity, and the first one names it.
      onSent: (sessionId) => invalidateSession(queryClient, sessionId),
    }),
  )
  return <OutboxContext.Provider value={outbox}>{children}</OutboxContext.Provider>
}
