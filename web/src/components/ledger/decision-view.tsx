import type { TFunction } from "i18next"
import { ArrowLeftIcon, FolderIcon, HistoryIcon } from "lucide-react"
import { useEffect, useRef, type ReactNode } from "react"
import { useTranslation } from "react-i18next"
import { Link } from "react-router"

import { isApiError } from "@/api/errors"
import { useDecision, useDecisionHistory } from "@/api/hooks"
import type { DecisionVersion, LedgerEntry } from "@/api/types"
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { dirOfPath, shortCommit } from "@/lib/ledger-tree"
import { formatDateTime, formatTimeAgo } from "@/lib/time"
import { decisionPath, ledgerPath } from "@/lib/tree"
import { cn } from "@/lib/utils"

import { LedgerMarkdown } from "./ledger-markdown"

const focusRing = "rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring/50"

/** Where "All decisions" came from, so the list can show it again. */
export interface BackState {
  focusDecision: number
}

/**
 * Back to the list, on narrow screens where the list and the decision are
 * two steps; the decision it came from stays selected and focused there.
 */
export function BackToList({ projectId, number }: { projectId: string; number?: number }) {
  const { t } = useTranslation()
  const state: BackState | undefined = number === undefined ? undefined : { focusDecision: number }
  return (
    <Link
      to={ledgerPath(projectId)}
      state={state}
      className={cn("flex w-fit items-center gap-1.5 text-sm text-muted-foreground hover:text-foreground md:hidden", focusRing)}
    >
      <ArrowLeftIcon className="size-4" aria-hidden />
      {t("ledger.backToList")}
    </Link>
  )
}

/** A message in place of a decision: not found, failed to load. */
export function DecisionMessage({
  projectId,
  title,
  children,
}: {
  projectId: string
  title: string
  children?: ReactNode
}) {
  return (
    <div className="flex flex-col gap-6 px-4 py-6 md:px-8">
      <BackToList projectId={projectId} />
      <div role="status" className="flex min-w-0 flex-col items-center gap-3 py-16 text-center">
        <p className="max-w-full font-medium [overflow-wrap:anywhere]">{title}</p>
        {children}
      </div>
    </div>
  )
}

/**
 * One decision: as it is now, or as of the commit `at`; its body, or
 * (`history`) the versions it went through.
 */
