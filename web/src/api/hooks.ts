import { useMutation, useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query"
import { useEffect } from "react"

import { localDeadline } from "@/lib/enroll"

import { useClient } from "./context"
import { isApiError } from "./errors"
import { announceAuthChange, queryKeys, resetAuth, type CachedAuth } from "./query-client"
import { useSessionHold } from "./session-hold"
import type { Device, EnrollLink, EnrollRole, Project, ProjectService, SessionSummary, User } from "./types"

export function useAuth() {
  const client = useClient()
  return useQuery<CachedAuth>({
    queryKey: queryKeys.auth,
    queryFn: () => client.auth.whoAmI(),
    // Sign-in changes come from this tab's mutations, other tabs' broadcasts
    // and, for anything else (an expired cookie), a check on focus.
    staleTime: Infinity,
    refetchOnWindowFocus: "always",
  })
}

/** The signed-in user, or null while signed out. */
export function useCurrentUser(): { user: User | null; isSuper: boolean } {
  const auth = useAuth()
  const user = auth.data?.kind === "signed_in" ? auth.data.user : null
  return { user, isSuper: user?.role === "super" }
}

export function useProjects() {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.projects,
    queryFn: () => client.projects.listProjects(),
  })
}

export function useSessionList(projectId?: string) {
  const client = useClient()
  const hold = useSessionHold()
  return useQuery({
    queryKey: queryKeys.sessions(projectId),
    queryFn: () => client.sessions.listSessions({ projectId }),
    select: hold ?? undefined,
  })
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
    mutationFn: (req: { projectId: string }) => client.projects.deleteProject(req),
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
    mutationFn: (req: { projectId: string; locationIndex: number }) =>
      client.sessions.createSession(req),
    onSuccess: (session, req) => {
      // Put the session into the cached lists right away, so its page finds
      // it before the refetch lands.
      for (const key of [queryKeys.sessions(), queryKeys.sessions(session.projectId)]) {
        queryClient.setQueryData<SessionSummary[]>(key, (list) => list && [session, ...list])
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
