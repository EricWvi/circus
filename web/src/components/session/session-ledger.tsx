import {
  BookMarkedIcon,
  ChevronRightIcon,
  CircleAlertIcon,
  CloudOffIcon,
  GitMergeIcon,
  HourglassIcon,
  InfoIcon,
  TriangleAlertIcon,
} from "lucide-react"
import type { TFunction } from "i18next"
import { useEffect, useId, useRef, useState, type Ref } from "react"
import { useTranslation } from "react-i18next"

import { useMergeSessionLedger, useSessionLedger } from "@/api/hooks"
import type { LedgerProblem, SessionLedger, SessionLedgerEntry, SessionLedgerStatus } from "@/api/types"
import { DisabledReason } from "@/components/disabled-reason"
import { InfoTip } from "@/components/info-tip"
import { DecisionLink, DecisionLinks, DecisionRefArea } from "@/components/ledger/decision-links"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Sheet, SheetContent, SheetDescription, SheetFooter, SheetHeader, SheetTitle } from "@/components/ui/sheet"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import {
  assignedNumbers,
  ledgerEntryView,
  mergeErrorMessage,
  mergeState,
  snapshotFailure,
} from "@/lib/session-ledger"
import { formatClock, formatDateTime } from "@/lib/time"
import { cn } from "@/lib/utils"

import { LedgerDiff } from "./ledger-diff"

/**
 * The session header's way into its Ledger changes: the number of changes,
 * red when they have problems; muted while the snapshot is prepared or
 * when it failed (with why); nothing without a snapshot.
 */
export function LedgerButton({
  status,
  onOpen,
  ref,
}: {
  status: SessionLedgerStatus | undefined
  onOpen: () => void
  /** The button, for returning focus to it when the drawer closes. */
  ref?: Ref<HTMLButtonElement>
}) {
  const { t } = useTranslation()
  const view = ledgerEntryView(status)
  if (view.kind === "none") return null
  if (view.kind !== "ready") {
    const tip =
      view.kind === "failed"
        ? t("sessionLedger.entryFailed", { reason: view.reason })
        : t("sessionLedger.entryPreparing")
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <Button
            ref={ref}
            variant="ghost"
            size="sm"
            data-ledger-entry={view.kind}
            className="text-muted-foreground"
            aria-label={t("sessionLedger.entryMutedLabel", { reason: tip })}
            onClick={onOpen}
          >
            {view.kind === "failed" ? <CloudOffIcon aria-hidden /> : <BookMarkedIcon aria-hidden />}
            {t("sessionLedger.entry")}
          </Button>
        </TooltipTrigger>
        <TooltipContent className="max-w-xs">{tip}</TooltipContent>
      </Tooltip>
    )
  }
  const label = view.problems
    ? t("sessionLedger.entryLabelProblems", { count: view.changes })
    : t("sessionLedger.entryLabel", { count: view.changes })
  return (
    <Button
      ref={ref}
      variant={view.problems ? "destructive" : "ghost"}
      size="sm"
      data-ledger-entry={view.problems ? "problems" : "ready"}
      aria-label={label}
      title={label}
      onClick={onOpen}
    >
      {view.problems ? <CircleAlertIcon aria-hidden /> : <BookMarkedIcon aria-hidden />}
      {t("sessionLedger.entry")}
      {view.changes > 0 && (
        <span
          className={cn(
            "min-w-4 rounded-full px-1 text-center text-xs leading-4 tabular-nums",
            view.problems ? "bg-destructive text-white" : "bg-muted text-foreground",
          )}
          aria-hidden
        >
          {view.changes}
        </span>
      )}
    </Button>
  )
}

