import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { useClient } from "./context"
import { announceAuthChange, queryKeys, resetAuth, type CachedAuth } from "./query-client"

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
