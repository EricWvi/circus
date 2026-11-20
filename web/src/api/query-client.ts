import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query"

import { isApiError } from "./errors"
import type { AuthState, LoginMethods } from "./types"

/**
 * The cached auth state. `afterSignOut` marks an explicit sign-out, so the
 * guard sends the user to a plain /login instead of carrying the page along.
 * `methods` is missing while unknown, after a request found the user signed
 * out; the auth query is then refetched to learn them.
 */
export type CachedAuth =
  | Exclude<AuthState, { kind: "signed_out" }>
  | { kind: "signed_out"; methods?: LoginMethods; afterSignOut?: boolean }

export const queryKeys = {
  auth: ["auth"] as const,
  projects: ["projects"] as const,
  devices: ["devices"] as const,
  sessions: (projectId?: string) => ["sessions", projectId ?? "all"] as const,
  allSessions: ["sessions"] as const,
  directory: (device: string, path: string, includeHidden: boolean) =>
    ["directory", device, path, includeHidden] as const,
}

/**
 * Replaces the auth state and drops everything cached for the previous
 * user. The auth query itself stays so its observers see the new state.
 */
const isAuthQuery = (q: { queryKey: readonly unknown[] }) => q.queryKey[0] === queryKeys.auth[0]

export function resetAuth(queryClient: QueryClient, auth: CachedAuth) {
  // A whoAmI started before this change would answer for the old cookie.
  void queryClient.cancelQueries({ queryKey: queryKeys.auth }, { revert: false, silent: true })
  queryClient.removeQueries({ predicate: (q) => !isAuthQuery(q) })
  queryClient.setQueryData<CachedAuth>(queryKeys.auth, auth)
  if (auth.kind === "signed_out" && !auth.methods) {
    void queryClient.invalidateQueries({ queryKey: queryKeys.auth })
  }
}

/** Who the cached auth state belongs to, for spotting a change of user. */
const identity = (auth: CachedAuth | undefined) =>
  auth?.kind === "signed_in" ? `user:${auth.user.userId}` : (auth?.kind ?? "")

export function createQueryClient(): QueryClient {
  // A request rejected as unauthenticated means the cookie expired or was
  // revoked: forget the user so the guard sends them to /login.
  const onError = (error: unknown) => {
    if (!isApiError(error, "unauthenticated")) return
    if (queryClient.getQueryData<CachedAuth>(queryKeys.auth)?.kind !== "signed_in") return
    resetAuth(queryClient, { kind: "signed_out" })
  }

  const queryClient: QueryClient = new QueryClient({
    queryCache: new QueryCache({ onError }),
    mutationCache: new MutationCache({ onError }),
    defaultOptions: {
      queries: {
        staleTime: 10_000,
        // Only transient failures are worth retrying.
        retry: (failures, error) => isApiError(error, "unavailable") && failures < 2,
      },
    },
  })

  // When a refetch finds a different user (signed out or switched in another
  // tab), drop the previous user's data. Signed out, the guard redirects and
  // nothing may fetch; switched to another user, active queries refetch.
  let lastIdentity = ""
  queryClient.getQueryCache().subscribe((event) => {
    if (event.type !== "updated" || !isAuthQuery(event.query)) return
    const auth = event.query.state.data as CachedAuth | undefined
    const next = identity(auth)
    if (next === "" || next === lastIdentity) return
    const changed = lastIdentity !== ""
    lastIdentity = next
    if (!changed) return
    const others = { predicate: (q: { queryKey: readonly unknown[] }) => !isAuthQuery(q) }
    if (auth?.kind === "signed_in") void queryClient.resetQueries(others)
    else queryClient.removeQueries(others)
  })

  return queryClient
}

const AUTH_CHANNEL = "circus-auth"
/** Identifies this tab, so it ignores its own announcements. */
const TAB_ID = Math.random().toString(36).slice(2)

/** Tells other tabs that this one signed in or out. */
export function announceAuthChange() {
  if (typeof BroadcastChannel === "undefined") return
  const channel = new BroadcastChannel(AUTH_CHANNEL)
  channel.postMessage({ from: TAB_ID })
  channel.close()
}

/** Re-checks auth whenever another tab signs in or out. Returns a cleanup. */
export function followAuthChanges(queryClient: QueryClient): () => void {
  if (typeof BroadcastChannel === "undefined") return () => {}
  const channel = new BroadcastChannel(AUTH_CHANNEL)
  channel.onmessage = (event: MessageEvent<{ from?: string }>) => {
    if (event.data?.from === TAB_ID) return
    void queryClient.invalidateQueries({ queryKey: queryKeys.auth })
  }
  return () => channel.close()
}
