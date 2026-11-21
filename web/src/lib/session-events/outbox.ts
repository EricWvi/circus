import type { OutboxMessage } from "@/state/outbox"

import type { SessionEvent, UserMessageEvent } from "../types"
import { messageEventId } from "./transcript"

/**
 * The view events with the messages still on their way: those the stream
 * already shows (by client message id) appear once, where the stream put
 * them; the others follow, as the user sent them.
 */
export function withOutbox(
  events: readonly SessionEvent[],
  outbox: readonly OutboxMessage[],
): readonly SessionEvent[] {
  if (outbox.length === 0) return events
  const shown = new Set(events.map((e) => e.id))
  const pending = outbox
    .filter((m) => !shown.has(messageEventId(m.clientMsgId)))
    .map(
      (m): UserMessageEvent => ({
        kind: "user_message",
        id: messageEventId(m.clientMsgId),
        at: m.at,
        text: m.text,
        clientMsgId: m.clientMsgId,
        delivery: m.delivery,
        ...(m.failure ? { failure: m.failure } : {}),
      }),
    )
  return pending.length === 0 ? events : [...events, ...pending]
}
