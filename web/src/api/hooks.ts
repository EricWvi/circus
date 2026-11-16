import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { useClient } from "./context"
import { announceAuthChange, queryKeys, resetAuth, type CachedAuth } from "./query-client"
import type { Location, Project, SessionSummary } from "./types"

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

export function useProjects() {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.projects,
    queryFn: () => client.projects.listProjects(),
  })
}

export function useSessionList(projectId?: string) {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.sessions(projectId),
    queryFn: () => client.sessions.listSessions({ projectId }),
  })
}

export function useDevices() {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.devices,
    queryFn: () => client.machines.listDevices(),
  })
}

export function useLogin() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: { username: string; password: string }) => client.auth.login(req),
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
    mutationFn: (req: { token: string; username: string; password: string }) =>
      client.auth.setup(req),
    onSuccess: (user) => {
      resetAuth(queryClient, { kind: "signed_in", user })
      announceAuthChange()
    },
  })
}

export function useLogout() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: () => client.auth.logout(),
    onSuccess: () => {
      resetAuth(queryClient, { kind: "signed_out", afterSignOut: true })
      announceAuthChange()
    },
  })
}

export function useCreateProject() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: { name: string; locations: Location[] }) =>
      client.projects.createProject(req),
    onSuccess: (project) => {
      queryClient.setQueryData<Project[]>(queryKeys.projects, (list) =>
        list ? sortByName([...list, project]) : list,
      )
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects })
    },
  })
}

export function useUpdateProject() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: { projectId: string; name?: string; locations?: Location[] }) =>
      client.projects.updateProject(req),
    onSuccess: (project) => {
      queryClient.setQueryData<Project[]>(queryKeys.projects, (list) =>
        list
          ? sortByName(list.map((p) => (p.projectId === project.projectId ? project : p)))
          : list,
      )
      void queryClient.invalidateQueries({ queryKey: queryKeys.projects })
    },
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
  })
}

const sortByName = (projects: Project[]) =>
  projects.sort((a, b) => a.name.localeCompare(b.name))
