// The browser's connection to the control plane, as the session view shows
// it: the backoff and state core, independent of what carries the stream.
// The owner reports `lost()` when the stream breaks and `resumed()` when it
// comes back by itself; between the two, the core calls the injected
// `attempt()` on a backoff schedule until one succeeds. Going offline counts
// as losing the stream; coming back online reconnects (and catches up)
// rather than jumping straight to connected.
//
// Timers run between `start()` and `dispose()`; the owner calls them from
// an effect's setup and cleanup. `dispose()` stops timers and drops a running
// attempt; a later `start()` (or `lost()`) picks the schedule up again, so an
// effect that is torn down and set up again (Fast Refresh, StrictMode) loses
// nothing. `close()` ends the controller for good (its owner is gone): late
// callbacks can't start it again.

export type Connection =
  | { kind: "connected" }
  | {
      kind: "reconnecting"
      /** When the stream was lost (the outage, not this attempt). */
      lostAt: number
      /** The attempt that is next (or running), from 1. */
      attempt: number
      /** When the next attempt starts; null while an attempt runs. */
      nextRetryAt: number | null
    }
  | { kind: "offline"; lostAt: number }

export const FIRST_RETRY_MS = 1_000
export const MAX_RETRY_MS = 30_000
/** How long the connection has to be lost before the UI says so. */
export const LOST_NOTICE_MS = 2_000

/** Wait before reconnect attempt `attempt` (1-based): 1, 2, 4, 8… s, at most 30 s. */
export function retryDelayMs(attempt: number): number {
  return Math.min(FIRST_RETRY_MS * 2 ** (attempt - 1), MAX_RETRY_MS)
}

/** Since when the connection is lost, or null while connected. */
export const lostSince = (connection: Connection): number | null =>
  connection.kind === "connected" ? null : connection.lostAt

/**
 * Which attempt comes first after a loss, and how soon: by default attempt
 * 1 after `retryDelayMs(1)`.
 */
export interface NextAttempt {
  attempt: number
  delayMs: number
}

export interface ConnectionController {
  get(): Connection
  subscribe(listener: () => void): () => void
  /**
   * The stream broke. Starts reconnecting unless already lost; also starts
   * the controller if it was disposed, so a lost link always has a timer.
   * `next` sets the first attempt and its delay (see `NextAttempt`).
   */
  lost(next?: NextAttempt): void
  /** The stream is back by itself. */
  resumed(): void
  /**
   * Attempts to reconnect now. Doesn't move the automatic schedule: after a
   * failed manual attempt the attempt number and deadline stay as they were.
   * Does nothing while an attempt runs, while connected or while offline.
   */
  retryNow(): void
  /** The browser's own network state. */
  setOnline(online: boolean): void
  /**
   * Arms the schedule: a pending attempt without a timer (after `dispose`,
   * or when starting lost) gets one again, firing at once if it is due.
   */
  start(): void
  /**
   * Stops every timer and drops an attempt still running (its answer is
   * ignored; the same attempt waits to run again). Publishes the resulting
   * state. `start()` resumes the schedule.
   */
  dispose(): void
  /**
   * Stops for good, like `dispose()`, after which nothing (`lost()`,
   * `retryNow()`, `start()`, network changes) starts the controller again.
   */
  close(): void
}

export interface ConnectionOptions {
  /** Tries to bring the stream back; resolves whether it worked. */
  attempt: () => Promise<boolean>
  /** Whether the browser starts online; starting offline starts lost. */
  online?: boolean
}

type Link =
  | { phase: "connected" }
  | { phase: "waiting"; lostAt: number; attempt: number; deadline: number }
  | { phase: "attempting"; lostAt: number; attempt: number; deadline: number | null; manual: boolean }

