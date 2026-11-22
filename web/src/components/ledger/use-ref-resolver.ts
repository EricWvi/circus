import { useContext, useMemo } from "react"
import { useTranslation } from "react-i18next"

import { useLedgerNumbers, useProjects } from "@/api/hooks"
import { decisionPath } from "@/lib/tree"

import { LedgerProjectContext, type RefResolver } from "./ledger-context"

const PENDING = { kind: "pending" } as const

/**
 * Resolves the references of one body in O(1) each: the projects by exact
 * name, and the decision numbers of this project's ledger and of the other
 * projects the body names (each ledger read once, as a set).
 */
export function useRefResolver(body: string): RefResolver {
  const { t, i18n } = useTranslation()
  const current = useContext(LedgerProjectContext)
  const projects = useProjects()

  const byName = useMemo(() => new Map(projects.data?.map((p) => [p.name, p.projectId]) ?? []), [projects.data])
  // The ledgers to read: this project's, and those of projects the body
  // names as "<name>#D" (a cheap scan; a name only seen in code costs a read).
  const ids = useMemo(() => {
    const others = (projects.data ?? []).filter((p) => p.projectId !== current && body.includes(`${p.name}#D`))
    return [...(current === null ? [] : [current]), ...others.map((p) => p.projectId)]
  }, [projects.data, current, body])
  const numbers = useLedgerNumbers(ids)

  return useMemo((): RefResolver => {
    const index = new Map(ids.map((id, i) => [id, i]))
    return {
      resolve(project, number) {
        if (current === null || !projects.data) return PENDING
        const projectId = project === null ? current : byName.get(project)
        if (projectId === undefined) {
          return { kind: "missing", title: t("ledger.unknownProject", { name: project ?? "" }) }
        }
        const known = numbers[index.get(projectId) ?? -1]
        if (known === undefined) return PENDING
        if (known !== "failed" && !known.has(number)) {
          return { kind: "missing", title: t("ledger.unknownDecision", { ref: `D${number}` }) }
        }
        const label = projectId === current ? `D${number}` : `${project}#D${number}`
        return { kind: "link", href: decisionPath(projectId, number), projectId, label }
      },
    }
    // The language is a dependency: the titles are made here.
    // oxlint-disable-next-line react/exhaustive-deps
  }, [ids, numbers, current, projects.data, byName, t, i18n.language])
}
