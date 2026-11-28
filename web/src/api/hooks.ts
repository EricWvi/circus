import {
  keepPreviousData,
  replaceEqualDeep,
  useInfiniteQuery,
  useIsFetching,
  useQueries,
  useMutation,
  useMutationState,
  useQuery,
  useQueryClient,
  type MutationState,
  type QueryClient,
} from "@tanstack/react-query"
import { useEffect, useRef } from "react"

import { useShowNotice } from "@/components/notice-context"
import { i18n } from "@/i18n/i18n"
import { agentBusy, agentOf } from "@/lib/agents"
import { localDeadline } from "@/lib/enroll"

import { useClient } from "./context"
import { ApiError, isApiError, isLedgerBusy } from "./errors"
import { announceAuthChange, queryKeys, resetAuth, type CachedAuth } from "./query-client"
import type {
  AgentKind,
  ApprovalAnswer,
  CreateSessionRequest,
  Device,
  DirectoryListing,
  EnrollLink,
  EnrollRole,
  Ledger,
  MachineUpdate,
  ModelProvider,
  ModelProviderService,
  Project,
  ProjectService,
  SessionDetails,
  SessionLedger,
  SessionSummary,
  User,
  Worker,
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

type LiveArrivals = () => readonly number[]
const liveArrivals = new Map<string, Set<LiveArrivals>>()

/**
 * Lets the session's reads know when its watch's live values (queue
 * reason, worker disconnect time) arrived, this browser's clock: `arrivals`
 * returns those times. Returns the unregister.
 */
export function trackLiveArrivals(sessionId: string, arrivals: LiveArrivals): () => void {
  const set = liveArrivals.get(sessionId) ?? new Set()
  liveArrivals.set(sessionId, set)
  set.add(arrivals)
  return () => {
    set.delete(arrivals)
    if (set.size === 0) liveArrivals.delete(sessionId)
  }
}

/**
 * Keeps the cached read when a new one changes nothing the view shows: the
 * same session, and no live value that arrived between the two reads'
 * requests (the view picks the live value or the read by comparing those
 * times, see `SessionDetails.readSentAtMs`). The kept read's older send time
 * then decides the same way the new one would.
 */
function shareSessionRead(sessionId: string, old: unknown, next: unknown): unknown {
  if (old === undefined) return next
  const { readSentAtMs: oldSent = 0, ...oldRest } = old as SessionDetails
  const { readSentAtMs: nextSent = 0, ...nextRest } = next as SessionDetails
  const between = [...(liveArrivals.get(sessionId) ?? [])].some((arrivals) =>
    arrivals().some((t) => t >= oldSent && t < nextSent),
  )
  if (!between && replaceEqualDeep(oldRest, nextRest) === oldRest) return old
  return replaceEqualDeep(old, next)
}

/**
 * One session with where it runs. Its stream refreshes it at turn and
 * approval boundaries (see `useSessionWatch`); a slow poll covers the rest.
 */
export function useSession(sessionId: string) {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.session(sessionId),
    queryFn: async () => {
      const readSentAtMs = Date.now()
      const session = await client.sessions.getSession({ sessionId })
      return { ...session, readSentAtMs }
    },
    structuralSharing: (old, next) => shareSessionRead(sessionId, old, next),
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

/**
 * How long this tab waits for one session ledger read before it gives up on
 * it (`deadline_exceeded`), a little past the server's own 30 seconds, so a
 * read that hangs (a stalled connection) can't hold the queue below.
 */
export const SESSION_LEDGER_READ_TIMEOUT_MS = 35_000

/**
 * The server runs one read of a session's ledger (the snapshot or a diff)
 * per user at a time and refuses another meanwhile as busy. This tab sends
 * them one after another. A read cancelled while it waits isn't sent; one
 * cancelled or timed out while it runs is abandoned (its request aborted)
 * and the next read goes at once.
 */
let sessionLedgerReads: Promise<unknown> = Promise.resolve()

export function oneSessionLedgerRead<T>(
  read: (signal: AbortSignal) => Promise<T>,
  signal?: AbortSignal,
  timeoutMs = SESSION_LEDGER_READ_TIMEOUT_MS,
): Promise<T> {
  const run = sessionLedgerReads.then(() => {
    signal?.throwIfAborted()
    const abort = new AbortController()
    return new Promise<T>((resolve, reject) => {
      const stop = (reason: unknown) => {
        abort.abort(reason)
        reject(reason)
      }
      const timer = setTimeout(
        () => stop(new ApiError("deadline_exceeded", "The Ledger read took too long.")),
        timeoutMs,
      )
      const onAbort = () => stop(signal?.reason)
      signal?.addEventListener("abort", onAbort, { once: true })
      read(abort.signal)
        .then(resolve, reject)
        .finally(() => {
          clearTimeout(timer)
          signal?.removeEventListener("abort", onAbort)
        })
    })
  })
  sessionLedgerReads = run.catch(() => {})
  return run
}

/**
 * Retries for session ledger reads. The server's busy reply is transient
 * (another tab, or another session's drawer, holds the user's slot):
 * retried with backoff, up to six times, while the query keeps what it
 * showed, and an error only once it lasts. Other refusals are errors.
 */
export const SESSION_LEDGER_BUSY_RETRIES = 6
const sessionLedgerRetry = (failures: number, error: unknown) =>
  (isLedgerBusy(error) && failures < SESSION_LEDGER_BUSY_RETRIES) ||
  (isApiError(error, "unavailable") && failures < 2)
export const sessionLedgerRetryDelay = (attempt: number) => Math.min(250 * 2 ** attempt, 4_000)

/**
 * A session's ledger snapshot with its open changeset's changes. The
 * session watch reads it again on every KIND_LEDGER event, and
 * `useSessionLedgerFollows` when the session shows a newer version.
 */
export function useSessionLedger(sessionId: string) {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.sessionLedger(sessionId),
    queryFn: ({ signal }) =>
      oneSessionLedgerRead((abort) => client.ledger.getSessionLedger({ sessionId }, { signal: abort }), signal),
    retry: sessionLedgerRetry,
    retryDelay: sessionLedgerRetryDelay,
  })
}

