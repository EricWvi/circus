import { createContext, useContext } from "react"

import type { ConversationStore, ImageAttachment, SessionEvent, SessionState } from "@/lib/types"

import type { Connection } from "./connection"

export interface ConversationsContextValue {
  /**
   * Conversation events of a session as this browser has received them,
   * oldest first. While the connection is down they stay as they were when
   * it dropped; once the loss is noticed, partially streamed text goes.
   */
  events: (sessionId: string) => SessionEvent[]
  /** Append a user message and simulate a streamed assistant reply. */
  send: (sessionId: string, text: string, attachments: ImageAttachment[]) => void
  /** The user stops the current turn: the session becomes idle. */
  stop: (sessionId: string) => void
  decideApproval: (
    sessionId: string,
    eventId: string,
    decision: "approved" | "denied",
  ) => void
  /**
   * Sends the latest interrupted turn's user message again, images included.
   * Does nothing unless the session is still interrupted.
   */
  retryTurn: (sessionId: string) => void
  /** A broken session goes on without the history it lost: idle, with a gap marker. */
  continueWithoutHistory: (sessionId: string) => void
  /** Ends the session for good. */
  stopSession: (sessionId: string) => void
  /** The connection to the control plane that streams the conversations. */
  connection: Connection
  /**
   * The current loss has lasted long enough to show (see `LOST_NOTICE_MS`).
   * The view drops partially streamed text at the same moment.
   */
  lossNoticed: boolean
  /** Tries to reconnect now instead of waiting for the next attempt. */
  retryNow: () => void
}

export const ConversationsContext = createContext<ConversationsContextValue | null>(null)

/** Development switches for the simulated conversations. */
export interface ConversationSimulation {
  /** The connection fails for a while (see the provider for how long). */
  dropConnection: () => void
  /**
   * The session's state on the server, which the view may not show yet
   * while the connection is down.
   */
  liveState: (sessionId: string) => SessionState | undefined
  /** The worker running the session's turn is lost mid-turn. */
  killWorker: (sessionId: string) => void
  /** Part of the session's transcript couldn't be saved. */
  breakTranscript: (sessionId: string) => void
}

/** Null where conversations are not simulated. */
export const ConversationSimulationContext = createContext<ConversationSimulation | null>(null)

export function useConversationSimulation(): ConversationSimulation | null {
  return useContext(ConversationSimulationContext)
}

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
