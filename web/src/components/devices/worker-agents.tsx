import { BotIcon } from "lucide-react"
import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from "react"
import { useTranslation } from "react-i18next"

import { errorMessage, isApiError } from "@/api/errors"
import { AGENT_ANSWER_WAIT_MS, answeredReport, useSetWorkerAgent, useWorkerAgentStates } from "@/api/hooks"
import type { AgentKind, Worker, WorkerAgent } from "@/api/types"
import { InfoTip } from "@/components/info-tip"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Spinner } from "@/components/ui/spinner"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { i18n } from "@/i18n/i18n"
import {
  AGENT_KINDS,
  agentActions,
  agentOf,
  formatBytes,
  installProgress,
  installs,
  lastReportedAt,
  pendingWish,
  shownError,
  shownState,
  type AgentAction,
  type AgentContext,
} from "@/lib/agents"
import { formatDateTime } from "@/lib/time"
import { cn } from "@/lib/utils"

/**
 * The agents of a worker (each one this client knows, whether or not the
 * worker reported it), with what the worker last reported and the user's
 * wish, and the buttons that change the wish; then, for an offline worker,
 * how long ago it reported them (`reportedAgo`, unset for an online one).
 */
export function WorkerAgents({ worker, reportedAgo }: { worker: Worker; reportedAgo: string | undefined }) {
  const { t } = useTranslation()
  const reportedAt = lastReportedAt(worker)
  return (
    <div className="col-span-3 col-start-2 flex min-w-0 flex-col gap-1 pt-0.5">
      <ul aria-label={t("devices.agents.listLabel", { worker: worker.name })} className="flex min-w-0 flex-col gap-2">
        {AGENT_KINDS.map((kind) => {
          const entry = agentOf(worker, kind)
          return entry ? (
            <AgentItem key={kind} worker={worker} agent={entry} />
          ) : (
            <AbsentAgent key={kind} worker={worker} agent={kind} />
          )
        })}
      </ul>
      {reportedAgo !== undefined && reportedAt > 0 && (
        <InfoTip content={formatDateTime(reportedAt)} side="top" align="start">
          <time
            tabIndex={0}
            dateTime={new Date(reportedAt).toISOString()}
            className="w-fit cursor-default rounded-sm pl-5 text-xs text-muted-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            {t("devices.agents.lastReported", { ago: reportedAgo })}
          </time>
        </InfoTip>
      )}
    </div>
  )
}

const STATE_TONES: Record<WorkerAgent["state"], string> = {
  unknown: "text-muted-foreground",
  not_installed: "text-muted-foreground",
  installing: "text-foreground",
  installed: "text-foreground",
  outdated: "text-amber-700 dark:text-amber-400",
  failed: "text-destructive",
  uninstalling: "text-foreground",
}

/** The agent's name, and what the worker says about it. */
function AgentLabel({ name, id, children }: { name: string; id: string; children: ReactNode }) {
  return (
    <span className="flex min-w-0 items-center gap-1.5 text-xs">
      <BotIcon className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
      <span id={id} className="text-foreground">
        {name}
      </span>
      {children}
    </span>
  )
}

/**
 * An agent the worker has no entry for: neither reported nor asked for.
 * A worker that ever connected reported every agent it supports, so it
 * doesn't support this one; one that never did hasn't reported yet.
 * Either way an install would be refused.
 */
function AbsentAgent({ worker, agent }: { worker: Worker; agent: AgentKind }) {
  const { t } = useTranslation()
  const id = useId()
  const name = t(`devices.agents.names.${agent}`)
  const reported = worker.online || worker.lastSeenUnixMs > 0
  return (
    <li
      className="flex min-w-0 flex-col gap-1.5"
      data-agent={agent}
      data-agent-state={reported ? "unsupported" : "unknown"}
      aria-labelledby={`${id}-name`}
    >
      <AgentLabel name={name} id={`${id}-name`}>
        <span className="text-muted-foreground">
          {reported ? t("devices.agents.states.notSupported") : t("devices.agents.states.unknown")}
        </span>
      </AgentLabel>
      <p className="pl-5 text-xs text-muted-foreground">
        {reported ? t("devices.agents.unsupportedHint", { agent: name }) : t("devices.agents.unknownHint")}
      </p>
    </li>
  )
}

