import {
  CheckIcon,
  ChevronDownIcon,
  CircleAlertIcon,
  CircleSlashIcon,
  FileWarningIcon,
  MessageSquareShareIcon,
  SquareIcon,
  TriangleAlertIcon,
} from "lucide-react"
import { useLayoutEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"

import { Message, MessageContent, MessageResponse } from "@/components/ai-elements/message"
import { Reasoning, ReasoningContent, ReasoningTrigger } from "@/components/ai-elements/reasoning"
import { DisabledReason } from "@/components/disabled-reason"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Spinner } from "@/components/ui/spinner"
import type { ApprovalRequest, NoteEvent, NoteKind, SendFailure, SessionEvent, UserMessageEvent } from "@/lib/types"

import { ApprovalCard } from "./approval-card"
import { ToolCall } from "./tool-call"

/** What the row of an interrupted turn offers while the turn can still be redone. */
export interface InterruptedTurnActions {
  onRetry: () => void
  onContinue: () => void
  /** Why the buttons can't be used right now; null when they can. */
  disabledReason: string | null
}

// The message box shrinks to its text, so clipping it would cut off what
// glyphs draw past their advance (the last letter of italic text). Code
// blocks and tables scroll in their own boxes; long words wrap instead.
const ASSISTANT_CONTENT = "overflow-visible break-words"

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

const noteKeys = {
  previous_turn_interrupted: "session.note.previousTurnInterrupted",
  recovered_call: "session.note.recoveredCall",
  call_notice: "session.note.callNotice",
  job_notice: "session.note.jobNotice",
  lost_bgtask: "session.note.lostBgtask",
  history_missing: "session.note.historyMissing",
  other: "session.note.other",
} as const satisfies Record<NoteKind, string>

/**
 * The text of an opened note. Where it is too long for its box and
 * scrolls, the box takes focus, so the keyboard can scroll it too.
 */
function NoteText({ text, label }: { text: string; label: string }) {
  const ref = useRef<HTMLDivElement>(null)
  const [scrolls, setScrolls] = useState(false)
  useLayoutEffect(() => {
    const box = ref.current
    if (!box) return
    const measure = () => setScrolls(box.scrollHeight > box.clientHeight + 1)
    measure()
    if (typeof ResizeObserver === "undefined") return
    const observer = new ResizeObserver(measure)
    observer.observe(box)
    return () => observer.disconnect()
  }, [])
  return (
    <div
      ref={ref}
      tabIndex={scrolls ? 0 : undefined}
      role={scrolls ? "region" : undefined}
      aria-label={scrolls ? label : undefined}
      className="mt-2 max-h-64 overflow-y-auto rounded-md bg-muted/50 px-3 py-2 font-mono text-xs whitespace-pre-wrap text-muted-foreground outline-none [overflow-wrap:anywhere] focus-visible:ring-2 focus-visible:ring-ring/50"
    >
      {text}
    </div>
  )
}

/**
 * What the worker told Claude: one quiet row, collapsed, that opens to the
 * exact text Claude was sent. A note without text is just the row.
 */
function NoteRow({ event }: { event: NoteEvent }) {
  const { t } = useTranslation()
  const label = t(noteKeys[event.noteKind])
  const row = "flex max-w-full items-center gap-1.5 text-left text-xs text-muted-foreground"
  const icon = <MessageSquareShareIcon className="size-3 shrink-0" aria-hidden />
  if (event.text === "") {
    return (
      <p className={row}>
        {icon}
        <span className="min-w-0">{label}</span>
      </p>
    )
  }
  return (
    <Collapsible className="group/note">
      <CollapsibleTrigger
        className={`${row} rounded-sm outline-none transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50`}
      >
        {icon}
        <span className="min-w-0">{label}</span>
        <ChevronDownIcon
          className="size-3 shrink-0 transition-transform group-data-[state=open]/note:rotate-180"
          aria-hidden
        />
      </CollapsibleTrigger>
      <CollapsibleContent className="data-[state=closed]:fade-out-0 data-[state=closed]:slide-out-to-top-2 data-[state=open]:slide-in-from-top-2 outline-none data-[state=closed]:animate-out data-[state=open]:animate-in">
        <NoteText text={event.text} label={label} />
      </CollapsibleContent>
    </Collapsible>
  )
}

