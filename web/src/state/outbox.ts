// Messages the user sent that the session's stream doesn't show yet. Each
// gets its own client message id, shows at once as "sending", and stays
// here (failed, with a retry that reuses the id and the text, so the server
// can't take it twice) until the stream brings its INPUT_ACCEPTED event.
// It lives above the pages, so a message sent from the project page is
// still there, failed or not, on the session page it leads to.

import { isApiError } from "@/api/errors"
import type { InputPriority, SessionService } from "@/api/types"
import type { SendFailure } from "@/lib/types"
import { randomUuid } from "@/lib/uuid"

export interface OutboxMessage {
  sessionId: string
  clientMsgId: string
  text: string
  priority: InputPriority
  /** ISO 8601; when it was first sent. */
  at: string
  /** `accepted` once SendMessage answered. */
  delivery: "sending" | "failed" | "accepted" | "not_kept"
  failure?: SendFailure
}

export interface Outbox {
  /**
   * Sends a new message. Resolves once the server accepted it; rejects with
   * the error otherwise, and the message stays as failed.
   */
  send(sessionId: string, text: string, priority?: InputPriority): Promise<void>
  /** Sends a failed message again, with the same id and text. */
  retry(clientMsgId: string): Promise<void>
  /** The session's messages, oldest first; the same array until they change. */
  messages(sessionId: string): readonly OutboxMessage[]
  /** Forgets the session's messages with these ids (its stream shows them now). */
  forget(sessionId: string, clientMsgIds: ReadonlySet<string>): void
  subscribe(listener: () => void): () => void
}

/** Why a send failed, by the server's answer. */
export function sendFailure(error: unknown): SendFailure {
  if (isApiError(error, "resource_exhausted")) return "too_many"
  if (isApiError(error, "failed_precondition")) return "stopped"
  if (isApiError(error, "unavailable")) return "unreachable"
  return "other"
}

/** A new message id: a random UUID. */
export const newClientMsgId = randomUuid

const none: readonly OutboxMessage[] = []

export function createOutbox(
  sessions: Pick<SessionService, "sendMessage">,
  {
    onSent = () => {},
    now = Date.now,
  }: {
    /** A message reached the server (the session's title and activity moved). */
    onSent?: (sessionId: string) => void
    now?: () => number
  } = {},
): Outbox {
  const bySession = new Map<string, readonly OutboxMessage[]>()
  const listeners = new Set<() => void>()
  /** Sends in flight, so a double click can't send twice at once. */
  const inFlight = new Set<string>()

  const notify = () => {
    for (const listener of listeners) listener()
  }

  const find = (clientMsgId: string) => {
    for (const list of bySession.values()) {
      const message = list.find((m) => m.clientMsgId === clientMsgId)
      if (message) return message
    }
    return undefined
  }

  const update = (clientMsgId: string, patch: Partial<OutboxMessage>) => {
    const message = find(clientMsgId)
    if (!message) return
    const list = bySession.get(message.sessionId)!
    bySession.set(
      message.sessionId,
      list.map((m) => (m.clientMsgId === clientMsgId ? { ...m, ...patch } : m)),
    )
    notify()
  }

  async function deliver(message: OutboxMessage) {
    const { sessionId, clientMsgId, text, priority } = message
    inFlight.add(clientMsgId)
    update(clientMsgId, { delivery: "sending", failure: undefined })
    try {
      const { eventSeq } = await sessions.sendMessage({ sessionId, clientMsgId, text, priority })
      // eventSeq 0: the session's view is full, so the stream will never
      // show this message; it stays here, saying so.
      update(clientMsgId, { delivery: eventSeq === 0 ? "not_kept" : "accepted" })
      onSent(sessionId)
    } catch (error) {
      update(clientMsgId, { delivery: "failed", failure: sendFailure(error) })
      throw error
    } finally {
      inFlight.delete(clientMsgId)
    }
  }

  return {
    send(sessionId, text, priority = "normal") {
      const message: OutboxMessage = {
        sessionId,
        clientMsgId: newClientMsgId(),
        text,
        priority,
        at: new Date(now()).toISOString(),
        delivery: "sending",
      }
      bySession.set(sessionId, [...(bySession.get(sessionId) ?? none), message])
      notify()
      return deliver(message)
    },
    retry(clientMsgId) {
      const message = find(clientMsgId)
      if (!message || message.delivery !== "failed" || inFlight.has(clientMsgId)) return Promise.resolve()
      return deliver(message)
    },
    messages: (sessionId) => bySession.get(sessionId) ?? none,
    forget(sessionId, clientMsgIds) {
      const list = bySession.get(sessionId)
      if (!list) return
      const kept = list.filter((m) => !clientMsgIds.has(m.clientMsgId) || inFlight.has(m.clientMsgId))
      if (kept.length === list.length) return
      if (kept.length === 0) bySession.delete(sessionId)
      else bySession.set(sessionId, kept)
      notify()
    },
    subscribe(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
  }
}
