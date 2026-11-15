import { Attachment, AttachmentPreview, Attachments } from "@/components/ai-elements/attachments"
import { Message, MessageContent, MessageResponse } from "@/components/ai-elements/message"
import { Reasoning, ReasoningContent, ReasoningTrigger } from "@/components/ai-elements/reasoning"
import type { Session, SessionEvent } from "@/lib/types"

import { ApprovalCard } from "./approval-card"
import { ToolCall } from "./tool-call"

export function EventItem({
  event,
  session,
  onDecide,
}: {
  event: SessionEvent
  session: Session
  onDecide: (eventId: string, decision: "approved" | "denied") => void
}) {
  switch (event.kind) {
    case "user_message":
      return (
        <Message from="user">
          {event.attachments && event.attachments.length > 0 && (
            <Attachments variant="grid">
              {event.attachments.map((a) => (
                <Attachment
                  key={a.id}
                  data={{
                    type: "file",
                    id: a.id,
                    url: a.url,
                    mediaType: a.mediaType,
                    filename: a.filename,
                  }}
                  className="size-auto h-24 w-40 border"
                >
                  <AttachmentPreview />
                </Attachment>
              ))}
            </Attachments>
          )}
          {event.text && (
            <MessageContent className="whitespace-pre-wrap">{event.text}</MessageContent>
          )}
        </Message>
      )
    case "assistant_text":
      if (!event.text) return null
      return (
        <Message from="assistant">
          <MessageContent>
            <MessageResponse isAnimating={event.streaming}>{event.text}</MessageResponse>
          </MessageContent>
        </Message>
      )
    case "reasoning":
      return (
        <Reasoning defaultOpen={false} duration={event.durationSeconds} className="mb-0">
          <ReasoningTrigger />
          <ReasoningContent>{event.text}</ReasoningContent>
        </Reasoning>
      )
    case "tool_call":
      if (event.status === "awaiting_approval") {
        return (
          <ApprovalCard
            event={event}
            deviceName={session.deviceName}
            onDecide={(d) => onDecide(event.id, d)}
          />
        )
      }
      return <ToolCall event={event} />
  }
}