export function DecisionView({
  projectId,
  number,
  entry,
  at,
  history,
}: {
  projectId: string
  number: number
  /** Its line in the ledger, for when it was last changed. */
  entry: LedgerEntry
  at: string | null
  history: boolean
}) {
  const { t } = useTranslation()
  const ref = `D${number}`
  // Only a full commit id can name a version; anything else is no version.
  const validAt = at === null || /^[0-9a-f]{40}$/.test(at)
  const decision = useDecision({ projectId, number, commit: at ?? "" }, { enabled: validAt })
  const versionMissing =
    at !== null &&
    (!validAt || isApiError(decision.error, "not_found") || isApiError(decision.error, "invalid_argument"))
  const loadFailed = decision.isError && !decision.data && !versionMissing

  // On narrow screens the list just went away: focus moves to the decision.
  const heading = useRef<HTMLHeadingElement>(null)
  useEffect(() => {
    if (window.matchMedia?.("(max-width: 767px)").matches) heading.current?.focus({ preventScroll: true })
  }, [])

  const d = versionMissing ? undefined : decision.data
  // Until it loads, the ledger's line stands in for the title and directory.
  const title = d?.title ?? entry.title
  const dir = d?.dir ?? entry.dir
  // An old version's time is in the note below.
  const updated = entry.updatedAtUnixMs
  const tab = (active: boolean) =>
    cn(
      "-mb-px border-b-2 px-1 pb-2 text-sm outline-none focus-visible:ring-2 focus-visible:ring-ring/50",
      active ? "border-foreground font-medium text-foreground" : "border-transparent text-muted-foreground hover:text-foreground",
    )

  return (
    <article className="mx-auto flex w-full max-w-[820px] min-w-0 flex-col gap-4 px-4 py-6 md:px-8">
      <BackToList projectId={projectId} number={number} />
      <header className="flex min-w-0 flex-col gap-2">
        <h2 ref={heading} tabIndex={-1} className="flex min-w-0 items-baseline gap-2.5 text-xl font-medium outline-none">
          <span className="shrink-0 font-mono text-lg text-muted-foreground">{ref}</span>
          <span className="min-w-0 [overflow-wrap:anywhere]">{title}</span>
        </h2>
        <p className="flex min-w-0 flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
          <span className="flex min-w-0 items-center gap-1.5">
            <FolderIcon className="size-3.5 shrink-0" aria-hidden />
            <span className="[overflow-wrap:anywhere]">{dir || t("ledger.rootDir")}</span>
          </span>
          {!at && (
            <time dateTime={new Date(updated).toISOString()} title={formatDateTime(updated)}>
              {t("ledger.updated", { ago: formatTimeAgo(updated) })}
            </time>
          )}
        </p>
      </header>

      {at && d && (
        <div
          role="note"
          className="flex flex-col gap-2 rounded-lg border border-amber-500/40 bg-amber-500/10 px-3 py-2.5 text-sm sm:flex-row sm:items-center sm:justify-between"
        >
          <span className="flex min-w-0 items-start gap-2">
            <HistoryIcon className="mt-0.5 size-4 shrink-0 text-amber-600 dark:text-amber-400" aria-hidden />
            <span className="min-w-0">
              {t("ledger.oldVersion", { ref, date: formatDateTime(d.committedAtUnixMs) })}{" "}
              <span className="font-mono text-xs text-muted-foreground">{shortCommit(d.commit)}</span>
            </span>
          </span>
          <Link
            to={decisionPath(projectId, number)}
            className={cn("shrink-0 self-start text-sm font-medium text-foreground underline underline-offset-4 hover:text-primary sm:self-auto", focusRing)}
          >
            {t("ledger.viewCurrent")}
          </Link>
        </div>
      )}

      <nav className="flex gap-4 border-b" aria-label={t("ledger.views", { ref })}>
        <Link to={decisionPath(projectId, number, { at: at ?? undefined })} aria-current={history ? undefined : "page"} className={tab(!history)}>
          {t("ledger.tabDecision")}
        </Link>
        <Link
          to={decisionPath(projectId, number, { at: at ?? undefined, history: true })}
          aria-current={history ? "page" : undefined}
          className={tab(history)}
        >
          {t("ledger.tabHistory")}
        </Link>
      </nav>

      {history ? (
        <DecisionHistory projectId={projectId} number={number} viewing={versionMissing ? null : at} />
      ) : versionMissing ? (
        <InlineMessage title={t("ledger.versionNotFound", { ref })}>
          <Link to={decisionPath(projectId, number)} className={cn("text-sm text-foreground underline underline-offset-4 hover:text-primary", focusRing)}>
            {t("ledger.viewCurrent")}
          </Link>
        </InlineMessage>
      ) : loadFailed ? (
        <InlineMessage title={t("ledger.decisionLoadFailed", { ref })}>
          <Button variant="outline" size="sm" onClick={() => void decision.refetch()}>
            {t("common.tryAgain")}
          </Button>
        </InlineMessage>
      ) : d ? (
        <LedgerMarkdown>{d.body}</LedgerMarkdown>
      ) : (
        <div className="flex flex-col gap-2.5" role="status" aria-label={t("common.loading")}>
          <Skeleton className="h-4 w-11/12" />
          <Skeleton className="h-4 w-4/5" />
          <Skeleton className="h-4 w-2/3" />
        </div>
      )}
    </article>
  )
}

/** A message in place of the body, under the decision's header and tabs. */
function InlineMessage({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div role="status" className="flex min-w-0 flex-col items-center gap-3 py-12 text-center">
      <p className="max-w-full font-medium [overflow-wrap:anywhere]">{title}</p>
      {children}
    </div>
  )
}

