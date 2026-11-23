import type { ToolPart } from "@/components/ai-elements/tool"
import type { ToolCallEvent, ToolCallStatus } from "@/lib/types"

const toolPartState: Record<ToolCallStatus, ToolPart["state"]> = {
  running: "input-available",
  completed: "output-available",
  error: "output-error",
  awaiting_approval: "approval-requested",
  denied: "output-denied",
}

/** Map our tool status onto the AI Elements tool state vocabulary. */
export function toToolPartState(status: ToolCallStatus): ToolPart["state"] {
  return toolPartState[status]
}

/**
 * The state a tool call's header shows. A denied call the user then approved
 * reads as approved, not as a failure: the agent retries it as a new call.
 */
export function toolCallPartState(event: Pick<ToolCallEvent, "status" | "approval">): ToolPart["state"] {
  if (event.status === "denied" && event.approval?.decision === "approved") return "approval-responded"
  return toToolPartState(event.status)
}
