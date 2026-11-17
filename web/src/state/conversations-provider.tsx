import { useQueryClient } from "@tanstack/react-query"
import {
  useCallback,
  useEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
  useSyncExternalStore,
  type ReactNode,
} from "react"

import { queryKeys } from "@/api/query-client"
import { SessionHoldContext } from "@/api/session-hold"
import {
  breakTranscript,
  continueWithoutHistory,
  interruptedTurnInput,
  interruptTurn,
  startTurn,
  stopSession,
  stopTurn,
  withoutStreamingText,
  type MarkerBase,
  type SessionChange,
} from "@/lib/turns"
import type { SessionSummary } from "@/api/types"
import type {
  Conversations,
  ImageAttachment,
  SessionActivity,
  SessionEvent,
  SessionState,
  ToolCallEvent,
} from "@/lib/types"

import { createConnectionController, lostSince, LOST_NOTICE_MS } from "./connection"
import {
  ConversationSimulationContext,
  ConversationsContext,
  useConversationStore,
  type ConversationSimulation,
  type ConversationsContextValue,
} from "./conversations-context"
import { conversationsReducer } from "./conversations-reducer"
import { createSimulatedLink, defaultTiming, type SimulationTiming } from "./simulated-link"

/** How often a streaming turn is saved; the store's staleness check relies on it. */
const SAVE_INTERVAL_MS = 500

const cannedReplies = [
  "Got it. I'll take a look on the device and report back.\n\nFirst I'll check the current state of the working tree with `git status`, then run the relevant tests so we have a baseline before changing anything.",
  "Sure. Here is the plan:\n\n1. Reproduce the problem locally.\n2. Narrow it down to the smallest failing case.\n3. Fix it and add a regression test.\n\nI'll start with step 1 now.",
  "That makes sense. The change is small, so I'll keep it in a single commit and run `go vet ./...` plus the package tests before handing it back to you.",
]

const noEvents: SessionEvent[] = []

let nextId = 0
const newId = (prefix: string) => `${prefix}-${Date.now()}-${++nextId}`
const marker = (prefix: string): MarkerBase => ({ id: newId(prefix), at: new Date().toISOString() })

/**
 * What the view shows while the stream is down: the events it had when the
 * connection dropped, and once the loss is noticed, without the text that
 * was still streaming (it is ephemeral and won't be replayed).
 */
interface Hold {
  lostAt: number
  conversations: Conversations
  /**
   * The session summaries as they were, by id: from the cached lists when
   * the stream dropped, or as first seen afterwards (a list loaded for the
   * first time during the outage, e.g. when starting offline).
   */
  sessions: Map<string, SessionSummary>
}

/** The ids of the assistant texts a view of these events shows. */
const shownTextIds = (events: readonly SessionEvent[] | undefined) =>
  new Set((events ?? []).filter((e) => e.kind === "assistant_text").map((e) => e.id))

const browserOnline = () => (typeof navigator === "undefined" ? true : navigator.onLine)

const pendingApprovals = (events: SessionEvent[]) =>
  events.filter(
    (e) => e.kind === "tool_call" && e.status === "awaiting_approval" && !e.approval?.decision,
  ).length

/**
 * Simulates the conversation stream of each session until it comes from the
 * control plane. Transcripts and session state live in the store: every
 * changed transcript is saved, and every state change is reported there and
 * patched into the cached session list so the sidebar follows immediately.
 *
 * It also simulates the transport of that stream (see `connection.ts` and
 * `simulated-link.ts`). While the connection is down, simulated turns go on
 * "on the server" (the store and the cached session lists) but nothing new
 * reaches the view: the conversations stay as they were when it dropped,
 * and the session lists are pinned to that moment (`SessionHoldContext`).
 * Once the loss is noticed (after a short grace), text that was still
 * streaming goes, since the stream won't replay it. Once connected again,
 * the view catches up at once; texts left unsaved by a turn that was
 * interrupted meanwhile stay hidden, as the viewer never received them.
 *
 * Mount it per signed-in user; unmounting cancels the simulated turns.
 */
