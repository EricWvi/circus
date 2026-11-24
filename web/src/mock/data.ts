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
 * - `stopped_tool`: the user stopped the second turn while its Bash call ran.
 * - `queued`: the second message waits for a worker to take the session.
 * - `running`: the second turn is still running.
 */
export type SeedEnding =
  | "idle"
  | "approval"
  | "interrupted"
  | "broken"
  | "stopped"
  | "stopped_tool"
  | "queued"
  | "running"

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
  /** The command waiting for approval (`approval`) or stopped while running (`stopped_tool`). */
  command?: string
  /** The model provider's id; local login when missing. */
  provider?: string
  /** The session holds a lease (it was taken by a worker and keeps it while idle). */
  leased?: boolean
  /**
   * The worker holding the lease lost its connection this many seconds
   * before the page loaded; the lease ends a minute after that.
   */
  workerDisconnectedSecondsAgo?: number
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
    ending: "stopped_tool",
    followUp: "Run the server's tests",
    command: "go test ./...",
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

/**
 * Sessions that wait, one for each reason a session can't be leased (the
 * mock's `?mock=queue` mode). "Team proxy" is limited to one session then,
 * which `proxy-holder` holds.
 */
export const queueDemoSessions: SeedSession[] = [
  {
    id: "proxy-holder",
    projectId: "circus",
    title: "Profile the event list rendering",
    minutesAgo: 12,
    device: "devbox",
    ending: "idle",
    provider: "provider-proxy",
    leased: true,
  },
  {
    id: "queue-full",
    projectId: "circus",
    title: "Add a dark mode screenshot test",
    minutesAgo: 5,
    device: "devbox",
    ending: "queued",
    followUp: "Now cover the Chinese catalog too",
    provider: "provider-proxy",
  },
  {
    id: "queue-no-worker",
    projectId: "toolbox",
    title: "Package the installer for Windows",
    minutesAgo: 6,
    device: "eric-win",
    ending: "queued",
    followUp: "Build the MSI",
    provider: "provider-anthropic",
  },
  {
    id: "queue-deleted",
    projectId: "palace",
    title: "Tune the image cache size",
    minutesAgo: 7,
    device: "palace-builder",
    ending: "queued",
    followUp: "Try 512 MiB",
    provider: "provider-gone",
  },
  {
    id: "queue-secrets",
    projectId: "palace",
    title: "Summarize last week's crash reports",
    minutesAgo: 8,
    device: "palace-builder",
    ending: "queued",
    followUp: "Group them by module",
    provider: "provider-legacy",
  },
  {
    id: "queue-offline",
    projectId: "toolbox",
    title: "Clean up the PowerShell profile",
    minutesAgo: 9,
    device: "eric-win",
    ending: "queued",
    followUp: "Remove the unused aliases",
  },
]

/**
 * Sessions whose worker lost its connection as the page loaded (the mock's
 * `?mock=worker` mode): one in the middle of a turn, one idle. The worker
 * doesn't come back, so each lease ends a minute after the disconnect (the
 * longest a server lease can last), interrupting the running turn.
 */
export const workerDemoSessions: SeedSession[] = [
  {
    id: "worker-gone-running",
    projectId: "circus",
    title: "Run the full end-to-end suite",
    minutesAgo: 0,
    device: "devbox",
    ending: "running",
    followUp: "Run it again with the race detector",
    leased: true,
    workerDisconnectedSecondsAgo: 0,
  },
  {
    id: "worker-gone-idle",
    projectId: "palace",
    title: "Check the nightly backup logs",
    minutesAgo: 1,
    device: "palace-builder",
    ending: "idle",
    leased: true,
    workerDisconnectedSecondsAgo: 0,
  },
]
