import { BookOpenIcon } from "lucide-react"
import { useTranslation } from "react-i18next"
import { Link, Navigate, useLocation, useParams, useSearchParams } from "react-router"

import { useLedger, useProjects } from "@/api/hooks"
import type { Ledger, Project } from "@/api/types"
import { DecisionMessage, DecisionView, type BackState } from "@/components/ledger/decision-view"
import { LedgerProjectContext } from "@/components/ledger/ledger-context"
import { LedgerTree } from "@/components/ledger/ledger-tree"
import { PageHeader } from "@/components/page-header"
import { Button } from "@/components/ui/button"
import { Empty, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { decisionPath, parseDecisionSegment, projectPath } from "@/lib/tree"
import { cn } from "@/lib/utils"

import { BarePage, LoadError } from "./page-states"

/**
 * A project's ledger: its directory tree on the left, the chosen decision
 * on the right (`/p/:projectId/ledger/D12`, `?at=<commit>` for an old
 * version, `?view=history` for its versions). On narrow screens they are
 * two steps: the tree, then the decision.
 */
export function LedgerPage() {
  const { projectId } = useParams()
  const projects = useProjects()

  if (projects.isError && !projects.data) return <LoadError onRetry={() => void projects.refetch()} />
  if (!projects.data) return <BarePage />
  const project = projects.data.find((p) => p.projectId === projectId)
  if (!project) return <Navigate to="/" replace />
  return (
    <LedgerProjectContext.Provider value={project.projectId}>
      <LedgerHome key={project.projectId} project={project} />
    </LedgerProjectContext.Provider>
  )
}

function LedgerHome({ project }: { project: Project }) {
  const { t } = useTranslation()
  const { decision: segment } = useParams()
  const [search] = useSearchParams()
  const location = useLocation()
  const ledger = useLedger(project.projectId)

  const number = segment === undefined ? null : parseDecisionSegment(segment)
  // "D012", "d12" and "D12 " are D12: move to the canonical path.
  if (number !== null && segment !== `D${number}`) {
    return <Navigate to={`${decisionPath(project.projectId, number)}${location.search}`} replace />
  }
  const selecting = segment !== undefined
  // Back from a decision on a narrow screen: it stays selected and focused.
  const back = (location.state as BackState | null)?.focusDecision
  const highlighted = number ?? (typeof back === "number" ? back : null)

  const header = (
    <PageHeader>
      <Link
        to={projectPath(project.projectId)}
        className="min-w-0 truncate rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
      >
        {project.name}
      </Link>
      <span className="text-muted-foreground" aria-hidden>
        /
      </span>
      <h1 className="shrink-0 font-medium">{t("ledger.title")}</h1>
    </PageHeader>
  )

  let content
  if (ledger.isError && !ledger.data) {
    content = (
      <div role="alert" className="flex flex-1 flex-col items-center justify-center gap-4 p-6">
        <p className="text-sm text-muted-foreground">{t("ledger.loadFailed")}</p>
        <Button variant="outline" onClick={() => void ledger.refetch()}>
          {t("common.tryAgain")}
        </Button>
      </div>
    )
  } else if (!ledger.data) {
    content = <LedgerSkeleton />
  } else if (ledger.data.entries.length === 0) {
    content = (
      <Empty>
        <EmptyHeader>
          <EmptyMedia variant="icon">
            <BookOpenIcon />
          </EmptyMedia>
          <EmptyTitle>{t("ledger.emptyTitle")}</EmptyTitle>
          <EmptyDescription>{t("ledger.emptyDescription")}</EmptyDescription>
        </EmptyHeader>
      </Empty>
    )
  } else {
    content = (
      <div className="flex min-h-0 flex-1">
        <LedgerTree
          projectId={project.projectId}
          ledger={ledger.data}
          selected={highlighted}
          focusSelected={!selecting && highlighted !== null}
          className={cn("w-full md:w-72 md:shrink-0 md:border-r lg:w-80", selecting && "max-md:hidden")}
        />
        {/* Only the body's own boxes (tables, code, deep lists) scroll sideways, never the pane. */}
        <div className={cn("min-h-0 min-w-0 flex-1 overflow-x-hidden overflow-y-auto", !selecting && "max-md:hidden")}>
          <Detail
            projectId={project.projectId}
            ledger={ledger.data}
            segment={segment}
            number={number}
            at={search.get("at") || null}
            history={search.get("view") === "history"}
          />
        </div>
      </div>
    )
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      {header}
      {content}
    </div>
  )
}

function Detail({
  projectId,
  ledger,
  segment,
  number,
  at,
  history,
}: {
  projectId: string
  ledger: Ledger
  segment: string | undefined
  number: number | null
  at: string | null
  history: boolean
}) {
  const { t } = useTranslation()
  if (segment === undefined) {
    return (
      <div className="flex h-full items-center justify-center p-6">
        <p className="text-sm text-muted-foreground">{t("ledger.selectPrompt")}</p>
      </div>
    )
  }
  const entry = number === null ? undefined : ledger.entries.find((e) => e.number === number)
  if (number === null || !entry) {
    // Only a number is shown back; anything else in the address is not echoed.
    const title = number === null ? t("ledger.noSuchDecision") : t("ledger.notFoundTitle", { ref: `D${number}` })
    return (
      <DecisionMessage projectId={projectId} title={title}>
        <p className="max-w-sm text-sm text-muted-foreground">{t("ledger.notFoundDescription")}</p>
      </DecisionMessage>
    )
  }
  return <DecisionView key={`${number}:${at ?? ""}`} projectId={projectId} number={number} entry={entry} at={at} history={history} />
}

function LedgerSkeleton() {
  const { t } = useTranslation()
  return (
    <div className="flex min-h-0 flex-1" role="status" aria-label={t("common.loading")}>
      <div className="flex w-full flex-col gap-2 p-3 md:w-72 md:border-r lg:w-80">
        <Skeleton className="h-8 w-full" />
        {Array.from({ length: 6 }, (_, i) => (
          <Skeleton key={i} className="h-6" style={{ width: `${85 - (i % 3) * 15}%` }} />
        ))}
      </div>
      <div className="hidden flex-1 md:block" />
    </div>
  )
}