/** The drawer with the session's Ledger changes, their diffs and the merge. */
export function SessionLedgerSheet({
  open,
  onOpenChange,
  sessionId,
  projectId,
  turnRunning,
  deviceOffline,
  onCloseAutoFocus,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  sessionId: string
  projectId: string
  turnRunning: boolean
  deviceOffline: boolean
  /** Where focus goes when the drawer closes (it isn't opened by a SheetTrigger). */
  onCloseAutoFocus?: (event: Event) => void
}) {
  const { t } = useTranslation()
  return (
    <Sheet open={open} onOpenChange={onOpenChange}>
      <SheetContent
        side="right"
        onCloseAutoFocus={onCloseAutoFocus}
        className="gap-0 data-[side=right]:w-full data-[side=right]:sm:max-w-xl"
        data-session-ledger
      >
        <SheetHeader className="border-b pr-12">
          <SheetTitle>{t("sessionLedger.title")}</SheetTitle>
          <SheetDescription>{t("sessionLedger.description")}</SheetDescription>
        </SheetHeader>
        {open && (
          <LedgerBody
            sessionId={sessionId}
            projectId={projectId}
            turnRunning={turnRunning}
            deviceOffline={deviceOffline}
          />
        )}
      </SheetContent>
    </Sheet>
  )
}

function LedgerBody({
  sessionId,
  projectId,
  turnRunning,
  deviceOffline,
}: {
  sessionId: string
  projectId: string
  turnRunning: boolean
  deviceOffline: boolean
}) {
  const { t } = useTranslation()
  const ledger = useSessionLedger(sessionId)
  if (!ledger.data) {
    if (ledger.isError) return <p className="p-4 text-sm text-destructive">{t("sessionLedger.loadFailed")}</p>
    return (
      <div className="flex flex-col gap-2 p-4" role="status" aria-label={t("common.loading")}>
        <Skeleton className="h-4 w-1/2" />
        <Skeleton className="h-10 w-full" />
        <Skeleton className="h-10 w-full" />
      </div>
    )
  }
  const data = ledger.data
  return (
    <>
      <DecisionRefArea className="flex min-h-0 flex-1 flex-col gap-4 overflow-y-auto p-4">
        <StatusLine status={data.status} deviceOffline={deviceOffline} />
        <Changes ledger={data} sessionId={sessionId} projectId={projectId} />
        {data.merged.length > 0 && (
          <section className="flex flex-col gap-1 text-sm" data-slot="ledger-merged">
            {data.merged.map((m) => {
              const numbers = assignedNumbers(m.numbers)
              const time = formatClock(m.changeset.updatedAtUnixMs)
              return (
                <p key={m.changeset.changesetId} className="flex items-start gap-1.5 text-muted-foreground">
                  <GitMergeIcon className="mt-0.5 size-3.5 shrink-0" aria-hidden />
                  <span className="min-w-0" title={formatDateTime(m.changeset.updatedAtUnixMs)}>
                    {numbers.length > 0 ? (
                      <>
                        {t("sessionLedger.mergedAt", { time })}
                        <DecisionLinks projectId={projectId} numbers={numbers} />
                      </>
                    ) : (
                      t("sessionLedger.mergedNoNumbers", { time })
                    )}
                  </span>
                </p>
              )
            })}
          </section>
        )}
      </DecisionRefArea>
      <MergeFooter
        ledger={data}
        fetching={ledger.isFetching}
        sessionId={sessionId}
        projectId={projectId}
        turnRunning={turnRunning}
        deviceOffline={deviceOffline}
      />
    </>
  )
}

function StatusLine({ status, deviceOffline }: { status: SessionLedgerStatus | undefined; deviceOffline: boolean }) {
  const { t } = useTranslation()
  if (!status) return null
  let line
  switch (status.state) {
    case "ready":
      line = (
        <>
          <span className="font-medium text-foreground">{t("sessionLedger.state.ready")}</span>
          <span aria-hidden>·</span>
          {status.collectedAtUnixMs > 0 ? (
            <time title={formatDateTime(status.collectedAtUnixMs)}>
              {t("sessionLedger.collectedAt", { time: formatClock(status.collectedAtUnixMs) })}
            </time>
          ) : (
            <span>{t("sessionLedger.notCollected")}</span>
          )}
          {status.applying !== "none" && (
            <>
              <span aria-hidden>·</span>
              <span data-slot="ledger-applying">
                {status.applying === "merge" ? t("sessionLedger.applying.merge") : t("sessionLedger.applying.discard")}
              </span>
            </>
          )}
        </>
      )
      break
    case "failed":
      line = (
        <span className="text-destructive">{t("sessionLedger.state.failed", { reason: snapshotFailure(status) })}</span>
      )
      break
    default:
      line = (
        <>
          <Spinner className="size-3" aria-hidden />
          <span>{t("sessionLedger.state.preparing")}</span>
        </>
      )
  }
  return (
    <div className="flex flex-col gap-1.5" data-slot="ledger-status">
      <p role="status" className="flex flex-wrap items-center gap-x-1.5 gap-y-0.5 text-sm text-muted-foreground">
        {line}
      </p>
      {status.errorDetail !== "" && (
        <p className="font-mono text-xs break-all text-muted-foreground" data-slot="ledger-error-detail">
          {status.errorDetail}
        </p>
      )}
      {status.collectionDue && (
        <p className="flex items-start gap-1.5 text-xs text-muted-foreground" data-slot="ledger-collection-due">
          <HourglassIcon className="mt-0.5 size-3 shrink-0" aria-hidden />
          {deviceOffline ? t("sessionLedger.collectionDueOffline") : t("sessionLedger.collectionDue")}
        </p>
      )}
    </div>
  )
}

