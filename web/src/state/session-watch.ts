// Follows one session's event stream: WatchSession from the last durable
// seq applied, reconnecting on the backoff schedule of `connection.ts`
// whenever the stream ends or fails, and building the transcript the view
// shows (`lib/session-events`).
//
// - `unavailable` (including the server's hourly end of every stream),
//   `resource_exhausted` (too many open streams), network errors and a
//   plain end of the stream: reconnect with `after_seq`; at once after a
//   stream that served a while, with the backoff after one that broke soon
//   after it opened (see `HEALTHY_STREAM_MS`).
// - `not_found`: the session is gone; no more attempts.
// - `unauthenticated`: the Connect client has already reported it (the app
//   goes to /login); no more attempts.
//
// Events arrive in small batches: those received together are applied in
// one step, so a long replay renders once.

import { isApiError } from "@/api/errors"
import type { RawSessionEvent, SessionEventKind, SessionService } from "@/api/types"
import {
  applyEvents,
  emptyTranscript,
  withoutStreaming,
  type Transcript,
} from "@/lib/session-events/transcript"

import { createConnectionController, retryDelayMs, type Connection, type NextAttempt } from "./connection"

export interface SessionWatchState {
  transcript: Transcript
  connection: Connection
  /** The session no longer exists. */
  gone: boolean
}

export interface SessionWatch {
  get(): SessionWatchState
  subscribe(listener: () => void): () => void
  /** Opens the stream (or resumes reconnecting); call from an effect's setup. */
  start(): void
  /** Closes the stream and stops timers; `start` resumes after the last event applied. */
  stop(): void
  /** Tries to reconnect now, without moving the automatic schedule. */
  retryNow(): void
  /** The browser's own network state. */
  setOnline(online: boolean): void
}

/** Events after which the session's state (GetSession) may have changed. */
export const boundaryKinds: ReadonlySet<SessionEventKind> = new Set<SessionEventKind>([
  "input_accepted",
  "turn_started",
  "result",
  "turn_stopped",
  "turn_interrupted",
  "approval_pending",
  "approval_resolved",
  "session_state",
  "transcript_gap_accepted",
  "error",
])

/** A stream that served at least this long ended normally rather than failing to stay open. */
export const HEALTHY_STREAM_MS = 10_000

export interface SessionWatchOptions {
  sessions: Pick<SessionService, "watchSession">
  sessionId: string
  /**
   * Durable events of these kinds arrived (once per batch, only between
   * `start` and `stop`). After a RESULT it is called once more,
   * `afterResultMs` later: the worker publishes RESULT just before it
   * reports the turn's end, so the session read right after may still run.
   */
  onBoundary?: (kinds: ReadonlySet<SessionEventKind>) => void
  afterResultMs?: number
  online?: boolean
}

