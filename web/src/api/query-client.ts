import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query"

import { isApiError } from "./errors"
import type { AuthState, LoginMethods } from "./types"
import { onUnauthenticated, reportIfUnauthenticated } from "./unauthenticated"

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
  workers: ["workers"] as const,
  modelProviders: ["model-providers"] as const,
  sessions: (projectId?: string) => ["sessions", projectId ?? "all"] as const,
  allSessions: ["sessions"] as const,
  session: (sessionId: string) => ["session", sessionId] as const,
  /**
   * A session's ledger snapshot, and (under it) its changed files' diffs,
   * each read for one version of the snapshot (its `updatedAtUnixMs`).
   * Invalidate the snapshot alone (`exact`): a diff is read again once the
   * snapshot's new version is in.
   */
  sessionLedger: (sessionId: string) => ["session-ledger", sessionId] as const,
  sessionLedgerDiff: (sessionId: string, path: string, version: number) =>
    ["session-ledger", sessionId, "diff", path, version] as const,
  directory: (device: string, path: string, includeHidden: boolean) =>
    ["directory", device, path, includeHidden] as const,
  /** Everything of one project's ledger: the listing, decisions and histories. */
  ledgerAll: (projectId: string) => ["ledger", projectId] as const,
  ledger: (projectId: string) => ["ledger", projectId, "entries"] as const,
  /** A decision now (`commit` empty) or at a commit. */
  decision: (projectId: string, number: number, commit: string) =>
    ["ledger", projectId, "decision", number, commit] as const,
  decisionHistory: (projectId: string, number: number) => ["ledger", projectId, "history", number] as const,
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

/**
 * Forgets the user whenever a request is rejected as unauthenticated, from
 * a query, a mutation or any Connect call (`unauthenticated.ts`): the
 * cookie expired or was revoked, so the guard sends them to /login, back to
 * the page they were on. Returns the unsubscribe.
 */
export function followUnauthenticated(queryClient: QueryClient): () => void {
  return onUnauthenticated(() => {
    if (queryClient.getQueryData<CachedAuth>(queryKeys.auth)?.kind !== "signed_in") return
    resetAuth(queryClient, { kind: "signed_out" })
  })
}

/** Creates the app's query client; queries and mutations report unauthenticated rejections. */
export function createQueryClient(): QueryClient {
  const onError = (error: unknown) => void reportIfUnauthenticated(error)

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
