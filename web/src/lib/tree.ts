import type { Project, SessionSummary } from "@/api/types"

import { isVisibleInTree } from "./status"

export interface ProjectNode {
  project: Project
  /** Visible sessions, most recent activity first. */
  sessions: SessionSummary[]
}

export function buildTree(projects: Project[], sessions: SessionSummary[]): ProjectNode[] {
  return projects.map((project) => ({
    project,
    sessions: sessions
      .filter((s) => s.projectId === project.projectId && isVisibleInTree(s))
      .sort((a, b) => b.lastActivityAtUnixMs - a.lastActivityAtUnixMs),
  }))
}

export function sessionPath(session: Pick<SessionSummary, "sessionId" | "projectId">): string {
  return `/p/${encodeURIComponent(session.projectId)}/s/${encodeURIComponent(session.sessionId)}`
}

export const projectPath = (projectId: string) => `/p/${encodeURIComponent(projectId)}`

export const projectSettingsPath = (projectId: string) => `${projectPath(projectId)}/settings`