export function createSessionWatch({
  sessions,
  sessionId,
  onBoundary = () => {},
  afterResultMs = 1_000,
  online = true,
}: SessionWatchOptions): SessionWatch {
  const resultTimers = new Set<ReturnType<typeof setTimeout>>()
  let transcript = emptyTranscript
  let gone = false
  /** Watching ended for good (gone, or signed out). */
  let ended = false
  /** Between `start` and `stop`. */
  let running = false
  let stream: AbortController | null = null
  /** Streams in a row that broke soon after they opened. */
  let shortLived = 0
  let pending: RawSessionEvent[] = []
  let flushTimer: ReturnType<typeof setTimeout> | null = null
  const listeners = new Set<() => void>()

  const controller = createConnectionController({ attempt: () => attempt(), online })
  let state: SessionWatchState = { transcript, connection: controller.get(), gone }

  const publish = () => {
    const connection = controller.get()
    // Keepalives and live frames that change nothing shown keep the state as it was.
    const sameView =
      transcript.events === state.transcript.events &&
      transcript.lastSeq === state.transcript.lastSeq &&
      transcript.turnOpen === state.transcript.turnOpen &&
      transcript.turnNo === state.transcript.turnNo
    if (sameView && connection === state.connection && gone === state.gone) return
    state = { transcript, connection, gone }
    for (const listener of listeners) listener()
  }
  controller.subscribe(publish)

  const flush = () => {
    if (flushTimer !== null) clearTimeout(flushTimer)
    flushTimer = null
    if (pending.length === 0) return
    const batch = pending
    pending = []
    const before = transcript.lastSeq
    transcript = applyEvents(transcript, batch)
    const kinds = new Set<SessionEventKind>()
    for (const e of batch) if (e.seq > before && boundaryKinds.has(e.kind)) kinds.add(e.kind)
    publish()
    if (kinds.size === 0 || !running) return
    onBoundary(kinds)
    if (kinds.has("result")) {
      const timer = setTimeout(() => {
        resultTimers.delete(timer)
        if (running) onBoundary(new Set<SessionEventKind>(["result"]))
      }, afterResultMs)
      resultTimers.add(timer)
    }
  }

  const receive = (event: RawSessionEvent) => {
    pending.push(event)
    flushTimer ??= setTimeout(flush, 0)
  }

  const closeStream = () => {
    stream?.abort()
    stream = null
  }

  /** Ends watching for good: the session is gone, or the user is signed out. */
  const end = (error: unknown) => {
    if (isApiError(error, "not_found")) gone = true
    ended = true
    closeStream()
    controller.close()
    publish()
  }
  const isFinal = (error: unknown) => isApiError(error, "not_found") || isApiError(error, "unauthenticated")

  type Outcome = "opened" | "failed" | "final" | "aborted"

  /**
   * Opens a stream after the last durable event. Resolves `opened` once the
   * server answered (with a first event, a keepalive at least), `failed`
   * or `final` (see `isFinal`) if it failed first, `aborted` when it was
   * closed meanwhile. A failure after it opened reports a loss.
   */
  function open(): Promise<Outcome> {
    closeStream()
    flush()
    const abort = new AbortController()
    stream = abort
    const afterSeq = transcript.lastSeq
    return new Promise((resolve) => {
      let opened = false
      let openedAt = 0
      void (async () => {
        try {
          for await (const event of sessions.watchSession({ sessionId, afterSeq, signal: abort.signal })) {
            if (abort.signal.aborted) break
            if (!opened) {
              opened = true
              openedAt = Date.now()
              // Whatever streamed live before the break won't come again.
              transcript = withoutStreaming(transcript)
              publish()
              resolve("opened")
            }
            receive(event)
          }
          if (!abort.signal.aborted) throw new Error("the session stream ended")
          resolve("aborted")
        } catch (error) {
          if (abort.signal.aborted) return resolve("aborted")
          flush()
          if (isFinal(error)) {
            end(error)
            return resolve("final")
          }
          if (stream === abort) stream = null
          resolve("failed")
          // A stream that broke after it opened is a loss. Reported after
          // the attempt that opened it has settled as connected.
          if (opened) {
            const next = afterLoss(Date.now() - openedAt)
            setTimeout(() => {
              if (running && !ended && stream === null) controller.lost(next)
            }, 0)
          }
        }
      })()
    })
  }

  /**
   * The first attempt after a stream that lived `livedMs`. One that served
   * for a while (the server ends every stream after about an hour) is
   * reopened at once; one that broke soon after it opened goes on with the
   * backoff, as if its opening had failed.
   */
  function afterLoss(livedMs: number): NextAttempt {
    shortLived = livedMs < HEALTHY_STREAM_MS ? shortLived + 1 : 0
    if (shortLived === 0) return { attempt: 1, delayMs: 0 }
    return { attempt: shortLived + 1, delayMs: retryDelayMs(shortLived + 1) }
  }

  async function attempt(): Promise<boolean> {
    return (await open()) === "opened"
  }

  return {
    get: () => state,
    subscribe(listener) {
      listeners.add(listener)
      return () => listeners.delete(listener)
    },
    start() {
      if (ended) return
      running = true
      controller.start()
      if (controller.get().kind === "connected" && stream === null) {
        void open().then((outcome) => {
          if (outcome === "failed" && running) controller.lost()
        })
      }
    },
    stop() {
      running = false
      closeStream()
      flush()
      for (const timer of resultTimers) clearTimeout(timer)
      resultTimers.clear()
      controller.dispose()
    },
    retryNow: () => controller.retryNow(),
    setOnline(next) {
      if (!next) closeStream()
      controller.setOnline(next)
    },
  }
}
