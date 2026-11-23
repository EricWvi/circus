import { FileWarningIcon, HourglassIcon, MessageSquareDashedIcon, MonitorOffIcon } from "lucide-react"
import { useEffect, useMemo, useReducer, useRef, useState } from "react"
import { useTranslation } from "react-i18next"
import { Link } from "react-router"
import { useStickToBottomContext } from "use-stick-to-bottom"

import { errorMessage, isApiError } from "@/api/errors"
import { i18n } from "@/i18n/i18n"
import {
  useChangeSessionProvider,
  useDevices,
  useInterrupt,
  useModelProviders,
  useRecoverSession,
  useResolveApproval,
  useRetryWorkspace,
  useStopSession,
} from "@/api/hooks"
import type { ModelProvider, Project, QueueReason, SessionDetails, WorkspaceStatus } from "@/api/types"
import type { ApprovalRequest } from "@/lib/types"
import {
  Conversation,
  ConversationContent,
  ConversationEmptyState,
  ConversationScrollButton,
} from "@/components/ai-elements/conversation"
import { Shimmer } from "@/components/ai-elements/shimmer"
import { SessionProviderPicker } from "@/components/model-providers/provider-picker"
import { PageHeader } from "@/components/page-header"
import { StatusBadge, WorkspaceBadge } from "@/components/status-dot"
import { lastInputWasKeyboard, lastInputWasTouch } from "@/lib/input-modality"
import { currentQueueReason, fixedOnProvidersPage, queueReasonText } from "@/lib/queue-reason"
import { withOutbox } from "@/lib/session-events/outbox"
import { approvalLapsed, turnRunning } from "@/lib/session-state"
import { sessionTitle } from "@/lib/title"
import { projectPath } from "@/lib/tree"
import { interruptedTurnInput, latestInterruptId } from "@/lib/turns"
import { useOutbox, useOutboxMessages } from "@/state/outbox-context"
import { currentWorkerDisconnectedAt } from "@/lib/worker-link"
import { currentWorkspace, workspaceBlocks, workspaceEventText } from "@/lib/workspace"
import { useSessionWatch } from "@/state/use-session-watch"

import { ConnectionBanner } from "./connection-banner"
import { EventItem } from "./event-list"
import { SessionMenu } from "./session-menu"
import { BrokenCard, DeviceOfflineNotice, GoneNotice, StoppedNotice, WorkerDisconnectedNotice } from "./session-notices"
import { SessionPrompt } from "./session-prompt"
import { WorkspaceBanner, WorkspaceComposerNote } from "./workspace-banner"