/**
 * One changed file of the session's open changeset next to its base
 * version, as of the snapshot's `version`: read again only once a newer
 * snapshot is in, showing the previous version's diff meanwhile.
 */
export function useSessionLedgerDiff(sessionId: string, path: string, version: number) {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.sessionLedgerDiff(sessionId, path, version),
    queryFn: ({ signal }) =>
      oneSessionLedgerRead((abort) => client.ledger.getSessionLedgerDiff({ sessionId, path }, { signal: abort }), signal),
    placeholderData: keepPreviousData,
    retry: sessionLedgerRetry,
    retryDelay: sessionLedgerRetryDelay,
  })
}

/**
 * Reads the session's snapshot again when the session shows it changed
 * (`version` is its `updatedAtUnixMs`): a change found by a poll or a read
 * after a turn, which no live event announced. Not while the changes are
 * being read (a ledger event or a merge reads them together with the
 * session): once that read is in, its version decides, since it may have
 * started before the change.
 */
export function useSessionLedgerFollows(sessionId: string, version: number) {
  const queryClient = useQueryClient()
  const fetching = useIsFetching({ queryKey: queryKeys.sessionLedger(sessionId), exact: true }) > 0
  // The version the changes were last read again for; the one the page opened with needs no read.
  const asked = useRef(version)
  useEffect(() => {
    if (fetching || asked.current >= version) return
    const queryKey = queryKeys.sessionLedger(sessionId)
    const held = queryClient.getQueryData<SessionLedger>(queryKey)?.status?.updatedAtUnixMs ?? -1
    if (held >= version) return
    asked.current = version
    void queryClient.invalidateQueries({ queryKey, exact: true })
  }, [queryClient, sessionId, version, fetching])
}

/**
 * Refetches a session's snapshot (its diffs follow its new version) and
 * what shows it: the session and the lists.
 */
export function invalidateSessionLedger(queryClient: QueryClient, sessionId: string) {
  void queryClient.invalidateQueries({ queryKey: queryKeys.sessionLedger(sessionId), exact: true })
  invalidateSession(queryClient, sessionId)
}

/**
 * Merges the session's open changeset. Merged, the session and the
 * project's ledger are read again, and the snapshot once it changes: its
 * KIND_LEDGER event, or the session's newer version
 * (`useSessionLedgerFollows`), not while the server still applies the
 * merge. Refused, the snapshot is read again at once, since it may have
 * changed under the request.
 */
