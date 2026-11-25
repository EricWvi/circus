import { useQueryClient } from "@tanstack/react-query"
import { useEffect, useState, useSyncExternalStore } from "react"

import { useClient } from "@/api/context"
import { invalidateSession, invalidateSessionLedger, trackLiveArrivals } from "@/api/hooks"

import { lostSince, LOST_NOTICE_MS } from "./connection"
import { createSessionWatch, type SessionWatch, type SessionWatchState } from "./session-watch"

const browserOnline = () => (typeof navigator === "undefined" ? true : navigator.onLine)

/**
 * Watches the session while the calling component is mounted (key it by
 * session). Boundary events refresh the session and the lists; ledger
 * events (live ones too) the session's snapshot as well.
 */
export function useSessionWatch(sessionId: string): SessionWatchState & {
  /** The current loss has lasted long enough to show (see `LOST_NOTICE_MS`). */
  lossNoticed: boolean
  retryNow: () => void
  /** The server's clock now, as far as the stream shows it. */
  serverNow: () => number
} {
  const client = useClient()
  const queryClient = useQueryClient()
  const [watch] = useState<SessionWatch>(() =>
    createSessionWatch({
      sessions: client.sessions,
      sessionId,
      online: browserOnline(),
      onBoundary: (kinds) =>
        kinds.has("ledger") ? invalidateSessionLedger(queryClient, sessionId) : invalidateSession(queryClient, sessionId),
      // Every stream after the first (not on mount, which fetches anyway),
      // a loss's reconnect included: the session may have moved on
      // meanwhile, live-only values too.
      onReopen: () => invalidateSessionLedger(queryClient, sessionId),
    }),
  )

  useEffect(
    () =>
      trackLiveArrivals(sessionId, () => watch.liveArrivals()),
    [sessionId, watch],
  )

  useEffect(() => {
    const online = () => watch.setOnline(true)
    const offline = () => watch.setOnline(false)
    window.addEventListener("online", online)
    window.addEventListener("offline", offline)
    watch.setOnline(browserOnline())
    watch.start()
    return () => {
      window.removeEventListener("online", online)
      window.removeEventListener("offline", offline)
      watch.stop()
    }
  }, [watch])

  const state = useSyncExternalStore(watch.subscribe, watch.get)
  const lostAt = lostSince(state.connection)
  const [noticedLoss, setNoticedLoss] = useState<number | null>(null)
  useEffect(() => {
    if (lostAt === null) return
    const timer = window.setTimeout(
      () => setNoticedLoss(lostAt),
      Math.max(0, lostAt + LOST_NOTICE_MS - Date.now()),
    )
    return () => window.clearTimeout(timer)
  }, [lostAt])

  return {
    ...state,
    lossNoticed: lostAt !== null && noticedLoss === lostAt,
    retryNow: watch.retryNow,
    serverNow: () => Date.now() - watch.serverOffsetMs(),
  }
}
