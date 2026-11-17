import { CircleSlashIcon, ImageOffIcon, SquareIcon, TriangleAlertIcon } from "lucide-react"
import { useState } from "react"
import { useTranslation } from "react-i18next"

import { Attachment, AttachmentPreview, Attachments } from "@/components/ai-elements/attachments"
import { Message, MessageContent, MessageResponse } from "@/components/ai-elements/message"
import { Reasoning, ReasoningContent, ReasoningTrigger } from "@/components/ai-elements/reasoning"
import { DisabledReason } from "@/components/disabled-reason"
import { Button } from "@/components/ui/button"
import type { SessionEvent } from "@/lib/types"

import { ApprovalCard } from "./approval-card"
import { ToolCall } from "./tool-call"

/** What the row of an interrupted turn offers while the turn can still be redone. */
export interface InterruptedTurnActions {
  onRetry: () => void
  onContinue: () => void
  /** Why the buttons can't be used right now; null when they can. */
  disabledReason: string | null
}

/**
 * "Turn interrupted", with Retry and Continue while the turn can be redone.
 * Either button works once: both disable as soon as one is used.
 */
function InterruptedTurnRow({ actions }: { actions?: InterruptedTurnActions }) {
  const { t } = useTranslation()
  const [used, setUsed] = useState(false)
  const disabled = used || actions?.disabledReason != null
  const use = (action: () => void) => () => {
    if (disabled) return
    setUsed(true)
    action()
  }
  return (
    <div className="flex flex-wrap items-center gap-x-3 gap-y-2 border px-3 py-2 text-sm">
      <span className="flex min-w-0 flex-1 items-center gap-2">
        <TriangleAlertIcon className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        {t("session.turnInterrupted")}
      </span>
      {actions && (
        <span className="flex gap-2">
          <DisabledReason reason={actions.disabledReason}>
            <Button variant="outline" size="sm" disabled={disabled} onClick={use(actions.onRetry)}>
              {t("session.retry")}
            </Button>
          </DisabledReason>
          <DisabledReason reason={actions.disabledReason}>
            <Button size="sm" disabled={disabled} onClick={use(actions.onContinue)}>
              {t("session.continue")}
            </Button>
          </DisabledReason>
        </span>
      )}
    </div>
  )
}

export function EventItem({
  event,
  deviceName,
  actionsDisabledReason = null,
  interruptedActions,
  onDecide,
}: {
  event: SessionEvent
  /** Device the session's tools execute on. */
  deviceName: string
  /** Why approvals can't be decided right now; null when they can. */
  actionsDisabledReason?: string | null
  /** Only for the latest interrupted turn, while the session is interrupted. */
  interruptedActions?: InterruptedTurnActions
  onDecide: (eventId: string, decision: "approved" | "denied") => void
}) {
  const { t } = useTranslation()
  switch (event.kind) {
    case "user_message":
      return (
        <Message from="user">
          {event.attachments && event.attachments.length > 0 && (
            <Attachments variant="grid">
              {event.attachments.map((a) =>
                // An empty url means the image data was not kept after reload.
                a.url === "" ? (
                  <div
                    key={a.id}
                    title={t("session.imageNotKeptTitle", { filename: a.filename })}
                    className="flex h-24 w-40 flex-col items-center justify-center gap-1 border bg-muted px-2 text-xs text-muted-foreground"
                  >
                    <ImageOffIcon className="size-4" aria-hidden />
                    <span className="text-center leading-tight">{t("session.imageNotKept")}</span>
                  </div>
                ) : (
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
                ),
              )}
            </Attachments>
          )}
          {event.text && (
            <MessageContent className="whitespace-pre-wrap">{event.text}</MessageContent>
          )}
        </Message>
      )
    case "assistant_text":
      if (!event.text) return null
      if (event.unsaved) {
        return (
          <div className="flex flex-col gap-2">
            <Message from="assistant" className="opacity-55">
              <MessageContent>
                <MessageResponse>{event.text}</MessageResponse>
              </MessageContent>
            </Message>
            <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
              <CircleSlashIcon className="size-3 shrink-0" aria-hidden />
              {t("session.unsaved")}
            </p>
          </div>
        )
      }
      return (
        <Message from="assistant">
          <MessageContent>
            <MessageResponse isAnimating={event.streaming}>{event.text}</MessageResponse>
          </MessageContent>
        </Message>
      )
    case "turn_stopped":
      return (
        <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <SquareIcon className="size-2.5 shrink-0" aria-hidden />
          {t("session.turnStopped")}
        </p>
      )
    case "turn_interrupted":
      return <InterruptedTurnRow actions={interruptedActions} />
    case "history_gap":
      return (
        <div className="flex items-center gap-3 text-xs text-muted-foreground">
          <span className="h-0 flex-1 border-t border-dashed" aria-hidden />
          <span className="max-w-[80%] text-center text-pretty">{t("session.historyGap")}</span>
          <span className="h-0 flex-1 border-t border-dashed" aria-hidden />
        </div>
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
            deviceName={deviceName}
            disabledReason={actionsDisabledReason}
            onDecide={(d) => onDecide(event.id, d)}
          />
        )
      }
      return <ToolCall event={event} />
  }
}
