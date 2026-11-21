import { useTranslation } from "react-i18next"
import { Link, Navigate, useParams } from "react-router"

import { isApiError } from "@/api/errors"
import { useProjects, useSession } from "@/api/hooks"
import { SessionView } from "@/components/session/session-view"
import { Button } from "@/components/ui/button"
import { projectPath } from "@/lib/tree"

import { BarePage, LoadError } from "./page-states"

export function SessionPage() {
  const { projectId = "", sessionId = "" } = useParams()
  const projects = useProjects()
  const session = useSession(sessionId)

  if (isApiError(session.error, "not_found") && !session.data) return <SessionGone projectId={projectId} />
  // Only a failed first load replaces the page; a failed refetch keeps the data.
  if ((projects.isError && !projects.data) || (session.isError && !session.data)) {
    return (
      <LoadError
        onRetry={() => {
          void projects.refetch()
          void session.refetch()
        }}
      />
    )
  }
  if (!projects.data || !session.data) return <BarePage />

  const project = projects.data.find((p) => p.projectId === projectId)
  if (!project || session.data.projectId !== projectId) return <Navigate to="/" replace />

  return <SessionView key={sessionId} project={project} session={session.data} />
}

/** The session doesn't exist (any more). */
function SessionGone({ projectId }: { projectId: string }) {
  const { t } = useTranslation()
  return (
    <BarePage>
      <div className="flex flex-1 flex-col items-center justify-center gap-4 p-6">
        <p className="text-sm text-muted-foreground">{t("session.gone")}</p>
        <Button variant="outline" asChild>
          <Link to={projectId ? projectPath(projectId) : "/"}>{t("session.backToProject")}</Link>
        </Button>
      </div>
    </BarePage>
  )
}