export function SessionView({ project, session }: { project: Project; session: SessionDetails }) {
  const { t } = useTranslation()
  const { sessionId } = session
  const watch = useSessionWatch(sessionId)
  const { transcript, connection } = watch
  const outbox = useOutbox()
  const outboxMessages = useOutboxMessages(sessionId)
  const events = useMemo(() => withOutbox(transcript.events, outboxMessages), [transcript.events, outboxMessages])

  // Messages the stream shows now leave the outbox (also once a send that
  // the stream answered first has finished).
  useEffect(() => {
    if (outboxMessages.length === 0) return
    const shown = new Set<string>()
    for (const e of transcript.events) if (e.kind === "user_message" && e.clientMsgId) shown.add(e.clientMsgId)
    outbox.forget(sessionId, shown)
  }, [outbox, sessionId, transcript.events, outboxMessages])

  const interrupt = useInterrupt()
  const stopSession = useStopSession()
  const recover = useRecoverSession()
  const resolveApproval = useResolveApproval()
  const retryWorkspace = useRetryWorkspace()
  const providers = useModelProviders()
  const changeProvider = useChangeSessionProvider()
  const [providerOpen, setProviderOpen] = useState(false)
  // The last action that failed, shown until the next one.
  const [actionError, setActionError] = useState<unknown>(null)
  const act = <Req,>(mutation: { mutate: (req: Req, options: { onError: (e: unknown) => void }) => void }, req: Req) => {
    setActionError(null)
    mutation.mutate(req, { onError: setActionError })
  }

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

  const send = (text: string) => {
    setActionError(null)
    // The message shows in the conversation, failed or not; nothing to do here.
    outbox.send(sessionId, text).catch(() => {})
  }

  const busy = turnRunning(transcript, session)
  // A pending approval is what the user has to act on, whatever runs.
  const state = busy && session.state !== "awaiting_approval" ? "running" : session.state
  const last = events.at(-1)
  const streamingText = last?.kind === "assistant_text" && last.streaming && last.text !== ""
  const lastIsTool = last?.kind === "tool_call" && (last.status === "running" || last.status === "awaiting_approval")
  const showWorking = busy && !streamingText && !lastIsTool
  // The folder isn't ready: no turn runs, messages wait. The latest of
  // GetSession and the stream says how far it is.
  const workspace = currentWorkspace(session.workspace, transcript.workspace)
  const waitsForFolder = workspaceBlocks(workspace) && session.state !== "stopped" && !watch.gone
  const waiting = !busy && session.state === "queued" && !waitsForFolder
  const queueReason = currentQueueReason(session.queueReason, session.readSentAtMs, transcript.queue)
  const provider = providers.data?.find((p) => p.providerId === session.providerId)
  const announcement = useWorkspaceAnnouncement(workspace, waitsForFolder)

  const device = devices.data?.find((d) => d.name === session.device)
  const deviceOffline = device?.online === false
  // The worker holding the lease lost its connection; it has 40 to 60
  // seconds to come back before the lease (and a running turn) ends.
  const workerDisconnectedAt = currentWorkerDisconnectedAt(
    session.workerDisconnectedAtUnixMs,
    session.readSentAtMs,
    transcript.worker,
  )
  // Anything that needs the server waits for the connection, once its loss
  // is noticed: the brief reconnect when the server ends a stream (about
  // once an hour) doesn't block anything, and calls don't need the stream.
  const disconnected =
    connection.kind !== "connected" && watch.lossNoticed ? t("session.connection.disconnected") : null
  const interruptId = session.state === "interrupted" ? latestInterruptId(events) : undefined
  const stopped = session.state === "stopped"
  // The provider changes only between turns; it takes effect when the next CLI starts.
  const providerBlocked =
    busy || session.serverState === "running" ? t("session.provider.blockedRunning") : disconnected
  // A change that became disallowed while the menu was open closes it.
  if (providerOpen && (providerBlocked !== null || stopped || watch.gone)) setProviderOpen(false)

  // Approvals that can't be decided any more: the session stopped, or the
  // request lapsed (`approvalLapsed`; the server records no event for that).
  const pendingApprovals = events.some((e) => e.kind === "tool_call" && e.status === "awaiting_approval")
  const now = useNow(pendingApprovals ? 15_000 : null)
  const approvalBlocked = (approval: ApprovalRequest) => {
    if (stopped) return t("session.approval.sessionStopped")
    if (approvalLapsed(approval, session, now)) return t("session.approval.expired")
    return disconnected
  }

  // At most one notice above the composer: gone > stopped (instead of it) >
  // broken > worker disconnected > device offline.
  let notice = null
  if (session.state === "broken" && !watch.gone) {
    notice = (
      <BrokenCard
        disabledReason={disconnected}
        onContinueWithout={() => act(recover, { sessionId })}
        onStopSession={() => act(stopSession, { sessionId })}
        onContinued={focusComposer}
      />
    )
  } else if (!stopped && !watch.gone && workerDisconnectedAt > 0) {
    notice = (
      <WorkerDisconnectedNotice
        disconnectedAtUnixMs={workerDisconnectedAt}
        turnRunning={busy}
        serverNow={watch.serverNow}
        clockKnown={watch.clockKnown}
      />
    )
  } else if (!stopped && device && deviceOffline && !waitsForFolder) {
    // While the folder waits, its banner already says the device is offline.
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
            <SessionProviderPicker
              className="max-w-44"
              providerId={session.providerId}
              providers={providers.data}
              stopped={stopped || watch.gone}
              disabledReason={providerBlocked}
              open={providerOpen}
              onOpenChange={setProviderOpen}
              onChange={(providerId) => {
                setActionError(null)
                changeProvider.mutate(
                  { sessionId, providerId },
                  { onError: (error) => setActionError(new ProviderChangeError(error)) },
                )
              }}
            />
            {/* On narrow screens, the folder's badge and banner already say it. */}
            {deviceOffline && !waitsForFolder && (
              <span className="flex shrink-0 md:hidden">
                <OfflineMark device={session.device} />
              </span>
            )}
            {!watch.gone && (
              <SessionMenu
                stopped={stopped}
                disabledReason={disconnected}
                onStopSession={() => act(stopSession, { sessionId })}
              />
            )}
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
        <h1 className="min-w-0 truncate font-medium">{sessionTitle(session.title)}</h1>
        {waitsForFolder && session.state !== "broken" ? (
          <WorkspaceBadge workspace={workspace} />
        ) : (
          <StatusBadge state={state} />
        )}
      </PageHeader>

      <ConnectionBanner connection={connection} noticed={watch.lossNoticed} onRetryNow={watch.retryNow} />
      <span aria-live="polite" className="sr-only" data-workspace-announcement>
        {announcement}
      </span>
      {waitsForFolder && (
        <WorkspaceBanner
          workspace={workspace}
          serverNow={watch.serverNow}
          retryDisabledReason={disconnected}
          retrying={retryWorkspace.isPending}
          onRetry={() => {
            setActionError(null)
            retryWorkspace.mutate({ sessionId }, { onError: setActionError, onSuccess: focusComposer })
          }}
        />
      )}

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
                approvalDisabledReason={approvalBlocked}
                interruptedActions={
                  event.id === interruptId
                    ? {
                        onRetry: () => {
                          const text = interruptedTurnInput(events)
                          if (text !== null) send(text)
                          focusComposer()
                        },
                        onContinue: () => {
                          send(t("session.continuePrompt"))
                          focusComposer()
                        },
                        disabledReason: disconnected,
                      }
                    : undefined
                }
                onRetrySend={(clientMsgId) => {
                  outbox.retry(clientMsgId).catch(() => {})
                }}
                onDecide={(approvalId, approve) => act(resolveApproval, { sessionId, approvalId, approve })}
              />
            ))
          )}
          {showWorking && (
            <div className="text-sm">
              <Shimmer>{t("session.working")}</Shimmer>
            </div>
          )}
          {waiting && (
            <QueueNotice
              reason={queueReason}
              provider={provider}
              onChangeProvider={providerBlocked === null ? () => setProviderOpen(true) : undefined}
            />
          )}
          {session.viewTruncated && (
            <p className="flex items-start gap-1.5 text-xs text-muted-foreground">
              <FileWarningIcon className="mt-px size-3 shrink-0" aria-hidden />
              {t("session.viewFull")}
            </p>
          )}
        </ConversationContent>
        <ConversationScrollButton aria-label={t("session.scrollToLatest")} />
        <KeepBottomOnShrink />
      </Conversation>

      <div ref={composer} className="mx-auto flex w-full max-w-3xl shrink-0 flex-col gap-2 px-4 pb-4">
        {actionError !== null && (
          <p role="alert" className="px-1 text-sm text-destructive">
            {actionErrorMessage(actionError, stopped)}
          </p>
        )}
        {notice}
        {watch.gone ? (
          <GoneNotice />
        ) : stopped ? (
          <StoppedNotice />
        ) : (
          <SessionPrompt
            busy={busy}
            sendDisabledReason={disconnected}
            disabledReason={session.state === "broken" ? t("session.broken.composerDisabled") : null}
            onSend={send}
            onStop={() => act(interrupt, { sessionId })}
          />
        )}
        {waitsForFolder && <WorkspaceComposerNote />}
      </div>
    </div>
  )
}

