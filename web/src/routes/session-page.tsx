import { Navigate, useParams } from "react-router"

import { useProjects, useSessionList } from "@/api/hooks"
import { SessionView } from "@/components/session/session-view"

import { BarePage, LoadError } from "./page-states"

export function SessionPage() {
  const { projectId, sessionId } = useParams()
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

  const project = projects.data.find((p) => p.projectId === projectId)
  const session = sessions.data.find(
    (s) => s.sessionId === sessionId && s.projectId === projectId,
  )
  if (!project || !session) return <Navigate to="/" replace />

  return <SessionView key={session.sessionId} project={project} session={session} />
}