/** What a version changed, as short phrases. */
function changes(v: DecisionVersion, previous: DecisionVersion | undefined, t: TFunction): string[] {
  if (v.added) return [t("ledger.change.added")]
  const out: string[] = []
  // A retitle changes the path too: only a new directory is a move.
  const movedDir = v.moved && (!previous || dirOfPath(previous.path) !== dirOfPath(v.path))
  if (movedDir) {
    const to = dirOfPath(v.path) || t("ledger.rootDir")
    out.push(
      previous
        ? t("ledger.change.moved", { from: dirOfPath(previous.path) || t("ledger.rootDir"), to })
        : t("ledger.change.movedTo", { to }),
    )
  }
  if (v.retitled) {
    out.push(previous ? t("ledger.change.retitled", { from: previous.title }) : t("ledger.change.retitledUnknown"))
  }
  if (v.contentChanged) out.push(t("ledger.change.content"))
  return out
}

/** The versions of a decision, newest first, each opening that version. */
function DecisionHistory({ projectId, number, viewing }: { projectId: string; number: number; viewing: string | null }) {
  const { t } = useTranslation()
  const history = useDecisionHistory(projectId, number)
  const ref = `D${number}`

  if (history.isError && !history.data) {
    return (
      <div role="status" className="flex flex-col items-start gap-3 py-4">
        <p className="text-sm text-muted-foreground">{t("ledger.historyLoadFailed")}</p>
        <Button variant="outline" size="sm" onClick={() => void history.refetch()}>
          {t("common.tryAgain")}
        </Button>
      </div>
    )
  }
  if (!history.data) {
    return (
      <div className="flex flex-col gap-3" role="status" aria-label={t("common.loading")}>
        <Skeleton className="h-12 w-full" />
        <Skeleton className="h-12 w-full" />
      </div>
    )
  }
  const versions = history.data.pages.flatMap((p) => p.versions)
  return (
    <section className="flex flex-col gap-3" aria-label={t("ledger.historyTitle", { ref })}>
      <h3 className="sr-only">{t("ledger.historyTitle", { ref })}</h3>
      <ol className="flex flex-col">
        {versions.map((v, i) => {
          const current = i === 0
          const open = viewing === v.commit || (viewing === null && current)
          return (
            <li key={v.commit}>
              <Link
                to={decisionPath(projectId, number, current ? {} : { at: v.commit })}
                aria-current={open ? "true" : undefined}
                className={cn(
                  "flex min-w-0 flex-col gap-1 rounded-md px-3 py-2.5 outline-none hover:bg-foreground/5 focus-visible:ring-2 focus-visible:ring-ring/50",
                  open && "bg-foreground/5",
                )}
              >
                <span className="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-0.5 text-xs text-muted-foreground">
                  <time dateTime={new Date(v.committedAtUnixMs).toISOString()} className="tabular-nums" title={formatDateTime(v.committedAtUnixMs)}>
                    {formatDateTime(v.committedAtUnixMs)}
                  </time>
                  <span className="font-mono [overflow-wrap:anywhere]">{v.author}</span>
                  {current && <span className="rounded-sm bg-foreground/10 px-1.5 text-foreground">{t("ledger.current")}</span>}
                  {open && !current && <span className="rounded-sm bg-amber-500/15 px-1.5 text-foreground">{t("ledger.viewing")}</span>}
                </span>
                <span className="text-sm [overflow-wrap:anywhere]">{v.summary}</span>
                <span className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
                  {changes(v, versions[i + 1], t).map((c) => (
                    <span key={c} className="[overflow-wrap:anywhere]">
                      {c}
                    </span>
                  ))}
                  <span className="font-mono">{shortCommit(v.commit)}</span>
                  {v.codeCommit && (
                    <span className="font-mono" title={v.codeCommit}>
                      {t("ledger.codeCommit", { commit: shortCommit(v.codeCommit) })}
                    </span>
                  )}
                </span>
              </Link>
            </li>
          )
        })}
      </ol>
      {history.hasNextPage && (
        <Button
          variant="outline"
          size="sm"
          className="self-start"
          disabled={history.isFetchingNextPage}
          onClick={() => void history.fetchNextPage()}
        >
          {history.isFetchingNextPage && <Spinner aria-hidden />}
          {t("ledger.loadMore")}
        </Button>
      )}
      {history.isFetchNextPageError && (
        <p role="alert" className="text-sm text-destructive">
          {t("ledger.historyLoadFailed")}
        </p>
      )}
    </section>
  )
}
