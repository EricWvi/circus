import { isVisibleInTree } from "./status"
import type { Project, Session } from "./types"

export interface ProjectNode {
  project: Project
  /** Visible sessions, most recent activity first. */
  sessions: Session[]
}

export function buildTree(projects: Project[], sessions: Session[]): ProjectNode[] {
  return projects.map((project) => ({
    project,
    sessions: sessions
      .filter((s) => s.projectId === project.id && isVisibleInTree(s))
      .sort((a, b) => b.lastActivityAt.localeCompare(a.lastActivityAt)),
  }))
}

export function sessionPath(session: Pick<Session, "id" | "projectId">): string {
  return `/p/${encodeURIComponent(session.projectId)}/s/${encodeURIComponent(session.id)}`
}
