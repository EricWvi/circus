import {
  CheckIcon,
  CircleAlertIcon,
  CircleSlashIcon,
  FileWarningIcon,
  FolderCheckIcon,
  FolderClockIcon,
  FolderSyncIcon,
  FolderXIcon,
  GitMergeIcon,
  SquareIcon,
  TriangleAlertIcon,
} from "lucide-react"
import { useState } from "react"
import { useTranslation } from "react-i18next"

import type { WorkspaceStatus } from "@/api/types"
import { Message, MessageContent, MessageResponse } from "@/components/ai-elements/message"
import { Reasoning, ReasoningContent, ReasoningTrigger } from "@/components/ai-elements/reasoning"
import { DisabledReason } from "@/components/disabled-reason"
import { DecisionLinks, DecisionRefArea } from "@/components/ledger/decision-links"
import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"
import type {
  ApprovalRequest,
  LedgerEvent,
  NoteEvent,
  NoteKind,
  SendFailure,
  SessionEvent,
  UserMessageEvent,
} from "@/lib/types"
import { workspaceEventText } from "@/lib/workspace"

import { ApprovalCard } from "./approval-card"
import { NoteDisclosure } from "./note-disclosure"
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
 * What the worker told Claude: one quiet row, collapsed, that opens to the
 * exact text Claude was sent. A note without text is just the row.
 */
function NoteRow({ event }: { event: NoteEvent }) {
  const { t } = useTranslation()
  return <NoteDisclosure label={t(noteKeys[event.noteKind])} text={event.text} />
}

/** A recorded change of the session's folder: one quiet line in the conversation. */
export function WorkspaceRow({ workspace }: { workspace: WorkspaceStatus }) {
  let icon = <FolderClockIcon className="mt-px size-3 shrink-0" aria-hidden />
  if (workspace.state === "ready") icon = <FolderCheckIcon className="mt-px size-3 shrink-0" aria-hidden />
  else if (workspace.state === "preparing") icon = <FolderSyncIcon className="mt-px size-3 shrink-0" aria-hidden />
  else if (workspace.state === "failed") icon = <FolderXIcon className="mt-px size-3 shrink-0 text-destructive" aria-hidden />
  return (
    <p className="flex items-start gap-1.5 text-xs text-muted-foreground">
      {icon}
      <span className="min-w-0 [overflow-wrap:anywhere]">{workspaceEventText(workspace)}</span>
    </p>
  )
}

/** A recorded merge or discard of the session's Ledger changes: one quiet line, the new decisions linked. */
export function LedgerRow({ event, projectId }: { event: LedgerEvent; projectId: string }) {
  const { t } = useTranslation()
  if (event.change === "discarded") {
    return (
      <p className="flex items-start gap-1.5 text-xs text-muted-foreground" data-ledger-event="discarded">
        <CircleSlashIcon className="mt-px size-3 shrink-0" aria-hidden />
        <span className="min-w-0 [overflow-wrap:anywhere]">
          {event.reason
            ? t("session.ledgerDiscarded", { reason: event.reason })
            : t("session.ledgerDiscardedNoReason")}
        </span>
      </p>
    )
  }
  return (
    <DecisionRefArea>
      <p className="flex items-start gap-1.5 text-xs text-muted-foreground" data-ledger-event="merged">
        <GitMergeIcon className="mt-px size-3 shrink-0" aria-hidden />
        <span className="min-w-0 [overflow-wrap:anywhere]">
          {event.numbers.length > 0 ? (
            <>
              {t("session.ledgerMerged")}
              <DecisionLinks projectId={projectId} numbers={event.numbers} />
            </>
          ) : (
            t("session.ledgerMergedNoNumbers")
          )}
        </span>
      </p>
    </DecisionRefArea>
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
  projectId,
  deviceName,
  actionsDisabledReason = null,
  approvalDisabledReason = () => actionsDisabledReason,
  interruptedActions,
  onRetrySend,
  onDecide,
}: {
  event: SessionEvent
  /** The session's project, whose Ledger decisions it links to. */
  projectId: string
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
    case "workspace":
      return <WorkspaceRow workspace={event.status} />
    case "ledger":
      return <LedgerRow event={event} projectId={projectId} />
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