export function useMergeSessionLedger() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: { sessionId: string; projectId: string; expectedChangesetId: string }) =>
      client.ledger.mergeSessionLedger({ sessionId: req.sessionId, expectedChangesetId: req.expectedChangesetId }),
    onSuccess: (_data, { sessionId, projectId }) => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.ledgerAll(projectId) })
      invalidateSession(queryClient, sessionId)
    },
    onError: (_error, { sessionId }) => invalidateSessionLedger(queryClient, sessionId),
  })
}

/**
 * Deletes a stopped session. Once deleted it leaves the cache, so nothing
 * refetches it, and `onDeleted` runs (with the mutation, not with the
 * component that started it, so it runs even if that one stopped
 * listening); on a refusal the session is read again.
 */
export function useDeleteSession({ onDeleted }: { onDeleted?: (sessionId: string) => void } = {}) {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: { sessionId: string; discardLedgerChanges?: boolean }) => client.sessions.deleteSession(req),
    onSuccess: (_data, { sessionId }) => {
      void queryClient.cancelQueries({ queryKey: queryKeys.session(sessionId) })
      queryClient.removeQueries({ queryKey: queryKeys.session(sessionId) })
      queryClient.removeQueries({ queryKey: queryKeys.sessionLedger(sessionId) })
      queryClient.setQueriesData<SessionSummary[]>({ queryKey: queryKeys.allSessions }, (list) =>
        list?.filter((s) => s.sessionId !== sessionId),
      )
      void queryClient.invalidateQueries({ queryKey: queryKeys.allSessions })
      onDeleted?.(sessionId)
    },
    onError: (_error, { sessionId }) => invalidateSessionLedger(queryClient, sessionId),
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
    (req: { sessionId: string; approvalId: string; approve: boolean; answers?: ApprovalAnswer[] }) =>
      client.sessions.resolveApproval({ approvalId: req.approvalId, approve: req.approve, answers: req.answers }),
    (r) => r.sessionId,
  )
}

/** How often a page that shows device status refetches it while visible. */
export const DEVICES_LIVE_INTERVAL_MS = 10_000

/**
 * How often a page that shows workers refetches them while one installs or
 * uninstalls an agent (or is about to): a worker reports install progress
 * at most once a second.
 */
export const AGENTS_BUSY_INTERVAL_MS = 1_000

/**
 * After this long busy without a new request, the page follows the
 * workers every AGENTS_BUSY_SLOW_INTERVAL_MS instead: a long download, or
 * a worker that doesn't act on a wish, costs little.
 */
export const AGENTS_BUSY_FAST_MS = 60_000
export const AGENTS_BUSY_SLOW_INTERVAL_MS = 5_000

/**
 * How often a page that shows devices or workers refetches both lists
 * while a machine update runs: it ends when the machine comes back, which
 * the lists show.
 */
export const MACHINE_UPDATE_INTERVAL_MS = 2_000

/** Whether a machine update runs, as either cached list shows it. */
function machineUpdating(queryClient: QueryClient): boolean {
  const running = (list: { update?: MachineUpdate }[] | undefined) =>
    list?.some((m) => m.update?.state === "running") ?? false
  return (
    running(queryClient.getQueryData<Device[]>(queryKeys.devices)) ||
    running(queryClient.getQueryData<Worker[]>(queryKeys.workers))
  )
}

/**
 * The caller's devices. `live` keeps their status fresh while the page is
 * visible: a refetch every few seconds and whenever the window regains
 * focus. Hidden tabs don't poll.
 */
export function useDevices({ live = false }: { live?: boolean } = {}) {
  const client = useClient()
  const queryClient = useQueryClient()
  return useQuery({
    queryKey: queryKeys.devices,
    queryFn: () => client.machines.listDevices(),
    refetchInterval: live
      ? () => (machineUpdating(queryClient) ? MACHINE_UPDATE_INTERVAL_MS : DEVICES_LIVE_INTERVAL_MS)
      : false,
    refetchOnWindowFocus: live ? "always" : true,
  })
}

