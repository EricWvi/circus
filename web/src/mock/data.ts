import type { Location } from "@/api/types"

/** Demo data of the mock backend (`api/mock`). */

export interface SeedProject {
  id: string
  name: string
  locations: Location[]
}

/**
 * How a seeded session's history ends, after a first exchange:
 * - `idle`: nothing more.
 * - `approval`: a tool call waits for approval.
 * - `interrupted`: the worker was lost in the second turn.
 * - `broken`: the second turn's transcript couldn't be saved.
 * - `stopped`: the user stopped the session.
 */
export type SeedEnding = "idle" | "approval" | "interrupted" | "broken" | "stopped"

export interface SeedSession {
  id: string
  projectId: string
  /** The first message, which names the session. */
  title: string
  /** Minutes before the page loaded. */
  minutesAgo: number
  device: string
  ending: SeedEnding
  /** The second message, for endings that have a second turn. */
  followUp?: string
  /** The command waiting for approval (`approval`). */
  command?: string
}

export const projects: SeedProject[] = [
  {
    id: "circus",
    name: "circus",
    locations: [
      { device: "eric-mbp", path: "/Users/eric/src/circus" },
      { device: "devbox", path: "/home/eric/src/circus" },
    ],
  },
  {
    id: "palace",
    name: "palace",
    locations: [
      { device: "palace-builder", path: "/srv/palace" },
      { device: "eric-mbp", path: "/Users/eric/src/palace" },
    ],
  },
  {
    id: "toolbox",
    name: "toolbox",
    locations: [
      { device: "eric-win", path: "C:\\Users\\eric\\src\\toolbox" },
      { device: "devbox", path: "/home/eric/src/toolbox" },
    ],
  },
]

export const sessions: SeedSession[] = [
  {
    id: "fix-auth-refresh",
    projectId: "circus",
    title: "Fix flaky auth token refresh tests",
    minutesAgo: 8,
    device: "eric-mbp",
    ending: "approval",
    followUp: "Push the fix to a branch",
    command: "git push -u origin fix/auth-refresh-clock",
  },
  {
    id: "web-vite",
    projectId: "circus",
    title: "Which Vite version does web use?",
    minutesAgo: 179,
    device: "devbox",
    ending: "idle",
  },
  {
    id: "quic-spike",
    projectId: "circus",
    title: "Spike: QUIC transport for the worker link",
    minutesAgo: 60 * 24 * 6,
    device: "devbox",
    ending: "stopped",
  },
  {
    id: "libvips",
    projectId: "palace",
    title: "Migrate thumbnailer to libvips",
    minutesAgo: 94,
    device: "palace-builder",
    ending: "broken",
    followUp: "Run the thumbnail benchmarks",
  },
  {
    id: "palace-new",
    projectId: "palace",
    title: "Add a health check endpoint",
    minutesAgo: 30,
    device: "eric-mbp",
    ending: "idle",
  },
  {
    id: "dotfiles-go",
    projectId: "toolbox",
    title: "Rewrite the dotfiles installer in Go",
    minutesAgo: 60 * 26,
    device: "eric-win",
    ending: "interrupted",
    followUp: "Rewrite install.sh in Go",
  },
  {
    id: "golangci-v2",
    projectId: "toolbox",
    title: "Bump golangci-lint to v2",
    minutesAgo: 60 * 50,
    device: "devbox",
    ending: "idle",
  },
]