/**
 * One agent of the worker. A change in flight marks its button busy and
 * holds the others; it lives in the mutation cache, so it survives leaving
 * the page. Its failure shows until the next try, or until the list shows
 * that what was asked happened after all (its answer was lost). The
 * buttons keep their places, so focus stays on the one pressed while its
 * action changes; when it goes, focus moves to the agent's item.
 */
function AgentItem({ worker, agent: a }: { worker: Worker; agent: WorkerAgent }) {
  const { t } = useTranslation()
  const id = useId()
  const name = t(`devices.agents.names.${a.agent}`)
  const set = useSetWorkerAgent(worker, a.agent)
  const changes = useWorkerAgentStates(worker.workerId, a.agent)
  const pending = changes.findLast((c) => c.status === "pending")
  const latest = changes.at(-1)
  // Render again when the latest answer stops counting as not acted on.
  const answeredAt = changes.findLast((c) => c.status === "success")?.submittedAt
  const [, expired] = useState(0)
  useEffect(() => {
    if (answeredAt === undefined) return
    const left = answeredAt + AGENT_ANSWER_WAIT_MS - Date.now()
    if (left <= 0) return
    const timer = setTimeout(() => expired((n) => n + 1), left)
    return () => clearTimeout(timer)
  }, [answeredAt])
  const ctx: AgentContext = { online: worker.online, answeredReportAt: answeredReport(changes, a.agent) }
  const failure =
    latest?.status === "error" && !isApiError(latest.error, "not_found") && !applied(a, ctx, latest.variables!.install)
      ? agentErrorMessage(latest.error, latest.variables!.install, a.agent)
      : null
  const [confirming, setConfirming] = useState(false)
  // The confirmation is mounted once first asked for (and kept, for its
  // closing animation), not with every row.
  const [asked, setAsked] = useState(false)
  const focus = useOverlayFocus<HTMLButtonElement>()
  // Set at once, so a second click before the mutation cache catches up
  // sends nothing.
  const sending = useRef(false)

  const item = useRef<HTMLLIElement>(null)
  // Set when a button that had focus goes. React runs a ref's cleanup
  // before it takes the element out of the page, so focus is still on it.
  const lostFocus = useRef(false)
  const button = (action: AgentAction) => (node: HTMLButtonElement | null) => {
    if (action === "uninstall") focus.triggerRef.current = node
    if (!node) return
    return () => {
      if (document.activeElement === node) lostFocus.current = true
      if (focus.triggerRef.current === node) focus.triggerRef.current = null
    }
  }
  useLayoutEffect(() => {
    if (!lostFocus.current) return
    lostFocus.current = false
    if (!item.current?.contains(document.activeElement)) item.current?.focus({ preventScroll: true })
  })

  // Only through press (or the confirmation press opened), which holds every request while one is under way.
  const request = (action: AgentAction) => {
    sending.current = true
    set.mutate({ install: installs(action) }, { onSettled: () => (sending.current = false) })
  }
  // `clicks` is the click's detail: 2 for the second click of a double
  // click, which lands on whatever the first one's answer put in its place
  // (Install becomes Cancel install), so it is ignored.
  const press = (action: AgentAction, clicks: number) => {
    if (pending || sending.current || clicks > 1) return
    if (action !== "uninstall") return request(action)
    focus.onOpenChange(true)
    setAsked(true)
    setConfirming(true)
  }
  const closeConfirm = (open: boolean) => {
    focus.onOpenChange(open)
    setConfirming(open)
  }

  // An offline worker's "installing" or "uninstalling" is an old report, not work under way.
  const working = worker.online && (a.state === "installing" || a.state === "uninstalling")
  const actions = agentActions(a, ctx)
  const state = shownState(a)
  return (
    <li
      ref={item}
      tabIndex={-1}
      className="flex min-w-0 flex-col gap-1.5 rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
      data-agent={a.agent}
      data-agent-state={state}
      aria-labelledby={`${id}-name`}
    >
      <div className="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1.5">
        <AgentLabel name={name} id={`${id}-name`}>
          <span aria-live="polite" className={cn("flex items-center gap-1", STATE_TONES[state])}>
            {working && <Spinner className="size-3" aria-hidden />}
            {t(`devices.agents.states.${state}`)}
          </span>
          {a.state === "installed" && <span className="text-muted-foreground">{a.installedVersion}</span>}
          {a.state === "installing" && a.targetVersion && <span className="text-muted-foreground">{a.targetVersion}</span>}
        </AgentLabel>
        {actions.length > 0 && (
          <span className="ml-auto flex shrink-0 items-center gap-1.5">
            {actions.map((action, i) => {
              const busy = pending !== undefined && installs(action) === pending.variables!.install
              return (
                <Button
                  // By place, not action: the pressed button stays (and keeps focus) when its action changes.
                  key={i}
                  ref={button(action)}
                  variant={i === 0 && !QUIET_ACTIONS.has(action) ? "outline" : "ghost"}
                  size="xs"
                  aria-label={t(`devices.agents.actionLabels.${action}`, { agent: name, worker: worker.name })}
                  aria-busy={busy}
                  // Not `disabled`: a browser takes focus off a button as it
                  // becomes disabled, and a keyboard user would lose their place.
                  aria-disabled={pending !== undefined}
                  className={cn(
                    "aria-disabled:cursor-not-allowed aria-disabled:opacity-50",
                    action === "uninstall" && "text-muted-foreground hover:text-destructive",
                  )}
                  onClick={(event) => press(action, event.detail)}
                >
                  {busy && <Spinner aria-hidden />}
                  {t(`devices.agents.actions.${action}`)}
                </Button>
              )
            })}
          </span>
        )}
      </div>
      <AgentDetail worker={worker} agent={a} ctx={ctx} name={name} />
      {/* Says when a wish starts or stops waiting for the worker. Out of the flow, so it adds no gap. */}
      <span aria-live="polite" className="sr-only">
        {wishText(pendingWish(a, ctx), worker.online)}
      </span>
      {failure && (
        <p role="alert" className="text-xs text-destructive">
          {failure}
        </p>
      )}
      {asked && (
        <Dialog open={confirming} onOpenChange={closeConfirm}>
          <DialogContent
            showCloseButton={false}
            onCloseAutoFocus={(event) => {
              focus.onCloseAutoFocus(event)
              // The Uninstall button is gone once the uninstall was accepted.
              if (!item.current?.contains(document.activeElement)) item.current?.focus({ preventScroll: true })
            }}
          >
            <DialogHeader>
              <DialogTitle>{t("devices.agents.uninstallTitle", { agent: name, worker: worker.name })}</DialogTitle>
              <DialogDescription>{t("devices.agents.uninstallBody", { agent: name })}</DialogDescription>
            </DialogHeader>
            <DialogFooter>
              <DialogClose asChild>
                <Button variant="outline">{t("common.cancel")}</Button>
              </DialogClose>
              <Button
                variant="destructive"
                onClick={() => {
                  closeConfirm(false)
                  request("uninstall")
                }}
              >
                {t("devices.agents.actions.uninstall")}
              </Button>
            </DialogFooter>
          </DialogContent>
        </Dialog>
      )}
    </li>
  )
}