/** Problems and warnings without a path, or with one no entry has: shown above the list. */
function unplaced(items: readonly LedgerProblem[], entries: readonly SessionLedgerEntry[]): LedgerProblem[] {
  const paths = new Set(entries.flatMap((e) => [e.path, e.basePath]).filter((p) => p !== ""))
  return items.filter((p) => !paths.has(p.path))
}

function Changes({ ledger, sessionId, projectId }: { ledger: SessionLedger; sessionId: string; projectId: string }) {
  const { t } = useTranslation()
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set())
  if (ledger.status?.state !== "ready") return null
  const problems = unplaced(ledger.problems, ledger.entries)
  const warnings = unplaced(ledger.warnings, ledger.entries)
  const toggle = (path: string) =>
    setExpanded((s) => {
      const next = new Set(s)
      if (next.has(path)) next.delete(path)
      else next.add(path)
      return next
    })
  return (
    <section className="flex flex-col gap-3" aria-label={t("sessionLedger.changes")}>
      {problems.length > 0 && (
        <ul className="flex flex-col gap-1" aria-label={t("sessionLedger.problems")} data-slot="ledger-problems">
          {problems.map((p, i) => (
            <li key={i} className="flex items-start gap-1.5 text-sm text-destructive">
              <CircleAlertIcon className="mt-0.5 size-3.5 shrink-0" aria-hidden />
              <span className="min-w-0 [overflow-wrap:anywhere]">
                {p.path !== "" && (
                  <span className="font-mono text-xs">{t("sessionLedger.pathPrefix", { path: p.path })}</span>
                )}
                {p.message}
              </span>
            </li>
          ))}
        </ul>
      )}
      {warnings.length > 0 && (
        <ul className="flex flex-col gap-1" aria-label={t("sessionLedger.warnings")} data-slot="ledger-warnings">
          {warnings.map((p, i) => (
            <li key={i} className="flex items-start gap-1.5 text-sm text-muted-foreground">
              <TriangleAlertIcon className="mt-0.5 size-3.5 shrink-0" aria-hidden />
              <span className="min-w-0 [overflow-wrap:anywhere]">
                {p.path !== "" && (
                  <span className="font-mono text-xs">{t("sessionLedger.pathPrefix", { path: p.path })}</span>
                )}
                {p.message}
              </span>
            </li>
          ))}
        </ul>
      )}
      {ledger.entries.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t("sessionLedger.noChanges")}</p>
      ) : (
        <ul className="flex flex-col divide-y border" data-slot="ledger-entries">
          {ledger.entries.map((entry) => (
            <EntryRow
              key={entry.path}
              entry={entry}
              warnings={ledger.warnings.filter((w) => w.path !== "" && (w.path === entry.path || w.path === entry.basePath))}
              sessionId={sessionId}
              projectId={projectId}
              expanded={expanded.has(entry.path)}
              onToggle={() => toggle(entry.path)}
            />
          ))}
        </ul>
      )}
    </section>
  )
}

/** "core/x/", or the root's name, for a file's directory. */
function dirLabel(path: string, root: string): string {
  const slash = path.lastIndexOf("/")
  return slash < 0 ? root : `${path.slice(0, slash)}/`
}

