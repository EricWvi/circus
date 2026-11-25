import { FileWarningIcon } from "lucide-react"
import { useEffect, useState } from "react"
import { useTranslation } from "react-i18next"

import { isApiError } from "@/api/errors"
import { useSessionLedgerDiff } from "@/api/hooks"
import type { SessionLedgerDiff } from "@/api/types"
import { Skeleton } from "@/components/ui/skeleton"
import { diffOffMainThread } from "@/lib/diff-runner"
import { compareVersions, MAX_DIFF_ROWS, type DiffLine, type DiffOutcome } from "@/lib/line-diff"
import { cn } from "@/lib/utils"

/**
 * One changed file of the session's open changeset as a line diff against
 * its base. Decision files are written by agents: everything here is plain
 * text, never Markdown or HTML.
 */
export function LedgerDiff({
  sessionId,
  path,
  version,
  label,
}: {
  sessionId: string
  path: string
  /** The snapshot's version the row comes from (its `updatedAtUnixMs`). */
  version: number
  label: string
}) {
  const { t } = useTranslation()
  const diff = useSessionLedgerDiff(sessionId, path, version)
  // The file is no longer changed: its row goes with the next read of the changes.
  if (isApiError(diff.error, "not_found")) return null
  if (diff.isError) return <p className="px-3 py-2 text-xs text-destructive">{t("sessionLedger.diffLoadFailed")}</p>
  if (!diff.data) return <Loading />
  return <DiffView diff={diff.data} label={label} />
}

function Loading() {
  const { t } = useTranslation()
  return (
    <div className="flex flex-col gap-1.5 px-3 py-2" role="status" aria-label={t("common.loading")}>
      <Skeleton className="h-3 w-3/4" />
      <Skeleton className="h-3 w-1/2" />
      <Skeleton className="h-3 w-2/3" />
    </div>
  )
}

/**
 * Comparisons by the diff they were made from: the query keeps its answer
 * while the row is collapsed, so opening it again shows the same diff at
 * once instead of comparing again.
 */
const outcomes = new WeakMap<SessionLedgerDiff, DiffOutcome>()
const comparing = new WeakMap<SessionLedgerDiff, Promise<DiffOutcome>>()

function compare(diff: SessionLedgerDiff): Promise<DiffOutcome> {
  let running = comparing.get(diff)
  if (!running) {
    const base = diff.baseExists ? diff.base : ""
    const next = diff.changesetExists ? diff.changeset : ""
    running = compareVersions(base, next, diffOffMainThread).then((outcome) => {
      outcomes.set(diff, outcome)
      return outcome
    })
    comparing.set(diff, running)
  }
  return running
}

/** The comparison of `diff`, once made. */
function useOutcome(diff: SessionLedgerDiff): DiffOutcome | undefined {
  const [, setDone] = useState<DiffOutcome>()
  useEffect(() => {
    if (outcomes.has(diff)) return
    let live = true
    void compare(diff).then((outcome) => {
      if (live) setDone(outcome)
    })
    return () => {
      live = false
    }
  }, [diff])
  return outcomes.get(diff)
}

export function DiffView({ diff, label }: { diff: SessionLedgerDiff; label: string }) {
  const { t } = useTranslation()
  const outcome = useOutcome(diff)
  if (!outcome) return <Loading />
  const { lines } = outcome
  const unchanged = lines.every((l) => l.kind === "context" || l.kind === "skipped")
  const whole = outcome.kind === "whole" ? outcome.reason : null
  return (
    <div data-slot="ledger-diff" data-outcome={whole ?? "diff"} className="flex min-w-0 flex-col gap-1">
      {(diff.baseTruncated || diff.changesetTruncated || whole !== null) && (
        <ul className="flex flex-col gap-0.5 px-3 pt-2 text-xs text-muted-foreground">
          {whole === "too_large" && <Note text={t("sessionLedger.diffTooLarge", { n: MAX_DIFF_ROWS / 2 })} />}
          {whole === "too_different" && <Note text={t("sessionLedger.diffTooDifferent", { n: MAX_DIFF_ROWS / 2 })} />}
          {diff.baseTruncated && <Note text={t("sessionLedger.baseTruncated")} />}
          {diff.changesetTruncated && <Note text={t("sessionLedger.changesetTruncated")} />}
        </ul>
      )}
      {unchanged ? (
        <p className="px-3 py-2 text-xs text-muted-foreground">{t("sessionLedger.diffNoChanges")}</p>
      ) : (
        <div className="max-h-96 overflow-auto font-mono text-xs leading-5" role="table" aria-label={label}>
          <div role="rowgroup" className="w-max min-w-full">
            {lines.map((line, i) => (
              <DiffRow key={i} line={line} />
            ))}
          </div>
        </div>
      )}
    </div>
  )
}

function Note({ text }: { text: string }) {
  return (
    <li data-diff-note className="flex items-start gap-1.5">
      <FileWarningIcon className="mt-0.5 size-3 shrink-0" aria-hidden />
      {text}
    </li>
  )
}

const signs = { context: " ", removed: "−", added: "+" } as const
const srLabels = { removed: "sessionLedger.diffRemoved", added: "sessionLedger.diffAdded" } as const

function DiffRow({ line }: { line: DiffLine }) {
  const { t } = useTranslation()
  if (line.kind === "skipped" || line.kind === "more") {
    return (
      <div role="row" data-line={line.kind} className="bg-muted/50 px-3 text-muted-foreground">
        <span role="cell">
          {line.kind === "skipped"
            ? t("sessionLedger.diffSkipped", { count: line.count })
            : t("sessionLedger.diffMore", { count: line.count })}
        </span>
      </div>
    )
  }
  const oldNo = line.kind === "added" ? "" : String(line.oldNo)
  const newNo = line.kind === "removed" ? "" : String(line.newNo)
  return (
    <div
      role="row"
      data-line={line.kind}
      className={cn(
        "flex whitespace-pre",
        line.kind === "added" && "bg-emerald-500/10 text-emerald-900 dark:text-emerald-200",
        line.kind === "removed" && "bg-red-500/10 text-red-900 dark:text-red-200",
      )}
    >
      <span role="cell" className="w-9 shrink-0 pr-1 text-right text-muted-foreground select-none">
        {oldNo}
      </span>
      <span role="cell" className="w-9 shrink-0 pr-1 text-right text-muted-foreground select-none">
        {newNo}
      </span>
      <span role="cell" className="w-4 shrink-0 text-center select-none">
        <span aria-hidden>{signs[line.kind]}</span>
        {line.kind !== "context" && <span className="sr-only">{t(srLabels[line.kind])}</span>}
      </span>
      <span role="cell" className="pr-3">
        {line.text}
      </span>
    </div>
  )
}