/**
 * The caller's workers. `live` keeps their status fresh while the page is
 * visible, as for devices, and follows an online worker's agent install
 * or uninstall (or a request it hasn't acted on yet) every second for a
 * minute from when that began or was last asked for, then every few
 * seconds until it is over. While a machine update runs (on either list),
 * both lists are read every MACHINE_UPDATE_INTERVAL_MS at most.
 */
export function useWorkers({ live = false }: { live?: boolean } = {}) {
  const client = useClient()
  const queryClient = useQueryClient()
  const busySince = useRef<number | null>(null)
  return useQuery({
    queryKey: queryKeys.workers,
    queryFn: () => client.machines.listWorkers(),
    refetchInterval: live
      ? (query) => {
          const busy = query.state.data?.some((w) =>
            agentBusy(w, (agent) => answeredReportAt(queryClient, w.workerId, agent)),
          )
          const updating = machineUpdating(queryClient) ? MACHINE_UPDATE_INTERVAL_MS : Infinity
          if (!busy) {
            busySince.current = null
            return Math.min(DEVICES_LIVE_INTERVAL_MS, updating)
          }
          const now = Date.now()
          const requests = queryClient.getMutationCache().findAll({ mutationKey: ["workerAgent"] })
          const lastRequest = Math.max(0, ...requests.map((m) => m.state.submittedAt))
          busySince.current = Math.max(busySince.current ?? now, lastRequest)
          const agents =
            now - busySince.current < AGENTS_BUSY_FAST_MS ? AGENTS_BUSY_INTERVAL_MS : AGENTS_BUSY_SLOW_INTERVAL_MS
          return Math.min(agents, updating)
        }
      : false,
    refetchOnWindowFocus: live ? "always" : true,
  })
}

/**
 * How long a request counts as not yet acted on while the list still shows
 * the report it was answered with. A worker that reported (failing again,
 * say) just before the answer was made reports nothing more: after this,
 * the report is taken as it is.
 */
export const AGENT_ANSWER_WAIT_MS = 30_000

/**
 * The report (its `reportedAtUnixMs`) that the latest successful change of
 * the worker's agent was answered with, if it was made less than
 * AGENT_ANSWER_WAIT_MS before `now`; undefined otherwise. While the list
 * shows the same report, the worker hasn't acted on it.
 */
export function answeredReportAt(
  queryClient: QueryClient,
  workerId: string,
  agent: AgentKind,
  now: number = Date.now(),
): number | undefined {
  const changes = queryClient.getMutationCache().findAll({ mutationKey: workerAgentKey(workerId, agent), exact: true })
  return answeredReport(changes.map((m) => m.state as MutationState<Worker, unknown, WorkerAgentRequest>), agent, now)
}

/** As answeredReportAt, from the changes' states (oldest first). */
export function answeredReport(
  changes: MutationState<Worker, unknown, WorkerAgentRequest>[],
  agent: AgentKind,
  now: number = Date.now(),
): number | undefined {
  const change = changes.findLast((c) => c.status === "success")
  if (!change?.data || now - change.submittedAt >= AGENT_ANSWER_WAIT_MS) return undefined
  return agentOf(change.data, agent)?.reportedAtUnixMs
}

/** The mutations that install or uninstall one agent of one worker. */
export const workerAgentKey = (workerId: string, agent: AgentKind) => ["workerAgent", workerId, agent] as const

/** What an agent change asks for: the worker and agent are the hook's. */
export interface WorkerAgentRequest {
  install: boolean
}

/**
 * The agent changes of one worker's agent, oldest first, as kept in the
 * mutation cache: like grant changes, they outlive the page that made them.
 */
export function useWorkerAgentStates(
  workerId: string,
  agent: AgentKind,
): MutationState<Worker, unknown, WorkerAgentRequest>[] {
  return useMutationState({
    filters: { mutationKey: workerAgentKey(workerId, agent), exact: true },
    select: (m) => m.state as MutationState<Worker, unknown, WorkerAgentRequest>,
  })
}

/**
 * Asks for an agent to be installed or uninstalled on a worker. The
 * returned worker goes into the cached list (unless a newer change of the
 * same agent was made meanwhile), and the list is read again, so its
 * polling follows the install from then on. A worker that is gone is
 * reported with a notice, as for grants.
 */