/** What changed, as short phrases ("revised", "moved a/ → b/", …). */
function changePhrases(entry: SessionLedgerEntry, t: TFunction): string[] {
  const root = t("sessionLedger.rootDir")
  if (entry.number === "") {
    if (entry.deleted) return [t("sessionLedger.change.deleted")]
    const out: string[] = [entry.added ? t("sessionLedger.change.fileAdded") : t("sessionLedger.change.fileChanged")]
    if (entry.basePath !== "") out.push(t("sessionLedger.change.fileMoved", { from: entry.basePath }))
    return out
  }
  if (entry.added) return [t("sessionLedger.change.added", { dir: dirLabel(entry.path, root) })]
  const out: string[] = []
  if (entry.contentChanged) out.push(t("sessionLedger.change.revised"))
  if (entry.moved) {
    out.push(
      t("sessionLedger.change.moved", {
        from: dirLabel(entry.basePath || entry.path, root),
        to: dirLabel(entry.path, root),
      }),
    )
  }
  if (entry.retitled) out.push(t("sessionLedger.change.retitled", { from: entry.baseTitle, to: entry.title }))
  if (entry.deleted) out.push(t("sessionLedger.change.deleted"))
  return out
}

function EntryRow({
  entry,
  warnings,
  sessionId,
  projectId,
  expanded,
  onToggle,
}: {
  entry: SessionLedgerEntry
  warnings: LedgerProblem[]
  sessionId: string
  projectId: string
  expanded: boolean
  onToggle: () => void
}) {
  const { t } = useTranslation()
  const problem = entry.problems.length > 0
  const numeric = /^D([0-9]+)$/.exec(entry.number)
  // A path may hold anything; ids may not.
  const diffId = `ledger-diff-${useId()}`
  return (
    <li
      data-slot="ledger-entry"
      data-problem={problem || undefined}
      className={cn("flex min-w-0 flex-col", problem && "bg-destructive/5")}
    >
      <div
        className="flex min-w-0 cursor-pointer items-start gap-2 px-2 py-2 hover:bg-muted/50"
        onClick={(event) => {
          // A decision's link navigates; anywhere else opens the diff.
          if ((event.target as Element).closest("a")) return
          onToggle()
        }}
      >
        <button
          type="button"
          aria-expanded={expanded}
          aria-controls={expanded ? diffId : undefined}
          aria-label={t(expanded ? "sessionLedger.hideDiff" : "sessionLedger.showDiff", { path: entry.path })}
          className="mt-0.5 rounded-sm text-muted-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
          onClick={(event) => {
            event.stopPropagation()
            onToggle()
          }}
        >
          <ChevronRightIcon className={cn("size-4 transition-transform", expanded && "rotate-90")} aria-hidden />
        </button>
        <div className="flex min-w-0 flex-1 flex-col gap-0.5">
          <p className="flex min-w-0 flex-wrap items-baseline gap-x-2 text-sm">
            {entry.number === "" ? (
              <>
                <Badge variant="outline" className="shrink-0">
                  {t("sessionLedger.fileBadge")}
                </Badge>
                <span className="min-w-0 font-mono text-xs [overflow-wrap:anywhere]">{entry.path}</span>
              </>
            ) : (
              <>
                {entry.added && (
                  <Badge variant="secondary" className="shrink-0">
                    {t("sessionLedger.newBadge")}
                  </Badge>
                )}
                <span className={cn("shrink-0 font-mono text-xs", problem && "text-destructive")}>
                  {numeric ? <DecisionLink projectId={projectId} number={Number(numeric[1])} /> : entry.number}
                </span>
                <span className={cn("min-w-0 font-medium [overflow-wrap:anywhere]", problem && "text-destructive")}>
                  {entry.title}
                </span>
              </>
            )}
          </p>
          <p className="text-xs text-muted-foreground [overflow-wrap:anywhere]">
            {changePhrases(entry, t).join(t("common.listSeparator"))}
          </p>
          {entry.problems.map((p, i) => (
            <p key={`p${i}`} className="flex items-start gap-1 text-xs text-destructive">
              <CircleAlertIcon className="mt-px size-3 shrink-0" aria-hidden />
              <span className="min-w-0 [overflow-wrap:anywhere]">{p}</span>
            </p>
          ))}
          {warnings.map((w, i) => (
            <p key={`w${i}`} className="flex items-start gap-1 text-xs text-muted-foreground">
              <TriangleAlertIcon className="mt-px size-3 shrink-0" aria-hidden />
              <span className="min-w-0 [overflow-wrap:anywhere]">{w.message}</span>
            </p>
          ))}
        </div>
      </div>
      {expanded && (
        <div id={diffId} className="border-t bg-background">
          <LedgerDiff
            sessionId={sessionId}
            path={entry.path}
            label={t("sessionLedger.diffTable", { path: entry.path })}
          />
        </div>
      )}
    </li>
  )
}

