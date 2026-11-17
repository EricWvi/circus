import { MessageSquareDashedIcon, MonitorOffIcon } from "lucide-react"
import { useEffect, useRef } from "react"
import { useTranslation } from "react-i18next"
import { Link } from "react-router"
import { useStickToBottomContext } from "use-stick-to-bottom"

import { useDevices } from "@/api/hooks"
import type { Project, SessionSummary } from "@/api/types"
import {
  Conversation,
  ConversationContent,
  ConversationEmptyState,
  ConversationScrollButton,
} from "@/components/ai-elements/conversation"
import { Shimmer } from "@/components/ai-elements/shimmer"
import { PageHeader } from "@/components/page-header"
import { StatusBadge } from "@/components/status-dot"
import { lastInputWasKeyboard, lastInputWasTouch } from "@/lib/input-modality"
import { projectPath } from "@/lib/tree"
import { latestInterruptId } from "@/lib/turns"
import { useConversations } from "@/state/conversations-context"

import { ConnectionBanner } from "./connection-banner"
import { EventItem } from "./event-list"
import { SessionPrompt } from "./session-prompt"
import { BrokenCard, DeviceOfflineNotice, StoppedNotice } from "./session-notices"
import { SimulateMenu } from "./simulate-menu"

export function SessionView({
  project,
  session,
}: {
  project: Project
  session: SessionSummary
}) {
  const { t } = useTranslation()
  const {
    events: eventsOf,
    send,
    stop,
    decideApproval,
    retryTurn,
    continueWithoutHistory,
    stopSession,
    connection,
    lossNoticed,
    retryNow,
  } = useConversations()
  const devices = useDevices()
  const composer = useRef<HTMLDivElement>(null)
  // After an action that hands the turn back to the user: a ring only when
  // the action came from the keyboard, and not at all after a touch, where
  // focusing a text field would pop up the on-screen keyboard.
  const focusComposer = () => {
    if (lastInputWasTouch()) return
    composer.current
      ?.querySelector("textarea")
      ?.focus({ preventScroll: true, focusVisible: lastInputWasKeyboard() } as FocusOptions)
  }
  const events = eventsOf(session.sessionId)
  const last = events.at(-1)
  const streamingText = last?.kind === "assistant_text" && last.streaming && last.text !== ""
  const { state, sessionId } = session
  const busy = state === "running"
  const showWorking = busy && !streamingText && last?.kind !== "tool_call"

  const device = devices.data?.find((d) => d.name === session.device)
  const deviceOffline = device?.online === false
  // Anything that needs the server waits for the connection.
  const disconnected = connection.kind === "connected" ? null : t("session.connection.disconnected")
  const interruptId = state === "interrupted" ? latestInterruptId(events) : undefined

  // At most one notice above the composer: stopped (instead of it) > broken > device offline.
  let notice = null
  if (state === "broken") {
    notice = (
      <BrokenCard
        disabledReason={disconnected}
        onContinueWithout={() => continueWithoutHistory(sessionId)}
        onStopSession={() => stopSession(sessionId)}
        onContinued={focusComposer}
      />
    )
  } else if (state !== "stopped" && device && deviceOffline) {
    notice = <DeviceOfflineNotice device={device} />
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        actions={
          <>
            <span className="hidden shrink-0 items-center gap-1.5 text-xs text-muted-foreground md:flex">
              {t("session.onDevice", { device: session.device })}
              {deviceOffline && <OfflineMark device={session.device} />}
            </span>
            {deviceOffline && (
              <span className="flex shrink-0 md:hidden">
                <OfflineMark device={session.device} />
              </span>
            )}
            <SimulateMenu session={session} />
          </>
        }
      >
        <Link
          to={projectPath(project.projectId)}
          className="hidden shrink-0 rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50 sm:inline"
        >
          {project.name}
        </Link>
        <span className="hidden text-muted-foreground sm:inline" aria-hidden>
          /
        </span>
        <h1 className="min-w-0 truncate font-medium">{session.title}</h1>
        <StatusBadge state={state} />
      </PageHeader>

      <ConnectionBanner connection={connection} noticed={lossNoticed} onRetryNow={retryNow} />

      <Conversation className="min-h-0">
        <ConversationContent className="mx-auto w-full max-w-3xl gap-6 px-4 py-6">
          {events.length === 0 ? (
            <ConversationEmptyState
              className="min-h-[50vh]"
              icon={<MessageSquareDashedIcon className="size-8" />}
              title={t("session.noMessages")}
              description={t("session.noMessagesHint", { device: session.device })}
            />
          ) : (
            events.map((event) => (
              <EventItem
                key={event.id}
                event={event}
                deviceName={session.device}
                actionsDisabledReason={disconnected}
                interruptedActions={
                  event.id === interruptId
                    ? {
                        onRetry: () => {
                          retryTurn(sessionId)
                          focusComposer()
                        },
                        onContinue: () => {
                          send(sessionId, t("session.continuePrompt"), [])
                          focusComposer()
                        },
                        disabledReason: disconnected,
                      }
                    : undefined
                }
                onDecide={(eventId, decision) => decideApproval(sessionId, eventId, decision)}
              />
            ))
          )}
          {showWorking && (
            <div className="text-sm">
              <Shimmer>{t("session.working")}</Shimmer>
            </div>
          )}
        </ConversationContent>
        <ConversationScrollButton aria-label={t("session.scrollToLatest")} />
        <KeepBottomOnShrink />
      </Conversation>

      <div ref={composer} className="mx-auto flex w-full max-w-3xl shrink-0 flex-col gap-2 px-4 pb-4">
        {notice}
        {state === "stopped" ? (
          <StoppedNotice />
        ) : (
          <SessionPrompt
            busy={busy}
            sendDisabledReason={disconnected}
            disabledReason={state === "broken" ? t("session.broken.composerDisabled") : null}
            onSend={(text, attachments) => send(sessionId, text, attachments)}
            onStop={() => stop(sessionId)}
          />
        )}
      </div>
    </div>
  )
}

/**
 * When a banner or notice takes room from the conversation, a view that was
 * at the bottom stays there, so the latest messages don't slide out of sight.
 */
function KeepBottomOnShrink() {
  const { scrollRef } = useStickToBottomContext()
  useEffect(() => {
    const element = scrollRef.current
    if (!element) return
    let height = element.clientHeight
    const observer = new ResizeObserver(() => {
      const before = height
      height = element.clientHeight
      const wasAtBottom = element.scrollHeight - element.scrollTop - before <= 8
      if (height < before && wasAtBottom) element.scrollTop = element.scrollHeight
    })
    observer.observe(element)
    return () => observer.disconnect()
  }, [scrollRef])
  return null
}

/** "offline" next to the session's device; names the device for assistive technology. */
function OfflineMark({ device }: { device: string }) {
  const { t } = useTranslation()
  return (
    <span
      className="inline-flex items-center gap-1 border px-1 text-xs leading-4 text-muted-foreground"
      title={t("session.deviceOfflineMark", { device })}
    >
      <MonitorOffIcon className="size-3" aria-hidden />
      <span aria-hidden>{t("common.offline")}</span>
      <span className="sr-only">{t("session.deviceOfflineMark", { device })}</span>
    </span>
  )
}
