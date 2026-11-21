import { keepPreviousData, useMutation, useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query"
import { useEffect } from "react"

import { localDeadline } from "@/lib/enroll"

import { useClient } from "./context"
import { isApiError } from "./errors"
import { announceAuthChange, queryKeys, resetAuth, type CachedAuth } from "./query-client"
import type {
  CreateSessionRequest,
  Device,
  DirectoryListing,
  EnrollLink,
  EnrollRole,
  Project,
  ProjectService,
  SessionDetails,
  SessionSummary,
  User,
  WorkspaceStatus,
} from "./types"

export function useAuth() {
  const client = useClient()
  return useQuery<CachedAuth>({
    queryKey: queryKeys.auth,
    queryFn: () => client.auth.whoAmI(),
    // Sign-in changes come from this tab's mutations, other tabs'
    // broadcasts and requests rejected as unauthenticated; for anything
    // else, a check on focus, at most once a minute.
    staleTime: 60_000,
    refetchOnWindowFocus: true,
  })
}

/** The signed-in user, or null while signed out. */
export function useCurrentUser(): { user: User | null; isSuper: boolean } {
  const auth = useAuth()
  const user = auth.data?.kind === "signed_in" ? auth.data.user : null
  return { user, isSuper: user?.role === "super" }
}

/** How often the projects refetch while every unready default folder waits for its device or an update. */
export const PROJECTS_WAITING_INTERVAL_MS = 60_000

/** Waits that can last for days: until a device is back, or its circus updated. */
const longWait = (ws: WorkspaceStatus) =>
  ws.state === "pending" && (ws.waiting === "device_offline" || ws.waiting === "daemon_outdated")

/**
 * How often the projects refetch for their default folders' state: like
 * the session lists while one is being prepared or about to be, slowly
 * while each of the others waits for a device or an update, not at all
 * once none is pending or preparing.
 */
export function projectsRefetchInterval(projects: readonly Project[] | undefined): number | false {
  const unready = (projects ?? []).flatMap((p) =>
    p.locations.flatMap((l) => (l.workspace?.state === "pending" || l.workspace?.state === "preparing" ? [l.workspace] : [])),
  )
  if (unready.length === 0) return false
  return unready.every(longWait) ? PROJECTS_WAITING_INTERVAL_MS : SESSIONS_LIVE_INTERVAL_MS
}

export function useProjects() {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.projects,
    queryFn: () => client.projects.listProjects(),
    refetchInterval: (query) => projectsRefetchInterval(query.state.data),
  })
}

/** How often session lists and the open session refetch while the tab is visible. */
export const SESSIONS_LIVE_INTERVAL_MS = 5_000

export function useSessionList(projectId?: string) {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.sessions(projectId),
    queryFn: () => client.sessions.listSessions({ projectId }),
    refetchInterval: SESSIONS_LIVE_INTERVAL_MS,
  })
}

/**
 * One session with where it runs. Its stream refreshes it at turn and
 * approval boundaries (see `useSessionWatch`); a slow poll covers the rest.
 */
export function useSession(sessionId: string) {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.session(sessionId),
    queryFn: () => client.sessions.getSession({ sessionId }),
    refetchInterval: SESSIONS_LIVE_INTERVAL_MS,
  })
}

/** Refetches what a change to a session may have moved: it and the lists. */
export function invalidateSession(queryClient: QueryClient, sessionId: string) {
  void queryClient.invalidateQueries({ queryKey: queryKeys.session(sessionId) })
  void queryClient.invalidateQueries({ queryKey: queryKeys.allSessions })
}

/** A session action: the call, then a refresh of the session and the lists. */
function useSessionAction<Req>(run: (req: Req) => Promise<void>, sessionIdOf: (req: Req) => string) {
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: run,
    onSettled: (_data, _error, req) => invalidateSession(queryClient, sessionIdOf(req)),
  })
}

/** Stops the running turn. */
export function useInterrupt() {
  const client = useClient()
  return useSessionAction((req: { sessionId: string }) => client.sessions.interrupt(req), (r) => r.sessionId)
}

/** Ends the session for good. */
export function useStopSession() {
  const client = useClient()
  return useSessionAction((req: { sessionId: string }) => client.sessions.stopSession(req), (r) => r.sessionId)
}

/** Lets a broken session go on without the history it lost. */
export function useRecoverSession() {
  const client = useClient()
  return useSessionAction((req: { sessionId: string }) => client.sessions.recoverSession(req), (r) => r.sessionId)
}

/**
 * Starts the session's failed folder over. The answer is the folder's new
 * state, which the session shows at once.
 */
