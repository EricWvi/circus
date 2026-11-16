import { FolderIcon, PlusIcon } from "lucide-react"
import { Navigate } from "react-router"

import { useProjects, useSessionList } from "@/api/hooks"
import { Button } from "@/components/ui/button"
import {
  Empty,
  EmptyContent,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import { isVisibleInTree } from "@/lib/status"
import { sessionPath } from "@/lib/tree"

import { BarePage, LoadError } from "./page-states"

/** Opens the most recently active session, or offers to create a project. */
export function HomePage() {
  const projects = useProjects()
  const sessions = useSessionList()

  // Only a failed first load replaces the page; a failed refetch keeps the data.
  if ((projects.isError && !projects.data) || (sessions.isError && !sessions.data)) {
    return (
      <LoadError
        onRetry={() => {
          void projects.refetch()
          void sessions.refetch()
        }}
      />
    )
  }
  if (!projects.data || !sessions.data) return <BarePage />

  if (projects.data.length === 0) {
    return (
      <BarePage>
        <Empty className="flex-1">
          <EmptyHeader>
            <EmptyMedia variant="icon">
              <FolderIcon />
            </EmptyMedia>
            <EmptyTitle>No projects</EmptyTitle>
          </EmptyHeader>
          <EmptyContent>
            <Button>
              <PlusIcon />
              New project
            </Button>
          </EmptyContent>
        </Empty>
      </BarePage>
    )
  }

  const projectIds = new Set(projects.data.map((p) => p.projectId))
  const latest = sessions.data
    .filter((s) => projectIds.has(s.projectId) && isVisibleInTree(s))
    .sort((a, b) => b.lastActivityAtUnixMs - a.lastActivityAtUnixMs)[0]
  if (latest) return <Navigate to={sessionPath(latest)} replace />
  return (
    <BarePage>
      <Empty className="flex-1">
        <EmptyHeader>
          <EmptyTitle>No active sessions</EmptyTitle>
        </EmptyHeader>
      </Empty>
    </BarePage>
  )
}