export function useSetWorkerAgent({ workerId, name }: Pick<Worker, "workerId" | "name">, agent: AgentKind) {
  const client = useClient()
  const queryClient = useQueryClient()
  const showNotice = useShowNotice()
  return useMutation({
    mutationKey: workerAgentKey(workerId, agent),
    mutationFn: ({ install }: WorkerAgentRequest) => client.machines.setWorkerAgent({ workerId, agent, install }),
    onSuccess: async (worker, request) => {
      await queryClient.cancelQueries({ queryKey: queryKeys.workers })
      const changes = queryClient
        .getMutationCache()
        .findAll({ mutationKey: workerAgentKey(workerId, agent), exact: true })
      const own = changes.findIndex((m) => m.state.variables === request)
      if (changes.slice(own + 1).some((m) => m.state.status !== "error")) return
      queryClient.setQueryData<Worker[]>(queryKeys.workers, (list) =>
        list?.map((w) => (w.workerId === worker.workerId ? worker : w)),
      )
      void queryClient.invalidateQueries({ queryKey: queryKeys.workers })
    },
    onError: (error) => {
      if (!isApiError(error, "not_found")) return
      void queryClient.invalidateQueries({ queryKey: queryKeys.workers })
      showNotice({
        content: () => ({
          title: i18n.t("devices.integratorWorkerGone", { name }),
          lines: [{ text: i18n.t("devices.agents.workerGoneHint") }],
        }),
        failure: true,
      })
    },
  })
}

/**
 * The message the server sent with an error (a Connect error's raw
 * message, kept as the ApiError's cause), else the error's own message.
 */
function serverMessage(error: unknown): string {
  const cause = error instanceof Error ? (error.cause as { rawMessage?: unknown } | undefined) : undefined
  if (typeof cause?.rawMessage === "string") return cause.rawMessage
  return error instanceof Error ? error.message : ""
}

/**
 * The device or worker a machine's update is asked for through: `name` is
 * its own name, `machine` the name of its machine's card.
 */
export type MachineUpdateTarget = (
  | { kind: "device"; deviceId: string; name: string }
  | { kind: "worker"; workerId: string; name: string }
) & { machine: string }

/**
 * Updates the target's machine (UpdateMachine) to the latest release. The
 * answer goes on the devices and workers of the target's machine (those
 * with its machine key) at once, and both lists are read again, so they
 * poll quickly until the update ends. A target that is gone or a machine
 * with no update to offer any more is reported with a notice.
 */
export function useUpdateMachine(target: MachineUpdateTarget) {
  const client = useClient()
  const queryClient = useQueryClient()
  const showNotice = useShowNotice()
  const key = target.kind === "device" ? target.deviceId : target.workerId
  return useMutation({
    mutationKey: ["machineUpdate", target.kind, key],
    mutationFn: () =>
      client.machines.updateMachine(target.kind === "device" ? { deviceId: target.deviceId } : { workerId: target.workerId }),
    onSuccess: async (update) => {
      await Promise.all([
        queryClient.cancelQueries({ queryKey: queryKeys.devices }),
        queryClient.cancelQueries({ queryKey: queryKeys.workers }),
      ])
      const devices = queryClient.getQueryData<Device[]>(queryKeys.devices) ?? []
      const workers = queryClient.getQueryData<Worker[]>(queryKeys.workers) ?? []
      const pressed =
        target.kind === "device"
          ? devices.find((d) => d.deviceId === target.deviceId)
          : workers.find((w) => w.workerId === target.workerId)
      const onMachine = (m: Device | Worker) =>
        pressed !== undefined && m.machineKey === pressed.machineKey
      const set = <T extends { updateVersion: string; update?: MachineUpdate }>(m: T): T => ({
        ...m,
        updateVersion: "",
        update,
      })
      queryClient.setQueryData<Device[]>(queryKeys.devices, (list) =>
        list?.map((d) => (onMachine(d) ? set(d) : d)),
      )
      queryClient.setQueryData<Worker[]>(queryKeys.workers, (list) => list?.map((w) => (onMachine(w) ? set(w) : w)))
      void queryClient.invalidateQueries({ queryKey: queryKeys.devices })
      void queryClient.invalidateQueries({ queryKey: queryKeys.workers })
    },
    onError: (error) => {
      void queryClient.invalidateQueries({ queryKey: queryKeys.devices })
      void queryClient.invalidateQueries({ queryKey: queryKeys.workers })
      const { name, machine } = target
      const gone = isApiError(error, "not_found")
      const notOffered = isApiError(error, "failed_precondition")
      const detail = error instanceof Error ? error.message : String(error)
      showNotice({
        content: () => {
          if (gone) {
            const title = target.kind === "device" ? "devices.update.deviceGone" : "devices.update.workerGone"
            return { title: i18n.t(title, { name }), lines: [{ text: i18n.t("devices.update.goneHint") }] }
          }
          if (notOffered) {
            // The server says why (already at the latest release, a
            // development build, not online as the installed service).
            return {
              title: i18n.t("devices.update.notOffered", { name: machine }),
              lines: [{ text: serverMessage(error) || i18n.t("devices.update.notOfferedHint") }],
            }
          }
          return { title: i18n.t("devices.update.startFailed", { name: machine }), lines: [{ text: detail }] }
        },
        failure: true,
      })
    },
  })
}

