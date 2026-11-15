import type { ToolPart } from "@/components/ai-elements/tool"
import type { ToolCallStatus } from "@/lib/types"

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