export function ConversationsProvider({
  children,
  timing: timingOverrides,
}: {
  children: ReactNode
  /** Shorter pacing for tests. */
  timing?: Partial<SimulationTiming>
}) {
  const queryClient = useQueryClient()
  const store = useConversationStore()
  const [conversations, dispatch] = useReducer(conversationsReducer, undefined, store.load)
  const [timing] = useState(() => ({ ...defaultTiming, ...timingOverrides }))

  const [{ link, controller }] = useState(() => {
    const simulated = createSimulatedLink({ latencyMs: timing.attemptLatencyMs })
    return {
      link: simulated,
      controller: createConnectionController({ attempt: simulated.attempt, online: browserOnline() }),
    }
  })
  const connection = useSyncExternalStore(controller.subscribe, controller.get)
  const lostAt = lostSince(connection)
  useEffect(() => {
    const online = () => controller.setOnline(true)
    const offline = () => controller.setOnline(false)
    window.addEventListener("online", online)
    window.addEventListener("offline", offline)
    controller.setOnline(browserOnline())
    return () => {
      window.removeEventListener("online", online)
      window.removeEventListener("offline", offline)
    }
  }, [controller])
  // Timers run while mounted. A torn-down effect pauses them; when it is set
  // up again right away (Fast Refresh, StrictMode) the schedule goes on,
  // otherwise the provider is gone and the controller closes for good.
  const pendingClose = useRef<number | null>(null)
  useEffect(() => {
    if (pendingClose.current !== null) window.clearTimeout(pendingClose.current)
    pendingClose.current = null
    controller.start()
    return () => {
      controller.dispose()
      pendingClose.current = window.setTimeout(() => controller.close(), 0)
    }
  }, [controller])

  // The view while the stream is down (see `Hold`), taken when it drops.
  const takeHold = (at: number): Hold => {
    const sessions = new Map<string, SessionSummary>()
    for (const [, list] of queryClient.getQueriesData<SessionSummary[]>({
      queryKey: queryKeys.allSessions,
    })) {
      for (const s of list ?? []) if (!sessions.has(s.sessionId)) sessions.set(s.sessionId, s)
    }
    return { lostAt: at, conversations, sessions }
  }
  const [hold, setHold] = useState<Hold | null>(() => (lostAt === null ? null : takeHold(lostAt)))
  // The loss whose grace has run out, so the view says so.
  const [noticedLoss, setNoticedLoss] = useState<number | null>(null)
  const noticed = lostAt !== null && noticedLoss === lostAt
  // Unsaved texts the view never received; they stay hidden after catching up.
  const [neverReceived, setNeverReceived] = useState<ReadonlySet<string>>(() => new Set())
  if ((hold?.lostAt ?? null) !== lostAt) {
    if (lostAt !== null) {
      setHold(takeHold(lostAt))
    } else if (hold) {
      if (noticedLoss === hold.lostAt) {
        const hidden = new Set(neverReceived)
        for (const [sessionId, events] of Object.entries(conversations)) {
          const shown = shownTextIds(withoutStreamingText(hold.conversations[sessionId] ?? []))
          for (const e of events) {
            if (e.kind === "assistant_text" && e.unsaved && !shown.has(e.id)) hidden.add(e.id)
          }
        }
        if (hidden.size !== neverReceived.size) setNeverReceived(hidden)
      }
      setHold(null)
    }
  }
  useEffect(() => {
    if (lostAt === null) return
    const timer = window.setTimeout(
      () => setNoticedLoss(lostAt),
      Math.max(0, lostAt + LOST_NOTICE_MS - Date.now()),
    )
    return () => window.clearTimeout(timer)
  }, [lostAt])

  const view = useMemo(() => {
    if (!hold) return conversations
    if (!noticed) return hold.conversations
    return Object.fromEntries(
      Object.entries(hold.conversations).map(([id, events]) => [id, withoutStreamingText(events)]),
    )
  }, [hold, noticed, conversations])

  const pinSessions = useMemo(() => {
    if (!hold) return null
    return (list: SessionSummary[]) =>
      list.map((s) => {
        const held = hold.sessions.get(s.sessionId)
        if (held) return held
        hold.sessions.set(s.sessionId, s)
        return s
      })
  }, [hold])
  // Pending timers per session, so a stop or unmount can cancel a simulated turn.
  const timers = useRef(new Map<string, number[]>())
  // Bookkeeping for saving: what the store last got from us, what still has
  // to go out, and the latest transcripts for flushes outside a render.
  const live = useRef({
    saved: conversations,
    latest: conversations,
    pending: new Set<string>(),
    lastFlushMs: 0,
    flushTimer: null as number | null,
  })

  const flush = useCallback(() => {
    const l = live.current
    if (l.flushTimer !== null) window.clearTimeout(l.flushTimer)
    l.flushTimer = null
    for (const sessionId of l.pending) {
      const events = l.latest[sessionId]
      if (events) store.save(sessionId, events)
    }
    l.pending.clear()
    l.lastFlushMs = Date.now()
  }, [store])

  // Save only the transcripts that changed, so another tab's sessions are
  // never overwritten with this tab's older copy. While a turn streams, save
  // at most every SAVE_INTERVAL_MS; once nothing is in flight, save at once.
  useEffect(() => {
    const l = live.current
    l.latest = conversations
    for (const [sessionId, events] of Object.entries(conversations)) {
      if (l.saved[sessionId] !== events) l.pending.add(sessionId)
    }
    l.saved = conversations
    if (l.pending.size === 0) return
    const wait = SAVE_INTERVAL_MS - (Date.now() - l.lastFlushMs)
    if (timers.current.size === 0 || wait <= 0) flush()
    else if (l.flushTimer === null) l.flushTimer = window.setTimeout(flush, wait)
  }, [conversations, flush])

  // Another tab changed some transcripts: take them over, except for the
  // sessions this tab is streaming right now, which keep the local copy.
  useEffect(
    () =>
      store.subscribe((sessionIds) => {
        const stored = store.load()
        for (const sessionId of sessionIds) {
          if (timers.current.has(sessionId)) continue
          const events = stored[sessionId] ?? noEvents
          live.current.saved = { ...live.current.saved, [sessionId]: events }
          dispatch({ type: "replace", sessionId, events })
        }
        void queryClient.invalidateQueries({ queryKey: queryKeys.allSessions })
      }),
    [queryClient, store],
  )

  // Back online: catch up on the session states that changed meanwhile.
  const connected = lostAt === null
  useEffect(() => {
    if (connected) void queryClient.invalidateQueries({ queryKey: queryKeys.allSessions })
  }, [connected, queryClient])

  /**
   * The session's state on the server, which the view may not show yet
   * while the connection is down (the cached lists hold it; the view sees
   * them through `pinSessions`).
   */
  const liveState = useCallback(
    (sessionId: string): SessionState | undefined => {
      for (const [, list] of queryClient.getQueriesData<SessionSummary[]>({
        queryKey: queryKeys.allSessions,
      })) {
        const session = list?.find((s) => s.sessionId === sessionId)
        if (session) return session.state
      }
      return undefined
    },
    [queryClient],
  )

  // The server's progress goes into the cached session lists at once; while
  // the connection is down the view sees them pinned (see `pinSessions`).
  const report = useCallback(
    (sessionId: string, activity: SessionActivity) => {
      store.reportActivity(sessionId, activity)
      const patch = { ...activity, lastActivityAtUnixMs: Date.now() }
      queryClient.setQueriesData<SessionSummary[]>({ queryKey: queryKeys.allSessions }, (list) =>
        list?.map((s) => (s.sessionId === sessionId ? { ...s, ...patch } : s)),
      )
      void queryClient.invalidateQueries({ queryKey: queryKeys.allSessions })
    },
    [queryClient, store],
  )

  const clearTimers = useCallback((sessionId: string) => {
    for (const t of timers.current.get(sessionId) ?? []) window.clearTimeout(t)
    timers.current.delete(sessionId)
  }, [])

  // Each timer leaves the map when it fires, so a session counts as in
  // flight exactly while it has simulated steps left.
  const schedule = useCallback((sessionId: string, fn: () => void, ms: number) => {
    const t = window.setTimeout(() => {
      const rest = (timers.current.get(sessionId) ?? []).filter((x) => x !== t)
      if (rest.length > 0) timers.current.set(sessionId, rest)
      else timers.current.delete(sessionId)
      fn()
    }, ms)
    timers.current.set(sessionId, [...(timers.current.get(sessionId) ?? []), t])
  }, [])

  // Unmounting (sign-out, a remount) cancels the simulated turns, so settle
  // what they left in flight instead of leaving it running forever.
  useEffect(() => {
    const map = timers.current
    return () => {
      const interrupted = [...map.keys()]
      for (const list of map.values()) for (const t of list) window.clearTimeout(t)
      map.clear()
      flush()
      for (const sessionId of interrupted) store.settle(sessionId)
      if (interrupted.length > 0) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.allSessions })
      }
    }
  }, [flush, queryClient, store])

  /**
   * Applies a change to the latest transcript (see `lib/turns`) and reports
   * the session progress it brings. User actions run after a render, when
   * `live.current.latest` holds the committed transcripts.
   */
  const apply = useCallback(
    (sessionId: string, change: (events: SessionEvent[]) => SessionChange) => {
      const next = change(live.current.latest[sessionId] ?? noEvents)
      dispatch({ type: "replace", sessionId, events: next.events })
      report(sessionId, next.activity)
    },
    [report],
  )

  const send = useCallback<ConversationsContextValue["send"]>(
    (sessionId, text, attachments: ImageAttachment[]) => {
      clearTimers(sessionId)
      apply(sessionId, (events) => startTurn(events, { text, attachments }, marker("user")))

      const reply = cannedReplies[nextId % cannedReplies.length]
      const words = reply.split(/(?<=\s)/)
      const ticks = Math.max(1, Math.floor(timing.streamMs / timing.streamTickMs))
      const perTick = Math.max(1, Math.ceil(words.length / ticks))
      const eventId = newId("assistant")

      schedule(
        sessionId,
        () => {
          dispatch({
            type: "append",
            sessionId,
            event: {
              kind: "assistant_text",
              id: eventId,
              at: new Date().toISOString(),
              text: "",
              streaming: true,
            },
          })
        },
        timing.firstTokenDelayMs,
      )
      for (let i = 1; i * perTick <= words.length + perTick - 1; i++) {
        const partial = words.slice(0, i * perTick).join("")
        const done = i * perTick >= words.length
        schedule(
          sessionId,
          () => {
            dispatch({
              type: "patch_event",
              sessionId,
              eventId,
              patch: { text: partial, streaming: !done },
            })
            if (done) report(sessionId, { state: "idle" })
          },
          timing.firstTokenDelayMs + i * timing.streamTickMs,
        )
      }
    },
    [apply, clearTimers, report, schedule, timing],
  )

  const stop = useCallback<ConversationsContextValue["stop"]>(
    (sessionId) => {
      clearTimers(sessionId)
      apply(sessionId, (events) => stopTurn(events, marker("stopped")))
    },
    [apply, clearTimers],
  )

  const retryTurn = useCallback<ConversationsContextValue["retryTurn"]>(
    (sessionId) => {
      // Once anything was sent the turn is no longer interrupted: nothing to retry.
      if (liveState(sessionId) !== "interrupted") return
      const input = interruptedTurnInput(live.current.latest[sessionId] ?? noEvents)
      if (input) send(sessionId, input.text, input.attachments)
    },
    [liveState, send],
  )

  const continueWithout = useCallback<ConversationsContextValue["continueWithoutHistory"]>(
    (sessionId) => apply(sessionId, (events) => continueWithoutHistory(events, marker("gap"))),
    [apply],
  )

  const stopSessionForGood = useCallback<ConversationsContextValue["stopSession"]>(
    (sessionId) => {
      clearTimers(sessionId)
      apply(sessionId, stopSession)
    },
    [apply, clearTimers],
  )

  const simulation = useMemo<ConversationSimulation>(
    () => ({
      dropConnection() {
        link.dropFor(timing.outageMs)
        controller.lost()
      },
      liveState,
      killWorker(sessionId) {
        // Only a running turn has a worker to lose.
        if (liveState(sessionId) !== "running") return
        clearTimers(sessionId)
        apply(sessionId, (events) => interruptTurn(events, marker("interrupted")))
      },
      breakTranscript(sessionId) {
        const state = liveState(sessionId)
        if (state !== "idle" && state !== "interrupted") return
        apply(sessionId, breakTranscript)
      },
    }),
    [apply, clearTimers, controller, link, liveState, timing],
  )

  const decideApproval = useCallback<ConversationsContextValue["decideApproval"]>(
    (sessionId, eventId, decision) => {
      const events = conversations[sessionId] ?? noEvents
      const event = events.find(
        (e): e is ToolCallEvent => e.id === eventId && e.kind === "tool_call",
      )
      if (!event?.approval || event.approval.decision) return

      const remaining = pendingApprovals(events) - 1
      const afterwards = remaining > 0 ? "awaiting_approval" : "idle"
      if (decision === "denied") {
        dispatch({
          type: "patch_event",
          sessionId,
          eventId,
          patch: { status: "denied", approval: { ...event.approval, decision } },
        })
        report(sessionId, { state: afterwards, pendingApprovalCount: remaining })
        return
      }

      dispatch({
        type: "patch_event",
        sessionId,
        eventId,
        patch: { status: "running", approval: { ...event.approval, decision } },
      })
      report(sessionId, { state: "running", pendingApprovalCount: remaining })
      schedule(
        sessionId,
        () => {
          dispatch({
            type: "patch_event",
            sessionId,
            eventId,
            patch: {
              status: "completed",
              output:
                "branch 'fix/auth-refresh-clock' set up to track 'origin/fix/auth-refresh-clock'.\nTo github.com:acme/circus.git\n * [new branch]      fix/auth-refresh-clock -> fix/auth-refresh-clock",
            },
          })
          report(sessionId, { state: afterwards })
        },
        1500,
      )
    },
    [conversations, report, schedule],
  )

  const value = useMemo<ConversationsContextValue>(
    () => ({
      events: (sessionId) => {
        const events = view[sessionId] ?? noEvents
        return neverReceived.size === 0 ? events : events.filter((e) => !neverReceived.has(e.id))
      },
      send,
      stop,
      decideApproval,
      retryTurn,
      continueWithoutHistory: continueWithout,
      stopSession: stopSessionForGood,
      connection,
      lossNoticed: noticed,
      retryNow: controller.retryNow,
    }),
    [
      view,
      neverReceived,
      send,
      stop,
      decideApproval,
      retryTurn,
      continueWithout,
      stopSessionForGood,
      connection,
      noticed,
      controller,
    ],
  )

  return (
    <ConversationsContext.Provider value={value}>
      <ConversationSimulationContext.Provider value={simulation}>
        <SessionHoldContext.Provider value={pinSessions}>{children}</SessionHoldContext.Provider>
      </ConversationSimulationContext.Provider>
    </ConversationsContext.Provider>
  )
}
