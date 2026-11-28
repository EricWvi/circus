import { ClockIcon, TriangleAlertIcon } from "lucide-react"
import { useEffect, useId, useLayoutEffect, useRef, useState, type ReactNode } from "react"
import { createPortal } from "react-dom"
import { useTranslation } from "react-i18next"

import { errorMessage, isApiError } from "@/api/errors"
import { AGENT_ANSWER_WAIT_MS, answeredReport, useSetWorkerAgent, useWorkerAgentStates } from "@/api/hooks"
import type { AgentKind, AgentState, Worker, WorkerAgent } from "@/api/types"
import { InfoTip } from "@/components/info-tip"
import { AgentMark } from "@/components/model-providers/provider-agent"
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
import { useReturnFocus } from "@/hooks/use-return-focus"
import { i18n } from "@/i18n/i18n"
import {
  AGENT_KINDS,
  LATER_AGENTS,
  agentActions,
  agentDialog,
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
 * The agents of a worker as a row of chips, like the same-machine devices
 * above them: each agent this client knows, whether or not the worker
 * reported it, then those still to come. Under them, why a request was
 * refused, and, for an offline worker, how long ago it reported its agents
 * (`reportedAgo`, unset for an online one).
 */
export function WorkerAgents({ worker, reportedAgo }: { worker: Worker; reportedAgo: string | undefined }) {
  const { t } = useTranslation()
  const reportedAt = lastReportedAt(worker)
  // Where a refused request is said: under the chips, not in them.
  const [alerts, setAlerts] = useState<HTMLDivElement | null>(null)
  return (
    <div className="col-span-3 col-start-2 flex min-w-0 flex-col gap-1.5 pt-0.5">
      <ul aria-label={t("devices.agents.listLabel", { worker: worker.name })} className="flex min-w-0 flex-wrap gap-1.5">
        {AGENT_KINDS.map((kind) => {
          const entry = agentOf(worker, kind)
          return entry ? (
            <AgentItem key={kind} worker={worker} agent={entry} alerts={alerts} />
          ) : (
            <AbsentAgent key={kind} worker={worker} agent={kind} />
          )
        })}
        {LATER_AGENTS.map((agent) => (
          <LaterAgent key={agent} agent={agent} />
        ))}
      </ul>
      <div ref={setAlerts} className="flex flex-col gap-1 empty:hidden" />
      {reportedAgo !== undefined && reportedAt > 0 && (
        <InfoTip content={formatDateTime(reportedAt)} side="top" align="start">
          <time
            tabIndex={0}
            dateTime={new Date(reportedAt).toISOString()}
            className="w-fit cursor-default rounded-sm text-xs text-muted-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            {t("devices.agents.lastReported", { ago: reportedAgo })}
          </time>
        </InfoTip>
      )}
    </div>
  )
}

/** A chip's frame: the size, border and text of the same-machine devices' chips. */
const CHIP = "relative flex h-6 max-w-full min-w-0 items-center border text-xs"
/** What a chip says, with the padding of those chips. */
const FACE = "flex min-w-0 items-center gap-1.5 self-stretch px-2"
/** A ring for keyboard focus; forced colours show an outline instead. */
const FOCUS = "outline-none focus-visible:ring-2 focus-visible:ring-ring/50 focus-visible:outline-hidden"
/** The dashed edge of an agent that isn't installed. */
const DASHED = "border-dashed border-muted-foreground/50"

/** The agent's mark and name. `muted` is AgentMark's. */
function Face({ agent, name, muted = false, children }: { agent: AgentKind | (typeof LATER_AGENTS)[number]; name: string; muted?: boolean | "always"; children?: ReactNode }) {
  return (
    <>
      <AgentMark agent={agent} muted={muted} className={cn("size-3.5", muted === "always" && "forced-colors:bg-[GrayText]")} />
      <span className="truncate">{name}</span>
      {children}
    </>
  )
}

/** An agent no worker can install yet: muted, with nothing to press. */
function LaterAgent({ agent }: { agent: (typeof LATER_AGENTS)[number] }) {
  const { t } = useTranslation()
  const name = t(`devices.agents.names.${agent}`)
  return (
    <li data-agent={agent} data-agent-state="coming_soon" className="flex min-w-0">
      {/* Not focusable: the muted chip says enough, and every worker has these. */}
      <InfoTip content={t("devices.agents.comingSoon")} side="top">
        <span className={cn(CHIP, FACE, "cursor-default border-dashed text-muted-foreground forced-colors:text-[GrayText]")}>
          <span aria-hidden className="contents">
            <Face agent={agent} name={name} muted="always" />
          </span>
          <span className="sr-only">{t("devices.agents.chipLabel", { agent: name, state: t("devices.agents.states.comingSoon") })}</span>
        </span>
      </InfoTip>
    </li>
  )
}

/**
 * An agent the worker has no entry for: neither reported nor asked for.
 * A worker that ever connected reported every agent it supports, so it
 * doesn't support this one; one that never did hasn't reported yet.
 * Either way an install would be refused: the chip only says why.
 */
function AbsentAgent({ worker, agent }: { worker: Worker; agent: AgentKind }) {
  const { t } = useTranslation()
  const hintId = useId()
  const name = t(`devices.agents.names.${agent}`)
  const reported = worker.online || worker.lastSeenUnixMs > 0
  const state = reported ? t("devices.agents.states.notSupported") : t("devices.agents.states.unknown")
  const hint = reported ? t("devices.agents.unsupportedHint", { agent: name }) : t("devices.agents.unknownHint")
  return (
    <li data-agent={agent} data-agent-state={reported ? "unsupported" : "unknown"} className="flex min-w-0">
      <InfoTip content={hint} side="top" align="start">
        <button
          type="button"
          aria-label={t("devices.agents.chipLabel", { agent: name, state })}
          aria-describedby={hintId}
          className={cn(CHIP, FACE, FOCUS, "cursor-default border-dashed text-muted-foreground")}
        >
          <Face agent={agent} name={name} muted="always" />
        </button>
      </InfoTip>
      <span id={hintId} hidden>
        {hint}
      </span>
    </li>
  )
}

/** What the dialog of an agent's chip shows. */
type Pane = "install" | "failed" | "uninstall"

/** An action offered beside the chip (the others are in its dialog). */
type SideAction = Exclude<AgentAction, "install" | "retry" | "dismiss">
const isSideAction = (action: AgentAction): action is SideAction =>
  action !== "install" && action !== "retry" && action !== "dismiss"
/**
 * Update stays out beside an outdated chip: it is what the chip asks for.
 * The others undo or take back, and come out on hover or focus.
 */
const shownAtOnce = (action: SideAction) => action === "update"

/** The chip's edge and text, by what it shows. */
const LOOKS: Partial<Record<AgentState, string>> = {
  not_installed: DASHED,
  unknown: DASHED,
  failed: "border-dashed border-destructive/60",
}

/**
 * One agent of the worker, as a chip. Not installed (or in a state this
 * client doesn't know), the chip is a button that asks to confirm the
 * install; failed, it opens the failure, to retry or give up. Otherwise the
 * chip shows the version. Update stays beside an outdated one; the other
 * actions (uninstall, taking back a wish the worker hasn't acted on) come
 * out beside it on hover or focus. What doesn't fit in it (progress, a
 * wish, why an outdated version matters) shows in a tooltip.
 *
 * A change in flight holds the others; it lives in the mutation cache, so
 * it survives leaving the page. Its failure shows under the chips until the
 * next try, or until the list shows that what was asked happened after all
 * (its answer was lost). When a button with focus goes, focus moves to the
 * chip, or to the item when the chip isn't focusable.
 */
function AgentItem({ worker, agent: a, alerts }: { worker: Worker; agent: WorkerAgent; alerts: HTMLElement | null }) {
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

  // The dialog is mounted once first asked for (and kept, with what it
  // shows, for its closing animation), not with every chip.
  const [pane, setPane] = useState<Pane | null>(null)
  const [open, setOpen] = useState(false)
  const returnFocus = useReturnFocus()
  // Set at once, so a second click before the mutation cache catches up
  // sends nothing.
  const sending = useRef(false)

  const item = useRef<HTMLLIElement>(null)
  const chip = useRef<HTMLButtonElement | null>(null)
  // Set when a button that had focus goes. React runs a ref's cleanup
  // before it takes the element out of the page, so focus is still on it.
  const lostFocus = useRef(false)
  const tracked = (node: HTMLButtonElement | null) => {
    if (!node) return
    return () => {
      if (document.activeElement === node) lostFocus.current = true
    }
  }
  const chipRef = (node: HTMLButtonElement | null) => {
    chip.current = node
    const cleanup = tracked(node)
    return () => {
      cleanup?.()
      if (chip.current === node) chip.current = null
    }
  }
  const keepFocus = () => {
    if (!item.current?.contains(document.activeElement)) (chip.current ?? item.current)?.focus({ preventScroll: true })
  }
  useLayoutEffect(() => {
    if (!lostFocus.current) return
    lostFocus.current = false
    keepFocus()
  })

  const held = () => pending !== undefined || sending.current
  // Only through a press, which holds every request while one is under way.
  const request = (action: AgentAction) => {
    sending.current = true
    set.mutate({ install: installs(action) }, { onSettled: () => (sending.current = false) })
  }
  const ask = (next: Pane, trigger: HTMLButtonElement) => {
    returnFocus.opened(trigger)
    setPane(next)
    setOpen(true)
  }
  // The dialog takes clicks while it fades out: a second one sends nothing.
  const confirm = (action: AgentAction) => {
    setOpen(false)
    if (held()) return
    request(action)
  }
  // The failed dialog opens on Retry, or on Close without it: never on
  // Don't install, which a second Enter would press.
  const initial = useRef<HTMLButtonElement>(null)
  // `clicks` is the click's detail: 2 for the second click of a double
  // click, which lands on whatever the first one's answer put in its place
  // (Cancel install becomes the chip that installs), so it is ignored.
  const pressChip = (dialog: "install" | "failed", trigger: HTMLButtonElement, clicks: number) => {
    if (held() || clicks > 1) return
    ask(dialog, trigger)
  }
  const pressSide = (action: SideAction, trigger: HTMLButtonElement, clicks: number) => {
    if (held() || clicks > 1) return
    if (action === "uninstall") return ask("uninstall", trigger)
    request(action)
  }

  const state = shownState(a)
  const wish = pendingWish(a, ctx)
  const actions = agentActions(a, ctx)
  const dialog = agentDialog(a, ctx)
  const side = dialog ? [] : actions.filter(isSideAction)
  // An offline worker's "installing" or "uninstalling" is an old report, not work under way.
  const working = worker.online && (a.state === "installing" || a.state === "uninstalling")
  const version =
    a.state === "installing" ? a.targetVersion : state === "installed" || state === "outdated" || state === "uninstalling" ? a.installedVersion : ""
  const stateText = t(`devices.agents.states.${state}`)
  const label = version
    ? t("devices.agents.chipLabelVersion", { agent: name, state: stateText, version })
    : t("devices.agents.chipLabel", { agent: name, state: stateText })
  const progress = a.state === "installing" ? progressTexts(a) : null

  // What doesn't fit in the chip: in its dialog, or else in a tooltip.
  const details: string[] = []
  if (progress) details.push(progress.full)
  if (!worker.online && (a.state === "installing" || a.state === "uninstalling")) details.push(t("devices.agents.staleHint"))
  if (a.state === "unknown") details.push(t("devices.agents.unknownHint"))
  if (a.state === "outdated" && wish === null) {
    details.push(t("devices.agents.outdatedHint", { installed: a.installedVersion, target: a.targetVersion }))
  }
  if (wish) details.push(wishText(wish, worker.online))

  const face = (
    <Face agent={a.agent} name={name} muted={!!LOOKS[state] && state !== "failed"}>
      {progress && <span className="text-muted-foreground tabular-nums">{progress.short}</span>}
      {!progress && version && (
        <span className={cn("tabular-nums", state === "outdated" ? "text-amber-700 dark:text-amber-400" : "text-muted-foreground")}>
          {version}
        </span>
      )}
      {/* Outdated is said by more than the colour; the chip's name says it in words. */}
      {state === "outdated" && <TriangleAlertIcon className="size-3 text-amber-700 dark:text-amber-400" aria-hidden />}
      {state === "failed" && <span className="text-destructive">{t("devices.agents.installFailed")}</span>}
      {working || pending || (wish && worker.online) ? (
        <Spinner className="size-3" aria-hidden />
      ) : (
        wish && <ClockIcon className="size-3 text-muted-foreground" aria-hidden />
      )}
    </Face>
  )
  const described = details.length > 0 ? `${id}-details` : undefined

  const sideButton = (action: SideAction, i: number) => (
    <Button
      // By place, not action: the pressed button stays (and keeps focus) when its action changes.
      key={i}
      ref={tracked}
      variant="ghost"
      size="xs"
      aria-label={t(`devices.agents.actionLabels.${action}`, { agent: name, worker: worker.name })}
      aria-busy={pending !== undefined && installs(action) === pending.variables!.install}
      aria-disabled={pending !== undefined}
      className={cn(
        "h-5 px-1 focus-visible:ring-2 focus-visible:ring-inset aria-disabled:cursor-not-allowed aria-disabled:opacity-50",
        action === "uninstall" && "text-muted-foreground hover:text-destructive",
      )}
      onClick={(event) => pressSide(action, event.currentTarget, event.detail)}
    >
      {t(`devices.agents.actions.${action}`)}
    </Button>
  )

  let body: ReactNode
  if (dialog) {
    body = (
      <button
        ref={chipRef}
        type="button"
        aria-label={label}
        aria-describedby={described}
        aria-haspopup="dialog"
        // Not `disabled`: a browser takes focus off a button as it
        // becomes disabled, and a keyboard user would lose their place.
        aria-disabled={pending !== undefined}
        className={cn(
          CHIP,
          FACE,
          FOCUS,
          "group cursor-pointer transition-colors hover:bg-muted aria-disabled:cursor-not-allowed",
          LOOKS[state],
        )}
        onClick={(event) => pressChip(dialog, event.currentTarget, event.detail)}
      >
        {face}
      </button>
    )
  } else {
    body = (
      // Where the buttons don't fit beside the name (a narrow phone, with
      // Update and Uninstall both out), they go to a second line of the chip
      // rather than cut the name short.
      <div className={cn(CHIP, "group/chip h-auto min-h-6 flex-wrap", LOOKS[state])}>
        {details.length > 0 ? (
          <InfoTip content={<Lines lines={details} />} contentClassName="max-w-64" side="top" align="start">
            <button
              ref={chipRef}
              type="button"
              aria-label={label}
              aria-describedby={described}
              className={cn(FACE, FOCUS, "cursor-default", side.length > 0 && "pr-1")}
            >
              {face}
            </button>
          </InfoTip>
        ) : (
          <span className={cn(FACE, side.length > 0 && "pr-1")}>{face}</span>
        )}
        {side.some(shownAtOnce) && (
          <span className="flex items-center pr-0.5">{side.filter(shownAtOnce).map(sideButton)}</span>
        )}
        {side.some((action) => !shownAtOnce(action)) && <Reveal>{side.filter((action) => !shownAtOnce(action)).map(sideButton)}</Reveal>}
        {a.state === "installing" && <ProgressLine agent={a} name={name} live={worker.online} />}
      </div>
    )
  }

  return (
    <li
      ref={item}
      tabIndex={-1}
      className={cn("flex min-w-0 rounded-sm", FOCUS)}
      data-agent={a.agent}
      data-agent-state={state}
      aria-labelledby={`${id}-label`}
    >
      {body}
      {/* Names the item, and says when the state changes. */}
      <span id={`${id}-label`} aria-live="polite" className="sr-only">
        {label}
      </span>
      {described && (
        <span id={described} hidden>
          {details.join(" ")}
        </span>
      )}
      {/* Says when a wish starts or stops waiting for the worker. */}
      <span aria-live="polite" className="sr-only">
        {wishText(wish, worker.online)}
      </span>
      {failure &&
        alerts &&
        createPortal(
          <p role="alert" className="text-xs text-destructive">
            {failure}
          </p>,
          alerts,
        )}
      {pane && (
        <Dialog
          open={open}
          onOpenChange={setOpen}
        >
          <DialogContent
            showCloseButton={false}
            // The button that opened it may be gone once what was asked is
            // accepted: focus goes to the chip, or the item, instead.
            onCloseAutoFocus={(event) => returnFocus.restore(event, () => chip.current ?? item.current)}
            onOpenAutoFocus={(event) => {
              if (pane !== "failed") return
              event.preventDefault()
              initial.current?.focus()
            }}
          >
            {pane === "install" && (
              <>
                <DialogHeader>
                  <DialogTitle>{t("devices.agents.installTitle", { agent: name, worker: worker.name })}</DialogTitle>
                  <DialogDescription>{t("devices.agents.installBody", { agent: name, worker: worker.name })}</DialogDescription>
                </DialogHeader>
                {a.state === "unknown" && <Note>{t("devices.agents.unknownHint")}</Note>}
                {!worker.online && <Note>{t("devices.agents.installOfflineHint", { agent: name, worker: worker.name })}</Note>}
                {/* Install only while it still applies: the worker may have moved on meanwhile. */}
                <DialogFooter>
                  <DialogClose asChild>
                    <Button variant="outline">{actions.includes("install") ? t("common.cancel") : t("common.close")}</Button>
                  </DialogClose>
                  {actions.includes("install") && (
                    <Button onClick={() => confirm("install")}>{t("devices.agents.actions.install")}</Button>
                  )}
                </DialogFooter>
              </>
            )}
            {pane === "failed" && (
              <>
                <DialogHeader>
                  <DialogTitle>{t("devices.agents.failedTitle", { agent: name, worker: worker.name })}</DialogTitle>
                  <DialogDescription>
                    {shownError(a) ? t("devices.agents.failedBody") : t("devices.agents.failedNoError")}
                  </DialogDescription>
                </DialogHeader>
                {shownError(a) && (
                  <p className="max-h-40 overflow-auto border bg-muted/50 p-2 font-mono text-xs break-words whitespace-pre-wrap text-destructive">
                    {shownError(a)}
                  </p>
                )}
                {wish && (
                  <Note>
                    {worker.online && <Spinner className="size-3" aria-hidden />}
                    {wishText(wish, worker.online)}
                  </Note>
                )}
                <DialogFooter>
                  {actions.includes("dismiss") && (
                    <Button variant="ghost" onClick={() => confirm("dismiss")}>
                      {t("devices.agents.actions.dismiss")}
                    </Button>
                  )}
                  <DialogClose asChild>
                    <Button ref={actions.includes("retry") ? undefined : initial} variant="outline">
                      {t("common.close")}
                    </Button>
                  </DialogClose>
                  {actions.includes("retry") && (
                    <Button ref={initial} onClick={() => confirm("retry")}>
                      {t("devices.agents.actions.retry")}
                    </Button>
                  )}
                </DialogFooter>
              </>
            )}
            {pane === "uninstall" && (
              <>
                <DialogHeader>
                  <DialogTitle>{t("devices.agents.uninstallTitle", { agent: name, worker: worker.name })}</DialogTitle>
                  <DialogDescription>{t("devices.agents.uninstallBody", { agent: name })}</DialogDescription>
                </DialogHeader>
                <DialogFooter>
                  <DialogClose asChild>
                    <Button variant="outline">{t("common.cancel")}</Button>
                  </DialogClose>
                  <Button variant="destructive" onClick={() => confirm("uninstall")}>
                    {t("devices.agents.actions.uninstall")}
                  </Button>
                </DialogFooter>
              </>
            )}
          </DialogContent>
        </Dialog>
      )}
    </li>
  )
}

/**
 * The chip's actions, folded away until the chip is hovered or has focus
 * inside (a folded button still takes focus, which unfolds it); always out
 * where nothing hovers. Unfolding widens the chip from its end; on a line
 * of its own (a narrow chip wraps) it keeps to the right.
 */
function Reveal({ children }: { children: ReactNode }) {
  return (
    <span
      className={cn(
        "ml-auto grid grid-cols-[minmax(0,0fr)] opacity-0 transition-[grid-template-columns,opacity] duration-200 ease-out motion-reduce:transition-none",
        "group-hover/chip:grid-cols-[minmax(0,1fr)] group-hover/chip:opacity-100",
        "group-focus-within/chip:grid-cols-[minmax(0,1fr)] group-focus-within/chip:opacity-100",
        "[@media(hover:none)]:grid-cols-[minmax(0,1fr)] [@media(hover:none)]:opacity-100",
      )}
    >
      <span className="min-w-0 overflow-hidden">
        <span className="flex items-center gap-0.5 pr-0.5">{children}</span>
      </span>
    </span>
  )
}

/** A line of a dialog besides its description. */
function Note({ children }: { children: ReactNode }) {
  return <p className="flex items-center gap-1.5 text-sm text-muted-foreground">{children}</p>
}

/** A tooltip's lines. */
function Lines({ lines }: { lines: string[] }) {
  return (
    <span className="flex flex-col gap-1">
      {lines.map((line) => (
        <span key={line}>{line}</span>
      ))}
    </span>
  )
}

/** The wish the worker hasn't acted on, as one line; empty when there is none. */
function wishText(wish: "install" | "uninstall" | null, online: boolean): string {
  if (!wish) return ""
  const key = wish === "install" ? "Install" : "Uninstall"
  return online ? i18n.t(`devices.agents.pending${key}`) : i18n.t(`devices.agents.pending${key}Offline`)
}

/** The download's progress: in full, and short for the chip (the percent, or the bytes when the total is unknown). */
function progressTexts(a: WorkerAgent): { full: string; short: string } {
  const percent = installProgress(a)
  const done = formatBytes(a.downloadedBytes)
  if (percent === null) return { full: i18n.t("devices.agents.progressUnknownTotal", { done }), short: done }
  return {
    full: i18n.t("devices.agents.progress", { done, total: formatBytes(a.totalBytes), percent }),
    short: i18n.t("devices.agents.progressPercent", { percent }),
  }
}

/**
 * The download's progress as a line along the chip's bottom edge: filled
 * to the percent, or (total unknown) a third of it, pulsing; it moves only
 * while the worker is online (`live`).
 */
function ProgressLine({ agent: a, name, live }: { agent: WorkerAgent; name: string; live: boolean }) {
  const { t } = useTranslation()
  const percent = installProgress(a)
  return (
    <span
      role="progressbar"
      aria-label={t("devices.agents.progressLabel", { agent: name })}
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={percent ?? undefined}
      aria-valuetext={progressTexts(a).full}
      className="pointer-events-none absolute inset-x-0 bottom-0 h-0.5"
    >
      <span
        className={cn(
          "absolute inset-y-0 left-0 bg-primary transition-[width] motion-reduce:transition-none",
          "forced-colors:bg-[Highlight] forced-colors:[forced-color-adjust:none]",
          percent === null && "w-1/3",
          percent === null && live && "animate-pulse motion-reduce:animate-none",
          !live && "bg-muted-foreground/50",
        )}
        style={percent === null ? undefined : { width: `${percent}%` }}
      />
    </span>
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