/** The mutations that change one worker's grants. */
export const workerGrantKey = (workerId: string) => ["workerGrant", workerId] as const

/** What a grant change asks for: the worker is the hook's. */
export interface WorkerGrantRequest {
  integrator: boolean
}

/**
 * The grant changes of one worker, oldest first, as kept in the mutation
 * cache: they outlive the page that made them, so a page opened again sees
 * a change still in flight.
 */
export function useWorkerGrantStates(workerId: string): MutationState<Worker, unknown, WorkerGrantRequest>[] {
  return useMutationState({
    filters: { mutationKey: workerGrantKey(workerId), exact: true },
    select: (m) => m.state as MutationState<Worker, unknown, WorkerGrantRequest>,
  })
}

/**
 * Turns a worker's integrator grant on or off. The returned worker goes
 * into the cached list, unless a newer change of the same worker was made
 * meanwhile (its own answer is the newer truth). A list fetched while the
 * change was in flight may predate it, so any such fetch is dropped. A
 * worker that is gone is reported with a notice, which outlives its row.
 */
export function useSetWorkerGrant({ workerId, name }: Pick<Worker, "workerId" | "name">) {
  const client = useClient()
  const queryClient = useQueryClient()
  const showNotice = useShowNotice()
  return useMutation({
    mutationKey: workerGrantKey(workerId),
    mutationFn: ({ integrator }: WorkerGrantRequest) => client.machines.setWorkerGrant({ workerId, integrator }),
    onSuccess: async (worker, request) => {
      await queryClient.cancelQueries({ queryKey: queryKeys.workers })
      const changes = queryClient.getMutationCache().findAll({ mutationKey: workerGrantKey(workerId), exact: true })
      const own = changes.findIndex((m) => m.state.variables === request)
      if (changes.slice(own + 1).some((m) => m.state.status !== "error")) return
      queryClient.setQueryData<Worker[]>(queryKeys.workers, (list) =>
        list?.map((w) => (w.workerId === worker.workerId ? worker : w)),
      )
    },
    onError: (error) => {
      if (!isApiError(error, "not_found")) return
      // The list catches up and the row goes; the notice says why.
      void queryClient.invalidateQueries({ queryKey: queryKeys.workers })
      showNotice({
        content: () => ({
          title: i18n.t("devices.integratorWorkerGone", { name }),
          lines: [{ text: i18n.t("devices.integratorWorkerGoneHint") }],
        }),
        failure: true,
      })
    },
  })
}

/**
 * The caller's model providers, ordered by name. `live` refreshes their
 * session counts while the page is visible.
 */
export function useModelProviders({ live = false }: { live?: boolean } = {}) {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.modelProviders,
    queryFn: () => client.modelProviders.listProviders(),
    refetchInterval: live ? DEVICES_LIVE_INTERVAL_MS : false,
  })
}

const sortProviders = (list: ModelProvider[]) =>
  list.sort((a, b) => a.name.toLowerCase().localeCompare(b.name.toLowerCase()) || a.providerId.localeCompare(b.providerId))

/**
 * Puts a provider the server returned into the cached list. Making it the
 * default clears the mark elsewhere, as the server does.
 */
