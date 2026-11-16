import { createContext, useContext } from "react"

import type { ConversationStore, ImageAttachment, SessionEvent } from "@/lib/types"

export interface ConversationsContextValue {
  /** Conversation events of a session, oldest first. */
  events: (sessionId: string) => SessionEvent[]
  /** Append a user message and simulate a streamed assistant reply. */
  send: (sessionId: string, text: string, attachments: ImageAttachment[]) => void
  /** Interrupt the current turn. */
  stop: (sessionId: string) => void
  decideApproval: (
    sessionId: string,
    eventId: string,
    decision: "approved" | "denied",
  ) => void
}

export const ConversationsContext = createContext<ConversationsContextValue | null>(null)

export function useConversations(): ConversationsContextValue {
  const ctx = useContext(ConversationsContext)
  if (!ctx) throw new Error("useConversations must be used within ConversationsProvider")
  return ctx
}

/** The store the simulated conversations read from and write to. */
export const ConversationStoreContext = createContext<ConversationStore | null>(null)

export function useConversationStore(): ConversationStore {
  const store = useContext(ConversationStoreContext)
  if (!store) throw new Error("useConversationStore must be used within ConversationStoreContext")
  return store
}
