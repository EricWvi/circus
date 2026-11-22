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

/** A project's ledger page, with no decision selected. */
export const ledgerPath = (projectId: string) => `${projectPath(projectId)}/ledger`

/**
 * A decision's page; `at` shows it as of that commit, `history` its
 * versions instead of its body.
 */
export function decisionPath(
  projectId: string,
  number: number,
  { at, history = false }: { at?: string; history?: boolean } = {},
): string {
  const query = new URLSearchParams()
  if (at) query.set("at", at)
  if (history) query.set("view", "history")
  const search = query.toString()
  return `${ledgerPath(projectId)}/D${number}${search ? `?${search}` : ""}`
}

/** The highest decision number (a uint32). */
export const MAX_DECISION_NUMBER = 0xffffffff

/**
 * The number in a decision's path segment ("D12"), or null when it isn't
 * one. Leading zeros, a lowercase "d" and surrounding spaces are accepted
 * here ("D012", "d12" and "D12 " are 12); the page then moves to the
 * canonical path.
 */
export function parseDecisionSegment(segment: string | undefined): number | null {
  const digits = /^[Dd]([0-9]{1,12})$/.exec((segment ?? "").trim())?.[1]
  if (digits === undefined) return null
  const number = Number(digits)
  return number <= MAX_DECISION_NUMBER ? number : null
}
