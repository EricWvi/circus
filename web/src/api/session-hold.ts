import { createContext, useContext } from "react"

import type { SessionSummary } from "./types"

/**
 * Session state comes from the conversation stream. While the stream is
 * down, this pins the session lists to what the view last received, so a
 * refetch can't show newer state than the frozen conversation. Null while
 * connected.
 */
export const SessionHoldContext = createContext<
  ((sessions: SessionSummary[]) => SessionSummary[]) | null
>(null)

export const useSessionHold = () => useContext(SessionHoldContext)