function MergeFooter({
  ledger,
  fetching,
  sessionId,
  projectId,
  turnRunning,
  deviceOffline,
}: {
  ledger: SessionLedger
  /** The changes are being read again. */
  fetching: boolean
  sessionId: string
  projectId: string
  turnRunning: boolean
  deviceOffline: boolean
}) {
  const { t } = useTranslation()
  const merge = useMergeSessionLedger()
  const state = mergeState(ledger, { turnRunning, deviceOffline })
  useForgetOnChange(merge, ledger.status?.updatedAtUnixMs ?? 0, fetching)
  const result = merge.data
  if (state.kind === "none" && !result && !merge.isError) return null
  return (
    <SheetFooter className="border-t" data-slot="ledger-merge">
      {result && (
        <DecisionRefArea>
          <p role="status" className="flex items-start gap-1.5 text-sm">
            <GitMergeIcon className="mt-0.5 size-3.5 shrink-0" aria-hidden />
            <span className="min-w-0">
              {assignedNumbers(result.numbers).length > 0 ? (
                <>
                  {t("sessionLedger.merge.done")}
                  <DecisionLinks projectId={projectId} numbers={assignedNumbers(result.numbers)} />
                </>
              ) : (
                t("sessionLedger.merge.doneNoNumbers")
              )}
            </span>
          </p>
        </DecisionRefArea>
      )}
      {merge.isError && (
        <p role="alert" className="text-sm text-destructive">
          {mergeErrorMessage(merge.error)}
        </p>
      )}
      {state.kind === "waiting" && (
        <div className="flex items-center gap-2" data-merge-state="waiting">
          <span className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-dashed px-3 text-sm text-muted-foreground">
            <HourglassIcon className="size-3.5" aria-hidden />
            {t("sessionLedger.merge.waiting")}
          </span>
          <InfoTip content={t("sessionLedger.merge.waitingHint")} contentClassName="max-w-xs">
            <button
              type="button"
              aria-label={t("sessionLedger.merge.waitingInfo")}
              className="rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
            >
              <InfoIcon className="size-4" aria-hidden />
            </button>
          </InfoTip>
        </div>
      )}
      {(state.kind === "enabled" || state.kind === "disabled") && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-1" data-merge-state={state.kind}>
          <DisabledReason reason={state.kind === "disabled" ? state.reason : null}>
            <Button
              disabled={state.kind === "disabled" || merge.isPending}
              onClick={() => {
                if (state.kind !== "enabled") return
                merge.mutate({ sessionId, projectId, expectedChangesetId: state.changesetId })
              }}
            >
              {merge.isPending ? <Spinner aria-hidden /> : <GitMergeIcon aria-hidden />}
              {merge.isPending ? t("sessionLedger.merge.merging") : t("sessionLedger.merge.button")}
            </Button>
          </DisabledReason>
          {state.kind === "disabled" && (
            <span className="text-xs text-muted-foreground" data-slot="merge-blocked">
              {state.reason}
            </span>
          )}
        </div>
      )}
    </SheetFooter>
  )
}

/**
 * Forgets a merge's answer (its numbers or its error) once the snapshot it
 * describes has changed: a new collection or changeset after it. The read
 * the merge itself causes is what the answer describes, so the snapshot's
 * version is taken once that read is in.
 */
function useForgetOnChange(
  merge: { isSuccess: boolean; isError: boolean; reset: () => void },
  version: number,
  fetching: boolean,
) {
  const { isSuccess, isError, reset } = merge
  const settled = isSuccess || isError
  const seen = useRef<number | null>(null)
  useEffect(() => {
    if (!settled) {
      seen.current = null
      return
    }
    if (fetching) return
    if (seen.current === null) seen.current = version
    else if (seen.current !== version) {
      seen.current = null
      reset()
    }
  }, [settled, fetching, version, reset])
}
