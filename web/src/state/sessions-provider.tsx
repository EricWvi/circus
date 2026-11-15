import { useCallback, useEffect, useMemo, useReducer, useRef, type ReactNode } from "react"

import type { ImageAttachment, ToolCallEvent } from "@/lib/types"
import { projects, sessions as initialSessions } from "@/mock/data"

import { SessionsContext, type SessionsContextValue } from "./sessions-context"
import { sessionsReducer } from "./sessions-reducer"

const STREAM_DURATION_MS = 2000
const STREAM_TICK_MS = 80
const FIRST_TOKEN_DELAY_MS = 500

const cannedReplies = [
  "Got it. I'll take a look on the device and report back.\n\nFirst I'll check the current state of the working tree with `git status`, then run the relevant tests so we have a baseline before changing anything.",
  "Sure. Here is the plan:\n\n1. Reproduce the problem locally.\n2. Narrow it down to the smallest failing case.\n3. Fix it and add a regression test.\n\nI'll start with step 1 now.",
  "That makes sense. The change is small, so I'll keep it in a single commit and run `go vet ./...` plus the package tests before handing it back to you.",
]

let nextId = 0
const newId = (prefix: string) => `${prefix}-${Date.now()}-${++nextId}`

export function SessionsProvider({ children }: { children: ReactNode }) {
  const [sessions, dispatch] = useReducer(sessionsReducer, initialSessions)
  // Pending timers per session, so a stop or unmount can cancel a simulated turn.
  const timers = useRef(new Map<string, number[]>())

  const clearTimers = useCallback((sessionId: string) => {
    for (const t of timers.current.get(sessionId) ?? []) window.clearTimeout(t)
    timers.current.delete(sessionId)
  }, [])

  const schedule = useCallback((sessionId: string, fn: () => void, ms: number) => {
    const t = window.setTimeout(fn, ms)
    timers.current.set(sessionId, [...(timers.current.get(sessionId) ?? []), t])
  }, [])

  useEffect(() => {
    const map = timers.current
    return () => {
      for (const list of map.values()) for (const t of list) window.clearTimeout(t)
      map.clear()
    }
  }, [])

  const send = useCallback<SessionsContextValue["send"]>(
    (sessionId, text, attachments: ImageAttachment[]) => {
      clearTimers(sessionId)
      const now = new Date().toISOString()
      dispatch({
        type: "append",
        sessionId,
        event: { kind: "user_message", id: newId("user"), at: now, text, attachments },
      })
      dispatch({ type: "set_state", sessionId, state: "running" })

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
            if (done) {
              dispatch({ type: "set_state", sessionId, state: "idle" })
              timers.current.delete(sessionId)
            }
          },
          FIRST_TOKEN_DELAY_MS + i * STREAM_TICK_MS,
        )
      }
    },
    [clearTimers, schedule],
  )

  const stop = useCallback<SessionsContextValue["stop"]>(
    (sessionId) => {
      clearTimers(sessionId)
      const session = sessions.find((s) => s.id === sessionId)
      const streaming = session?.events.findLast(
        (e) => e.kind === "assistant_text" && e.streaming,
      )
      if (streaming) {
        dispatch({
          type: "patch_event",
          sessionId,
          eventId: streaming.id,
          patch: { streaming: false },
        })
      }
      dispatch({ type: "set_state", sessionId, state: "interrupted" })
    },
    [clearTimers, sessions],
  )

  const decideApproval = useCallback<SessionsContextValue["decideApproval"]>(
    (sessionId, eventId, decision) => {
      const session = sessions.find((s) => s.id === sessionId)
      const event = session?.events.find(
        (e): e is ToolCallEvent => e.id === eventId && e.kind === "tool_call",
      )
      if (!session || !event?.approval || event.approval.decision) return

      dispatch({ type: "approval_resolved", sessionId })
      const stillPending = session.pendingApprovalCount > 1
      if (decision === "denied") {
        dispatch({
          type: "patch_event",
          sessionId,
          eventId,
          patch: { status: "denied", approval: { ...event.approval, decision } },
        })
        if (!stillPending) dispatch({ type: "set_state", sessionId, state: "idle" })
        return
      }

      dispatch({
        type: "patch_event",
        sessionId,
        eventId,
        patch: { status: "running", approval: { ...event.approval, decision } },
      })
      dispatch({ type: "set_state", sessionId, state: "running" })
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
          dispatch({
            type: "set_state",
            sessionId,
            state: stillPending ? "awaiting_approval" : "idle",
          })
        },
        1500,
      )
    },
    [schedule, sessions],
  )

  const value = useMemo<SessionsContextValue>(
    () => ({ projects, sessions, send, stop, decideApproval }),
    [sessions, send, stop, decideApproval],
  )

  return <SessionsContext.Provider value={value}>{children}</SessionsContext.Provider>
}