export function useRetryWorkspace() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: { sessionId: string }) => client.sessions.retryWorkspace(req),
    onSuccess: (workspace, { sessionId }) => {
      queryClient.setQueryData<SessionDetails>(queryKeys.session(sessionId), (s) => s && { ...s, workspace })
    },
    onSettled: (_data, _error, { sessionId }) => {
      invalidateSession(queryClient, sessionId)
      // The project's locations show the folder too.
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects })
    },
  })
}

export function useResolveApproval() {
  const client = useClient()
  return useSessionAction(
    (req: { sessionId: string; approvalId: string; approve: boolean }) =>
      client.sessions.resolveApproval({ approvalId: req.approvalId, approve: req.approve }),
    (r) => r.sessionId,
  )
}

/** How often a page that shows device status refetches it while visible. */
export const DEVICES_LIVE_INTERVAL_MS = 10_000

/**
 * The caller's devices. `live` keeps their status fresh while the page is
 * visible: a refetch every few seconds and whenever the window regains
 * focus. Hidden tabs don't poll.
 */
export function useDevices({ live = false }: { live?: boolean } = {}) {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.devices,
    queryFn: () => client.machines.listDevices(),
    refetchInterval: live ? DEVICES_LIVE_INTERVAL_MS : false,
    refetchOnWindowFocus: live ? "always" : true,
  })
}

/**
 * Fetches the device list now, whatever the cache holds (joining a fetch
 * already under way), and updates the cache with it. Fails at once instead
 * of retrying.
 */
export function useFetchDevices(): () => Promise<Device[]> {
  const client = useClient()
  const queryClient = useQueryClient()
  return () =>
    queryClient.fetchQuery({
      queryKey: queryKeys.devices,
      queryFn: () => client.machines.listDevices(),
      staleTime: 0,
      retry: false,
    })
}

/**
 * A directory on a device, asked live. While another directory loads, the
 * previous listing stays (`isPlaceholderData`). A listing is also cached
 * under the path the device resolved, so "~" and the home directory's own
 * path share one answer. Failures are not retried: the picker offers that.
 * `fetch` waits for the listing, joining a request under way.
 */
export function useDirectory(req: { device: string; path: string; includeHidden: boolean }) {
  const client = useClient()
  const queryClient = useQueryClient()
  const { device, path, includeHidden } = req
  const options = {
    queryKey: queryKeys.directory(device, path, includeHidden),
    queryFn: async (): Promise<DirectoryListing> => {
      const listing = await client.machines.listDirectory({ device, path, includeHidden })
      if (listing.path !== "" && listing.path !== path && listing.error === undefined) {
        queryClient.setQueryData(queryKeys.directory(device, listing.path, includeHidden), listing)
      }
      return listing
    },
    staleTime: 5_000,
    gcTime: 60_000,
    retry: false,
  }
  const query = useQuery({ ...options, placeholderData: keepPreviousData, refetchOnWindowFocus: false })
  return { query, fetch: () => queryClient.fetchQuery(options) }
}

/**
 * Issues an enrollment link. The link is a secret that works once: the
 * result lives only as long as the component that asked for it (no cache
 * time), so nothing keeps it after the dialog closes. `deadlineUnixMs` is
 * when it expires by this browser's clock (see `localDeadline`).
 */
export function useCreateEnrollLink() {
  const client = useClient()
  return useMutation({
    mutationFn: async (role: EnrollRole): Promise<EnrollLink & { deadlineUnixMs: number }> => {
      const link = await client.machines.createEnrollLink({ role })
      return { ...link, deadlineUnixMs: localDeadline(link.expiresUnixMs, Date.now()) }
    },
    gcTime: 0,
  })
}

export function useLogin() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: { email: string; password: string }) => client.auth.login(req),
    onSuccess: (user) => {
      resetAuth(queryClient, { kind: "signed_in", user })
      announceAuthChange()
    },
  })
}

export function useSetup() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: { token: string; email: string; password: string }) =>
      client.auth.setup(req),
    onSuccess: (user) => {
      resetAuth(queryClient, { kind: "signed_in", user })
      announceAuthChange()
    },
  })
}

/**
 * Starts signing in through the OIDC provider. With a real server the page
 * navigates away and the mutation stays pending; otherwise the new auth
 * state is fetched once the flow resolves.
 */
export function useOidcLogin() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useResetOnBackForward(useMutation({
    mutationFn: async (next?: string) => {
      await client.oidc.startLogin(next)
      return client.auth.whoAmI()
    },
    onSuccess: (auth) => {
      resetAuth(queryClient, auth)
      announceAuthChange()
    },
  }))
}

