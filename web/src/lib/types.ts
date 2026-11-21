// View-model types for the Web UI: what the session view renders. The
// reducer in `session-events/` builds them from the control plane's raw
// session events; the components never see the wire format.

export type SessionState =
  | "running"
  | "awaiting_approval"
  | "queued"
  | "idle"
  | "interrupted"
  | "broken"
  | "stopped"

export type ToolCallStatus =
  | "running"
  | "completed"
  | "error"
  | "awaiting_approval"
  | "denied"

export interface ApprovalRequest {
  /** The control plane's approval id, for ResolveApproval. */
  approvalId: string
  /** Why the tool needs approval, as shown to the user. */
  reason: string
  /** What the approval covers (the command or path), as shown to the user. */
  display: string
  /** When the server asked; a request expires ten minutes later. */
  requestedAtUnixMs: number
  /** Unset while pending. */
  decision?: "approved" | "denied" | "expired"
}

interface EventBase {
  /** Stable across reconnects and replays; React keys use it. */
  id: string
  /** ISO 8601 timestamp. */
  at: string
}

/**
 * How far a message the user sent has come: the request is under way
 * (`sending`) or failed (`failed`), the control plane took it
 * (`accepted`), or the agent received it (`delivered`). `not_kept`: the
 * control plane took it, but the session's view is full, so the stream
 * will never show it or its delivery.
 */
export type MessageDelivery = "sending" | "failed" | "accepted" | "delivered" | "not_kept"

/** Why sending a message failed, as the composer explains it. */
export type SendFailure =
  /** Too many messages wait for delivery. */
  | "too_many"
  /** The session is stopped. */
  | "stopped"
  /** The server can't be reached. */
  | "unreachable"
  | "other"

export interface UserMessageEvent extends EventBase {
  kind: "user_message"
  text: string
  /** The client's id of the message; retries reuse it. Empty for messages known only from the agent. */
  clientMsgId: string
  delivery: MessageDelivery
  /** Set while `delivery` is `failed`. */
  failure?: SendFailure
}

export interface AssistantTextEvent extends EventBase {
  kind: "assistant_text"
  /** Markdown. */
  text: string
  streaming?: boolean
  /**
   * The turn was interrupted while this text streamed: it was shown but the
   * agent may not remember it.
   */
  unsaved?: boolean
}

export interface ReasoningEvent extends EventBase {
  kind: "reasoning"
  text: string
  streaming?: boolean
  /** How long the model thought, when this browser saw it stream. */
  durationSeconds?: number
}

export interface ToolCallEvent extends EventBase {
  kind: "tool_call"
  /** Claude Code tool name, e.g. Bash, Read, Edit. */
  tool: string
  input: Record<string, unknown>
  status: ToolCallStatus
  output?: string
  error?: string
  approval?: ApprovalRequest
}

/** The user stopped the turn; what was shown stays. */
export interface TurnStoppedEvent extends EventBase {
  kind: "turn_stopped"
}

/**
 * The worker was lost mid-turn (the server's TURN_INTERRUPTED). The turn can
 * be retried or continued until the user sends anything else.
 */
export interface TurnInterruptedEvent extends EventBase {
  kind: "turn_interrupted"
}

/** The turn ended with an error the worker reported. */
export interface TurnErrorEvent extends EventBase {
  kind: "turn_error"
  /** The worker's own description; not translated. */
  message: string
}

/** The user continued a broken session without the history it lost. */
export interface HistoryGapEvent extends EventBase {
  kind: "history_gap"
}

/** An event too large for the session's view: its content was dropped. */
export interface TruncatedEvent extends EventBase {
  kind: "truncated"
}

export type SessionEvent =
  | UserMessageEvent
  | AssistantTextEvent
  | ReasoningEvent
  | ToolCallEvent
  | TurnStoppedEvent
  | TurnInterruptedEvent
  | TurnErrorEvent
  | HistoryGapEvent
  | TruncatedEvent