/** Actions that undo or give up rather than do: never the outlined main button. */
const QUIET_ACTIONS: ReadonlySet<AgentAction> = new Set<AgentAction>(["uninstall", "cancel", "dismiss"])

/** The wish the worker hasn't acted on, as one line; empty when there is none. */
function wishText(wish: "install" | "uninstall" | null, online: boolean): string {
  if (!wish) return ""
  const key = wish === "install" ? "Install" : "Uninstall"
  return online ? i18n.t(`devices.agents.pending${key}`) : i18n.t(`devices.agents.pending${key}Offline`)
}

/**
 * The lines under the agent: progress (or, offline, that the worker went
 * away during it), the hint for an outdated or unknown state, the
 * failure, and the wish the worker hasn't acted on.
 */
function AgentDetail({
  worker,
  agent: a,
  ctx,
  name,
}: {
  worker: Worker
  agent: WorkerAgent
  ctx: AgentContext
  name: string
}) {
  const { t } = useTranslation()
  const wish = pendingWish(a, ctx)
  const error = shownError(a)
  const lines = []
  if (a.state === "installing") lines.push(<Progress key="progress" agent={a} name={name} live={worker.online} />)
  if (!worker.online && (a.state === "installing" || a.state === "uninstalling")) {
    lines.push(<span key="stale">{t("devices.agents.staleHint")}</span>)
  }
  if (a.state === "unknown") lines.push(<span key="unknown">{t("devices.agents.unknownHint")}</span>)
  if (a.state === "outdated" && wish === null) {
    lines.push(
      <span key="outdated">
        {t("devices.agents.outdatedHint", { installed: a.installedVersion, target: a.targetVersion })}
      </span>,
    )
  }
  if (error) {
    lines.push(
      <span key="error" className="font-mono break-words whitespace-pre-wrap text-destructive">
        {error}
      </span>,
    )
  }
  if (wish) {
    lines.push(
      // Announced by AgentItem's live region; hidden from assistive technology here.
      <span key="wish" aria-hidden className="flex items-center gap-1.5 text-foreground">
        {worker.online && <Spinner className="size-3" aria-hidden />}
        {wishText(wish, worker.online)}
      </span>,
    )
  }
  if (lines.length === 0) return null
  return <div className="flex min-w-0 flex-col gap-1 pl-5 text-xs text-muted-foreground">{lines}</div>
}

