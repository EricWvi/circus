// View-model types for the Web UI. They mirror what the control plane
// will eventually stream to the browser, but are intentionally decoupled
// from the generated protocol code so the UI can be built against mock data.

export type SessionState =
  | "running"
  | "awaiting_approval"
  | "queued"
  | "idle"
  | "interrupted"
  | "broken"
  | "stopped"

export interface ImageAttachment {
  id: string
  filename: string
  mediaType: string
  /** Object URL or data URL; mock data uses inline data URLs. */
  url: string
}

export type ToolCallStatus =
  | "running"
  | "completed"
  | "error"
  | "awaiting_approval"
  | "denied"

export interface ApprovalRequest {
  /** Why Claude wants to run this tool, as shown to the user. */
  reason: string
  decision?: "approved" | "denied"
}

interface EventBase {
  id: string
  /** ISO 8601 timestamp. */
  at: string
}

export interface UserMessageEvent extends EventBase {
  kind: "user_message"
  text: string
  attachments?: ImageAttachment[]
}

export interface AssistantTextEvent extends EventBase {
  kind: "assistant_text"
  /** Markdown. */
  text: string
  streaming?: boolean
  /**
   * The turn was interrupted while this text streamed: it was shown but never
   * saved, so Claude doesn't remember it.
   */
  unsaved?: boolean
}

export interface ReasoningEvent extends EventBase {
  kind: "reasoning"
  text: string
  durationSeconds: number
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

/** The user continued a broken session without the history it lost. */
export interface HistoryGapEvent extends EventBase {
  kind: "history_gap"
}

export type SessionEvent =
  | UserMessageEvent
  | AssistantTextEvent
  | ReasoningEvent
  | ToolCallEvent
  | TurnStoppedEvent
  | TurnInterruptedEvent
  | HistoryGapEvent

/** Session progress the server observes from the agent itself. */
export interface SessionActivity {
  state?: SessionState
  pendingApprovalCount?: number
  /** The server names a session after its first message. */
  title?: string
}

/** Conversation events keyed by session id, oldest first. */
export type Conversations = Record<string, SessionEvent[]>

/**
 * Where the simulated conversation stream keeps its transcripts and reports
 * session progress, until both come from the control plane.
 */
export interface ConversationStore {
  load(): Conversations
  save(sessionId: string, events: SessionEvent[]): void
  reportActivity(sessionId: string, activity: SessionActivity): void
  /** Ends whatever the session had in flight; nothing will finish it. */
  settle(sessionId: string): void
  /** Called with the sessions whose transcripts another tab changed. */
  subscribe(listener: (sessionIds: string[]) => void): () => void
}