const failureKeys = {
  too_many: "session.delivery.tooMany",
  stopped: "session.delivery.stopped",
  unreachable: "session.delivery.unreachable",
  other: "session.delivery.other",
} as const satisfies Record<SendFailure, string>

/**
 * How far a sent message has come, quietly under its bubble: sending, sent
 * (the server has it; the agent hasn't received it yet) or failed with a
 * retry. Nothing once the agent received it.
 */
function Delivery({ event, onRetry }: { event: UserMessageEvent; onRetry: () => void }) {
  const { t } = useTranslation()
  const line = "flex items-center justify-end gap-1.5 text-xs text-muted-foreground"
  switch (event.delivery) {
    case "delivered":
      return null
    case "sending":
      return (
        <p className={line}>
          <Spinner className="size-3" aria-hidden />
          {t("session.delivery.sending")}
        </p>
      )
    case "accepted":
      return (
        <p className={line}>
          <CheckIcon className="size-3" aria-hidden />
          {t("session.delivery.accepted")}
        </p>
      )
    case "not_kept":
      return (
        <p className={`${line} text-right text-pretty`}>
          <CheckIcon className="size-3 shrink-0" aria-hidden />
          {t("session.delivery.notKept")}
        </p>
      )
    case "failed":
      return (
        <div role="alert" className="flex flex-wrap items-center justify-end gap-x-2 gap-y-1 text-xs text-destructive">
          <CircleAlertIcon className="size-3 shrink-0" aria-hidden />
          <span>{t(failureKeys[event.failure ?? "other"])}</span>
          <Button variant="outline" size="xs" onClick={onRetry}>
            {t("session.retry")}
          </Button>
        </div>
      )
  }
}

export function EventItem({
  event,
  deviceName,
  actionsDisabledReason = null,
  approvalDisabledReason = () => actionsDisabledReason,
  interruptedActions,
  onRetrySend,
  onDecide,
}: {
  event: SessionEvent
  /** Device the session's tools execute on. */
  deviceName: string
  /** Why actions can't be used right now; null when they can. */
  actionsDisabledReason?: string | null
  /** Why this approval can't be decided (any more); null when it can. */
  approvalDisabledReason?: (approval: ApprovalRequest) => string | null
  /** Only for the latest interrupted turn, while the session is interrupted. */
  interruptedActions?: InterruptedTurnActions
  /** Sends a failed message again. */
  onRetrySend: (clientMsgId: string) => void
  onDecide: (approvalId: string, approve: boolean) => void
}) {
  const { t } = useTranslation()
  switch (event.kind) {
    case "user_message":
      return (
        <div className="flex flex-col gap-1.5">
          <Message from="user">
            <MessageContent className="whitespace-pre-wrap break-words">{event.text}</MessageContent>
          </Message>
          <Delivery event={event} onRetry={() => onRetrySend(event.clientMsgId)} />
        </div>
      )
    case "assistant_text":
      if (!event.text) return null
      if (event.unsaved) {
        return (
          <div className="flex flex-col gap-2">
            <Message from="assistant" className="opacity-55">
              <MessageContent className={ASSISTANT_CONTENT}>
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
          <MessageContent className={ASSISTANT_CONTENT}>
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
        <Reasoning
          defaultOpen={false}
          isStreaming={event.streaming}
          duration={event.durationSeconds}
          className="mb-0"
        >
          <ReasoningTrigger />
          <ReasoningContent>{event.text}</ReasoningContent>
        </Reasoning>
      )
    case "turn_error":
      return (
        <p className="flex items-start gap-1.5 text-xs text-destructive">
          <CircleAlertIcon className="mt-px size-3 shrink-0" aria-hidden />
          <span className="min-w-0 break-words">{t("session.turnError", { message: event.message })}</span>
        </p>
      )
    case "note":
      return <NoteRow event={event} />
    case "truncated":
      return (
        <p className="flex items-center gap-1.5 text-xs text-muted-foreground">
          <FileWarningIcon className="size-3 shrink-0" aria-hidden />
          {t("session.truncated")}
        </p>
      )
    case "tool_call": {
      const approval = event.approval
      if (event.status === "awaiting_approval" && approval) {
        return (
          <ApprovalCard
            event={event}
            deviceName={deviceName}
            disabledReason={approvalDisabledReason(approval)}
            onDecide={(d) => onDecide(approval.approvalId, d === "approved")}
          />
        )
      }
      return <ToolCall event={event} />
    }
  }
}