export function createConnectionController({
  attempt: tryReconnect,
  online = true,
}: ConnectionOptions): ConnectionController {
  let browserOnline = online
  let link: Link = online
    ? { phase: "connected" }
    : { phase: "waiting", lostAt: Date.now(), attempt: 1, deadline: Date.now() }
  let timer: ReturnType<typeof setTimeout> | null = null
  // Bumped whenever a running attempt's result no longer matters.
  let generation = 0
  // Between start() and dispose(); timers are only set while running.
  let running = true
  // After close(): nothing starts it again.
  let closed = false
  const listeners = new Set<() => void>()

  const view = (): Connection => {
    if (link.phase === "connected") return { kind: "connected" }
    if (!browserOnline) return { kind: "offline", lostAt: link.lostAt }
    return {
      kind: "reconnecting",
      lostAt: link.lostAt,
      attempt: link.attempt,
      nextRetryAt: link.phase === "waiting" ? link.deadline : null,
    }
  }
  let current = view()

  const publish = () => {
    const next = view()
    if (JSON.stringify(next) === JSON.stringify(current)) return
    current = next
    for (const listener of listeners) listener()
  }

  const cancelTimer = () => {
    if (timer !== null) clearTimeout(timer)
    timer = null
  }

  /** Gives a waiting link its timer (offline: none, it waits for online). */
  const arm = () => {
    cancelTimer()
    if (link.phase !== "waiting" || !browserOnline || !running) return
    timer = setTimeout(() => run(false), Math.max(0, link.deadline - Date.now()))
  }

  /** Waits for attempt `n` until `deadline`. */
  const wait = (lostAt: number, n: number, deadline: number) => {
    link = { phase: "waiting", lostAt, attempt: n, deadline }
    arm()
    publish()
  }

  function run(manual: boolean) {
    if (link.phase !== "waiting" || !browserOnline || !running) return
    cancelTimer()
    const { lostAt, attempt, deadline } = link
    link = { phase: "attempting", lostAt, attempt, deadline: manual ? deadline : null, manual }
    const mine = ++generation
    publish()
    void tryReconnect().then(
      (ok) => settle(mine, ok),
      () => settle(mine, false),
    )
  }

  function settle(mine: number, ok: boolean) {
    if (mine !== generation || link.phase !== "attempting") return
    const { lostAt, attempt, deadline, manual } = link
    if (ok) {
      link = { phase: "connected" }
      publish()
    } else if (manual && deadline !== null) {
      // A manual retry leaves the automatic schedule where it was.
      wait(lostAt, attempt, deadline)
    } else {
      wait(lostAt, attempt + 1, Date.now() + retryDelayMs(attempt + 1))
    }
  }

  function dispose() {
    running = false
    cancelTimer()
    generation++
    // An attempt that was running will never answer; it runs again on start.
    if (link.phase === "attempting") {
      const deadline = link.deadline ?? Date.now()
      link = { phase: "waiting", lostAt: link.lostAt, attempt: link.attempt, deadline }
    }
    publish()
  }

  return {
    get: () => current,
    subscribe(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    lost(next) {
      if (closed) return
      // Reporting a loss is using the controller: it starts if it was disposed.
      running = true
      if (link.phase === "connected") {
        const now = Date.now()
        const attempt = Math.max(1, next?.attempt ?? 1)
        wait(now, attempt, now + Math.max(0, next?.delayMs ?? retryDelayMs(attempt)))
      } else if (link.phase === "waiting" && timer === null) {
        arm()
      }
    },
    resumed() {
      if (closed) return
      cancelTimer()
      generation++
      link = { phase: "connected" }
      publish()
    },
    retryNow() {
      if (closed) return
      // Like lost(): asking to reconnect starts a disposed controller.
      running = true
      run(true)
    },
    setOnline(next) {
      if (closed || next === browserOnline) return
      browserOnline = next
      if (!next) {
        // Offline loses the stream; whatever was scheduled or running is moot.
        cancelTimer()
        generation++
        const lostAt = link.phase === "connected" ? Date.now() : link.lostAt
        link = { phase: "waiting", lostAt, attempt: 1, deadline: Date.now() }
        publish()
      } else if (link.phase === "waiting") {
        // Back online: reconnect (and catch up) at once.
        wait(link.lostAt, 1, Date.now())
      }
    },
    start() {
      if (closed) return
      running = true
      arm()
    },
    close() {
      dispose()
      closed = true
    },
    dispose,
  }
}
