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

export interface Project {
  id: string
  name: string
}

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

export type SessionEvent =
  | UserMessageEvent
  | AssistantTextEvent
  | ReasoningEvent
  | ToolCallEvent

export interface Session {
  id: string
  projectId: string
  title: string
  state: SessionState
  /** ISO 8601 timestamp of the last event or state change. */
  lastActivityAt: string
  pendingApprovalCount: number
  /** Device the session's tools execute on. */
  deviceName: string
  events: SessionEvent[]
}
