// One place that hears about requests the server rejected as
// unauthenticated: the browser session expired or was signed out elsewhere.
// Connect calls (streams included) and React Query report here; the query
// client listens and forgets the user, so the guard sends them to /login.

import { isApiError } from "./errors"

const listeners = new Set<() => void>()

/** Reports `error` if it is an unauthenticated rejection. Returns the error. */
export function reportIfUnauthenticated<E>(error: E): E {
  if (isApiError(error, "unauthenticated")) for (const listener of listeners) listener()
  return error
}

/** Calls `listener` for every unauthenticated rejection. Returns an unsubscribe. */
export function onUnauthenticated(listener: () => void): () => void {
  listeners.add(listener)
  return () => listeners.delete(listener)
}