/**
 * The download's progress: a bar and the bytes, or (total unknown) the
 * bytes so far under a moving bar; it moves only while the worker is
 * online (`live`).
 */
function Progress({ agent: a, name, live }: { agent: WorkerAgent; name: string; live: boolean }) {
  const { t } = useTranslation()
  const percent = installProgress(a)
  const done = formatBytes(a.downloadedBytes)
  const text =
    percent === null
      ? t("devices.agents.progressUnknownTotal", { done })
      : t("devices.agents.progress", { done, total: formatBytes(a.totalBytes), percent })
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <div
        role="progressbar"
        aria-label={t("devices.agents.progressLabel", { agent: name })}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={percent ?? undefined}
        aria-valuetext={text}
        className="relative h-1 w-full max-w-64 overflow-hidden bg-muted"
      >
        <div
          className={cn(
            "absolute inset-y-0 left-0 bg-primary transition-[width]",
            percent === null && "w-1/3",
            percent === null && live && "animate-pulse",
            !live && "bg-muted-foreground/50",
          )}
          style={percent === null ? undefined : { width: `${percent}%` }}
        />
      </div>
      <span className="tabular-nums">{text}</span>
    </div>
  )
}

/** The worker's agent shows that a request to install it (or not) took effect. */
const applied = (a: WorkerAgent, ctx: AgentContext, install: boolean) =>
  install ? a.state === "installing" || a.state === "installed" || pendingWish(a, ctx) === "install" : !a.wanted

/** Why a change was refused, as one line. */
function agentErrorMessage(error: unknown, install: boolean, agent: AgentKind): string {
  const name = i18n.t(`devices.agents.names.${agent}`)
  if (isApiError(error, "failed_precondition")) {
    return install ? i18n.t("devices.agents.neverReported") : i18n.t("devices.agents.holdsSession", { agent: name })
  }
  if (isApiError(error, "invalid_argument")) return i18n.t("devices.agents.unsupported", { agent: name })
  return errorMessage(error)
}