/** Starts setup through the OIDC provider; see `useOidcLogin`. */
export function useOidcSetup() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useResetOnBackForward(useMutation({
    mutationFn: async (token: string) => {
      await client.oidc.startSetup(token)
      return client.auth.whoAmI()
    },
    onSuccess: (auth) => {
      resetAuth(queryClient, auth)
      announceAuthChange()
    },
  }))
}

/**
 * Leaving for the provider keeps the mutation pending. Coming back with the
 * browser's back button can restore the page from the back/forward cache in
 * that state, so it is reset then.
 */
function useResetOnBackForward<T extends { reset: () => void }>(mutation: T): T {
  const { reset } = mutation
  useEffect(() => {
    const onPageShow = (event: PageTransitionEvent) => {
      if (event.persisted) reset()
    }
    window.addEventListener("pageshow", onPageShow)
    return () => window.removeEventListener("pageshow", onPageShow)
  }, [reset])
  return mutation
}

export function useLogout() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async () => {
      await client.auth.logout()
      // The sign-in page needs the login methods; if this fails, the cache
      // refetches them.
      const auth = await client.auth.whoAmI().catch(() => null)
      return auth?.kind === "signed_out" ? auth.methods : undefined
    },
    onSuccess: (methods) => {
      resetAuth(queryClient, { kind: "signed_out", methods, afterSignOut: true })
      announceAuthChange()
    },
  })
}

/**
 * Refreshes what a failed project call showed to be stale: the project list
 * when the project is gone (pages then leave it), the devices when a
 * location's device is unknown.
 */
function refreshAfterProjectError(queryClient: QueryClient, error: unknown) {
  if (isApiError(error, "not_found")) {
    void queryClient.invalidateQueries({ queryKey: queryKeys.projects })
  }
  if (isApiError(error) && /^locations\[\d+\]\.device$/.test(error.field ?? "")) {
    void queryClient.invalidateQueries({ queryKey: queryKeys.devices })
  }
}

export function useCreateProject() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: Parameters<ProjectService["createProject"]>[0]) =>
      client.projects.createProject(req),
    onSuccess: (project) => {
      queryClient.setQueryData<Project[]>(queryKeys.projects, (list) =>
        list ? sortByName([...list, project]) : list,
      )
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects })
    },
    onError: (error) => refreshAfterProjectError(queryClient, error),
  })
}

export function useUpdateProject() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: Parameters<ProjectService["updateProject"]>[0]) =>
      client.projects.updateProject(req),
    onSuccess: (project) => {
      queryClient.setQueryData<Project[]>(queryKeys.projects, (list) =>
        list
          ? sortByName(list.map((p) => (p.projectId === project.projectId ? project : p)))
          : list,
      )
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects })
    },
    onError: (error) => refreshAfterProjectError(queryClient, error),
  })
}

export function useDeleteProject() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: { projectId: string; removeWorkspacesOn?: string[] }) => client.projects.deleteProject(req),
    onSuccess: (_, { projectId }) => {
      // Drop the project from the cache now, so whatever renders next (the
      // landing page) never picks it, and its own session list isn't refetched.
      queryClient.removeQueries({ queryKey: queryKeys.sessions(projectId), exact: true })
      queryClient.setQueryData<Project[]>(queryKeys.projects, (list) =>
        list?.filter((p) => p.projectId !== projectId),
      )
      queryClient.setQueryData<SessionSummary[]>(queryKeys.sessions(), (list) =>
        list?.filter((s) => s.projectId !== projectId),
      )
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects })
      void queryClient.invalidateQueries({ queryKey: queryKeys.allSessions })
    },
  })
}

export function useCreateSession() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: CreateSessionRequest) => client.sessions.createSession(req),
    onSuccess: (session: SessionDetails, req) => {
      // Put the session into the cache right away, so its page and the
      // lists show it before the refetch lands.
      queryClient.setQueryData(queryKeys.session(session.sessionId), session)
      const { device: _, cwd: __, ...summary } = session
      for (const key of [queryKeys.sessions(), queryKeys.sessions(session.projectId)]) {
        queryClient.setQueryData<SessionSummary[]>(key, (list) => list && [summary, ...list])
      }
      queryClient.setQueryData<Project[]>(queryKeys.projects, (list) =>
        list?.map((p) =>
          p.projectId === req.projectId ? { ...p, lastUsedLocation: req.locationIndex } : p,
        ),
      )
      void queryClient.invalidateQueries({ queryKey: queryKeys.allSessions })
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects })
    },
    onError: (error) => refreshAfterProjectError(queryClient, error),
  })
}

const sortByName = (projects: Project[]) =>
  projects.sort((a, b) => a.name.localeCompare(b.name))