function cacheProvider(queryClient: QueryClient, provider: ModelProvider) {
  queryClient.setQueryData<ModelProvider[]>(queryKeys.modelProviders, (list) => {
    if (!list) return list
    const others = list
      .filter((p) => p.providerId !== provider.providerId)
      .map((p) => (provider.isDefault && p.isDefault ? { ...p, isDefault: false } : p))
    return sortProviders([...others, provider])
  })
  void queryClient.invalidateQueries({ queryKey: queryKeys.modelProviders })
}

/** Refreshes what a failed provider call showed to be stale. */
function refreshAfterProviderError(queryClient: QueryClient, error: unknown) {
  if (isApiError(error, "not_found") || isApiError(error, "aborted")) {
    void queryClient.invalidateQueries({ queryKey: queryKeys.modelProviders })
  }
  if (isApiError(error) && /^worker_ids/.test(error.field ?? "")) {
    void queryClient.invalidateQueries({ queryKey: queryKeys.workers })
  }
}

export function useCreateModelProvider() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: Parameters<ModelProviderService["createProvider"]>[0]) => client.modelProviders.createProvider(req),
    // The request holds secrets: nothing keeps it once the form is gone.
    gcTime: 0,
    onSuccess: (provider) => cacheProvider(queryClient, provider),
    onError: (error) => refreshAfterProviderError(queryClient, error),
  })
}

export function useUpdateModelProvider() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: Parameters<ModelProviderService["updateProvider"]>[0]) => client.modelProviders.updateProvider(req),
    // The request holds secrets: nothing keeps it once the form is gone.
    gcTime: 0,
    onSuccess: (provider) => {
      cacheProvider(queryClient, provider)
      // Sessions waiting on it may go on now.
      void queryClient.invalidateQueries({ queryKey: queryKeys.allSessions })
    },
    onError: (error) => refreshAfterProviderError(queryClient, error),
  })
}

export function useDeleteModelProvider() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: (req: { providerId: string }) => client.modelProviders.deleteProvider(req),
    onSuccess: (_, { providerId }) => {
      queryClient.setQueryData<ModelProvider[]>(queryKeys.modelProviders, (list) =>
        list?.filter((p) => p.providerId !== providerId),
      )
      void queryClient.invalidateQueries({ queryKey: queryKeys.modelProviders })
      void queryClient.invalidateQueries({ queryKey: queryKeys.allSessions })
    },
    onError: (error) => {
      // Sessions using it hold leases now: the counts are behind.
      void queryClient.invalidateQueries({ queryKey: queryKeys.modelProviders })
      refreshAfterProviderError(queryClient, error)
    },
  })
}

/** Switches a session to another provider ("" for local login). */
export function useChangeSessionProvider() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: async (req: { sessionId: string; providerId: string }) => {
      const readSentAtMs = Date.now()
      const session = await client.sessions.updateSession(req)
      return { ...session, readSentAtMs }
    },
    onSuccess: (session) => queryClient.setQueryData(queryKeys.session(session.sessionId), session),
    onSettled: (_data, _error, { sessionId }) => {
      invalidateSession(queryClient, sessionId)
      void queryClient.invalidateQueries({ queryKey: queryKeys.modelProviders })
    },
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
/**
 * What a link is issued for: the role, the name the paste command joins
 * under (empty for the machine's host name; kept here only, the link does
 * not carry it), and for a rejoin the identity it replaces.
 */
export interface EnrollLinkRequest {
  role: EnrollRole
  name: string
  replaceId?: string
}

export function useCreateEnrollLink() {
  const client = useClient()
  return useMutation({
    mutationFn: async ({ role, replaceId }: EnrollLinkRequest): Promise<EnrollLink & { deadlineUnixMs: number }> => {
      const link = await client.machines.createEnrollLink(replaceId ? { role, replaceId } : { role })
      return { ...link, deadlineUnixMs: localDeadline(link.expiresUnixMs, Date.now()) }
    },
    gcTime: 0,
  })
}

/** A device or a worker to delete. */
export type MachineIdentity = { kind: "device"; id: string } | { kind: "worker"; id: string }

/**
 * Deletes one of the user's devices or workers. Either way the lists
 * refetch (one already gone reads as deleted: the list catches up), and
 * so do the projects, whose locations on a deleted device read as removed,
 * and the providers, which a deleted worker leaves.
 */
