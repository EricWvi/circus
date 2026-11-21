import { createContext, useContext, useSyncExternalStore } from "react"

import type { Outbox, OutboxMessage } from "./outbox"

export const OutboxContext = createContext<Outbox | null>(null)

export function useOutbox(): Outbox {
  const outbox = useContext(OutboxContext)
  if (!outbox) throw new Error("useOutbox must be used within OutboxProvider")
  return outbox
}

/** The session's messages that its stream doesn't show yet. */
export function useOutboxMessages(sessionId: string): readonly OutboxMessage[] {
  const outbox = useOutbox()
  return useSyncExternalStore(outbox.subscribe, () => outbox.messages(sessionId))
}