/**
 * What a live region says about the folder: each state while the session
 * waits for it (without the ticking time), and that it is ready once it
 * became ready while the page was open. Empty on the first render, so the
 * state the page opens with is announced too.
 */
function useWorkspaceAnnouncement(workspace: WorkspaceStatus | undefined, waits: boolean): string {
  const [mounted, setMounted] = useState(false)
  const [waited, setWaited] = useState(false)
  useEffect(() => {
    const timer = window.setTimeout(() => setMounted(true), 0)
    return () => window.clearTimeout(timer)
  }, [])
  if (waits && !waited) setWaited(true)
  if (!mounted || !workspace) return ""
  if (waits || (waited && workspace.state === "ready")) return workspaceEventText(workspace)
  return ""
}

/** The time, read again every `intervalMs` while that is set. */
function useNow(intervalMs: number | null): number {
  const [now, tick] = useReducer(() => Date.now(), 0, () => Date.now())
  useEffect(() => {
    if (intervalMs === null) return
    tick()
    const timer = window.setInterval(tick, intervalMs)
    return () => window.clearInterval(timer)
  }, [intervalMs])
  return now
}

/** A failed change of the session's provider, told apart from other actions. */
class ProviderChangeError extends Error {
  constructor(readonly error: unknown) {
    super("provider change failed")
  }
}

/** Why a session action failed, by the server's answer and whether the session is stopped now. */
function actionErrorMessage(error: unknown, stopped: boolean): string {
  if (error instanceof ProviderChangeError) {
    const cause = error.error
    if (isApiError(cause, "failed_precondition")) {
      return i18n.t(stopped ? "session.provider.changeFailedStopped" : "session.provider.changeFailedRunning")
    }
    if (isApiError(cause) && cause.field === "provider_id") return i18n.t("validation.providerGone")
    return actionErrorMessage(cause, stopped)
  }
  // Stopped, no longer broken, an approval that expired: the view is behind.
  if (isApiError(error, "failed_precondition")) return i18n.t("session.actionError.stale")
  if (isApiError(error, "not_found")) return i18n.t("session.actionError.gone")
  return errorMessage(error)
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

/**
 * Why the session waits for a worker, and what to do about it: the
 * Providers page for a provider's limit, workers or secrets, another
 * provider for a deleted one.
 */
function QueueNotice({
  reason,
  provider,
  onChangeProvider,
}: {
  reason: QueueReason
  provider: ModelProvider | undefined
  onChangeProvider?: () => void
}) {
  const { t } = useTranslation()
  if (reason === "unspecified") return <p className="text-sm text-muted-foreground">{t("session.queued")}</p>
  const action = "rounded-sm text-foreground underline underline-offset-4 outline-none hover:text-primary focus-visible:ring-2 focus-visible:ring-ring/50"
  return (
    <p role="status" data-queue-reason={reason} className="flex items-start gap-2 text-sm text-muted-foreground">
      <HourglassIcon className="mt-0.5 size-3.5 shrink-0" aria-hidden />
      <span className="min-w-0">
        {queueReasonText(reason, provider)}{" "}
        {fixedOnProvidersPage(reason) && (
          <Link to="/providers" className={action}>
            {t("session.queue.openProviders")}
          </Link>
        )}
        {reason === "provider_deleted" && onChangeProvider && (
          <button type="button" className={action} onClick={onChangeProvider}>
            {t("session.provider.change")}
          </button>
        )}
      </span>
    </p>
  )
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