export function useDeleteMachine() {
  const client = useClient()
  const queryClient = useQueryClient()
  return useMutation({
    mutationFn: ({ kind, id }: MachineIdentity) =>
      kind === "device" ? client.machines.deleteDevice({ deviceId: id }) : client.machines.deleteWorker({ workerId: id }),
    onSettled: () =>
      Promise.all([
        queryClient.invalidateQueries({ queryKey: queryKeys.devices }),
        queryClient.invalidateQueries({ queryKey: queryKeys.workers }),
        queryClient.invalidateQueries({ queryKey: queryKeys.projects }),
        queryClient.invalidateQueries({ queryKey: queryKeys.modelProviders }),
      ]),
  })
}

/** The worker list, fetched now (not from the cache), for comparing against later. */
export function useFetchWorkers(): () => Promise<Worker[]> {
  const client = useClient()
  const queryClient = useQueryClient()
  return () =>
    queryClient.fetchQuery({
      queryKey: queryKeys.workers,
      queryFn: () => client.machines.listWorkers(),
      staleTime: 0,
      retry: false,
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
      queryClient.removeQueries({ queryKey: queryKeys.ledgerAll(projectId) })
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
    mutationFn: async (req: CreateSessionRequest) => {
      const readSentAtMs = Date.now()
      const session = await client.sessions.createSession(req)
      return { ...session, readSentAtMs }
    },
    onSuccess: (session: SessionDetails, req) => {
      // Put the session into the cache right away, so its page and the
      // lists show it before the refetch lands.
      queryClient.setQueryData(queryKeys.session(session.sessionId), session)
      const { device: _, cwd: __, readSentAtMs: ___, ...summary } = session
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

/** A project's ledger: its decisions on the main line. */
export function useLedger(projectId: string, { enabled = true }: { enabled?: boolean } = {}) {
  const client = useClient()
  return useQuery({
    queryKey: queryKeys.ledger(projectId),
    queryFn: () => client.ledger.getLedger({ projectId }),
    enabled,
  })
}

/** A ledger's decision numbers. */
const numbersOf = (ledger: Ledger): ReadonlySet<number> => new Set(ledger.entries.map((e) => e.number))

/**
 * What is known of each ledger's numbers: the set once loaded, "failed",
 * or undefined while loading. Stable while nothing changes.
 */
export type LedgerNumbers = ReadonlyArray<ReadonlySet<number> | "failed" | undefined>
const combineNumbers = (results: Array<{ data?: ReadonlySet<number>; isError: boolean }>): LedgerNumbers =>
  results.map((r) => r.data ?? (r.isError ? "failed" : undefined))

/**
 * The decision numbers of several projects' ledgers, in the order asked,
 * each worked out once per ledger (shares the cache with `useLedger`).
 */
export function useLedgerNumbers(projectIds: readonly string[]): LedgerNumbers {
  const client = useClient()
  return useQueries({
    queries: projectIds.map((projectId) => ({
      queryKey: queryKeys.ledger(projectId),
      queryFn: () => client.ledger.getLedger({ projectId }),
      select: numbersOf,
    })),
    combine: combineNumbers,
  })
}

/**
 * One decision, as it is now or (with `commit`) as it was at that commit.
 * A version at a commit never changes, so it is never refetched.
 */
export function useDecision(
  req: { projectId: string; number: number; commit?: string },
  { enabled = true }: { enabled?: boolean } = {},
) {
  const client = useClient()
  const { projectId, number } = req
  const commit = req.commit ?? ""
  return useQuery({
    queryKey: queryKeys.decision(projectId, number, commit),
    queryFn: () => client.ledger.getDecision({ projectId, number, commit }),
    staleTime: commit === "" ? undefined : Infinity,
    enabled,
  })
}

/** How many versions a page of a decision's history holds. */
export const HISTORY_PAGE_SIZE = 20

/** A decision's versions, newest first, loaded a page at a time. */
export function useDecisionHistory(projectId: string, number: number) {
  const client = useClient()
  return useInfiniteQuery({
    queryKey: queryKeys.decisionHistory(projectId, number),
    queryFn: ({ pageParam }) =>
      client.ledger.listDecisionHistory({ projectId, number, pageSize: HISTORY_PAGE_SIZE, pageToken: pageParam }),
    initialPageParam: "",
    getNextPageParam: (last) => last.nextPageToken || undefined,
  })
}
