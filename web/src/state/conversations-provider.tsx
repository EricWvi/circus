import { useQueryClient } from "@tanstack/react-query"
import { useCallback, useEffect, useMemo, useReducer, useRef, type ReactNode } from "react"

import { queryKeys } from "@/api/query-client"
import { titleFromMessage } from "@/lib/title"
import type { SessionSummary } from "@/api/types"
import type {
  ImageAttachment,
  SessionActivity,
  SessionEvent,
  ToolCallEvent,
} from "@/lib/types"

import {
  ConversationsContext,
  useConversationStore,
  type ConversationsContextValue,
} from "./conversations-context"
import { conversationsReducer } from "./conversations-reducer"

const STREAM_DURATION_MS = 2000
const STREAM_TICK_MS = 80
const FIRST_TOKEN_DELAY_MS = 500
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
 * Mount it per signed-in user; unmounting cancels the simulated turns.
 */
export function ConversationsProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient()
  const store = useConversationStore()
  const [conversations, dispatch] = useReducer(conversationsReducer, undefined, store.load)
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

  const send = useCallback<ConversationsContextValue["send"]>(
    (sessionId, text, attachments: ImageAttachment[]) => {
      clearTimers(sessionId)
      const first = !(live.current.latest[sessionId] ?? noEvents).some(
        (e) => e.kind === "user_message",
      )
      dispatch({
        type: "append",
        sessionId,
        event: {
          kind: "user_message",
          id: newId("user"),
          at: new Date().toISOString(),
          text,
          attachments,
        },
      })
      // The first message names the session, as the server would.
      const title = first ? titleFromMessage(text) : null
      report(sessionId, title ? { state: "running", title } : { state: "running" })

      const reply = cannedReplies[nextId % cannedReplies.length]
      const words = reply.split(/(?<=\s)/)
      const ticks = Math.floor(STREAM_DURATION_MS / STREAM_TICK_MS)
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
        FIRST_TOKEN_DELAY_MS,
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
          FIRST_TOKEN_DELAY_MS + i * STREAM_TICK_MS,
        )
      }
    },
    [clearTimers, report, schedule],
  )

  const stop = useCallback<ConversationsContextValue["stop"]>(
    (sessionId) => {
      clearTimers(sessionId)
      dispatch({ type: "settle", sessionId })
      report(sessionId, { state: "interrupted" })
    },
    [clearTimers, report],
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
      events: (sessionId) => conversations[sessionId] ?? noEvents,
      send,
      stop,
      decideApproval,
    }),
    [conversations, send, stop, decideApproval],
  )

  return <ConversationsContext.Provider value={value}>{children}</ConversationsContext.Provider>
}
