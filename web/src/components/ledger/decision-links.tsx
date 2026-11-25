import type { ReactNode } from "react"
import { useTranslation } from "react-i18next"

import { cn } from "@/lib/utils"
import { decisionPath } from "@/lib/tree"

import { REF_CLASS, RefPreviewLayer, refStyles } from "./decision-ref"

/**
 * Links to decisions outside a body (a list, a status line), looking and
 * previewing like references in one: wrap the area in `DecisionRefArea`.
 */
export function DecisionLink({ projectId, number }: { projectId: string; number: number }) {
  return (
    <a href={decisionPath(projectId, number)} className={REF_CLASS}>
      D{number}
    </a>
  )
}

/** "D92, D93" as links, separated as the language does. */
export function DecisionLinks({ projectId, numbers }: { projectId: string; numbers: readonly number[] }) {
  const { t } = useTranslation()
  return numbers.map((n, i) => (
    <span key={n}>
      {i > 0 && t("common.listSeparator")}
      <DecisionLink projectId={projectId} number={n} />
    </span>
  ))
}

/** An area whose decision links navigate within the app and show a preview. */
export function DecisionRefArea({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <RefPreviewLayer>
      <div className={cn(refStyles, className)}>{children}</div>
    </RefPreviewLayer>
  )
}
