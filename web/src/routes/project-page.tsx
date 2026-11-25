import { useQueryClient } from "@tanstack/react-query"
import { BookOpenIcon, FolderIcon, SettingsIcon } from "lucide-react"
import { useEffect, useRef, useState } from "react"
import { Trans, useTranslation } from "react-i18next"
import { Link, Navigate, useNavigate, useParams } from "react-router"

import { isApiError } from "@/api/errors"
import { useCreateSession, useDevices, useModelProviders, useProjects, useSessionList } from "@/api/hooks"
import { queryKeys } from "@/api/query-client"
import type { Device, Location, Project, ProjectLocation, SessionSummary } from "@/api/types"
import { InfoTip } from "@/components/info-tip"
import { NewSessionProviderPicker } from "@/components/model-providers/provider-picker"
import { PageHeader } from "@/components/page-header"
import { NewSessionPermissionPicker } from "@/components/session/permission-mode"
import { SessionPrompt } from "@/components/session/session-prompt"
import { StatusDot, WorkspaceMark } from "@/components/status-dot"
import { Button } from "@/components/ui/button"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { i18n } from "@/i18n/i18n"
import { locationDirectory, pickLocation, projectErrorMessage } from "@/lib/location-form"
import { DEFAULT_PERMISSION_MODE, type PermissionMode } from "@/lib/permission-mode"
import { choiceIsLocalLogin, DEFAULT_CHOICE, LOCAL_LOGIN, requestedProvider } from "@/lib/provider-choice"
import { queueReasonText } from "@/lib/queue-reason"
import { formatRelative } from "@/lib/time"
import { sessionTitle } from "@/lib/title"
import { ledgerPath, projectSettingsPath, sessionPath } from "@/lib/tree"
import { cn } from "@/lib/utils"
import { useOutbox } from "@/state/outbox-context"
import { forgetStartKey, startKey } from "@/state/start-keys"

import { BarePage, LoadError } from "./page-states"

/** A project's page: start a session in one of its locations, or open one. */
export function ProjectPage() {
  const { projectId } = useParams()
  const projects = useProjects()

  if (projects.isError && !projects.data) return <LoadError onRetry={() => void projects.refetch()} />
  if (!projects.data) return <BarePage />
  const project = projects.data.find((p) => p.projectId === projectId)
  if (!project) return <Navigate to="/" replace />
  return <ProjectHome key={project.projectId} project={project} />
}

function ProjectHome({ project }: { project: Project }) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const devices = useDevices()
  const sessions = useSessionList(project.projectId)
  const createSession = useCreateSession()
  const providers = useModelProviders()
  const outbox = useOutbox()
  // The provider picked for new sessions; null until the user picks one
  // (the request then names none, and the server applies the defaults). A
  // picked provider that is gone since counts as not picked.
  const [pickedProvider, setPickedProvider] = useState<string | null>(null)
  const [permissionMode, setPermissionMode] = useState<PermissionMode>(DEFAULT_PERMISSION_MODE)
  const providerChoice =
    pickedProvider === null ||
    pickedProvider === DEFAULT_CHOICE ||
    pickedProvider === LOCAL_LOGIN ||
    providers.data?.some((p) => p.providerId === pickedProvider) !== false
      ? pickedProvider
      : null
  // The chosen location, remembered by value: the project can change under
  // us (another tab removes or reorders locations), so the index is derived.
  // -1 when no location can be used.
  const [chosen, setChosen] = useState<Location | null>(null)
  const locationIndex = pickLocation(project, chosen)
  const noLocation =
    locationIndex >= 0
      ? null
      : project.locations.length === 0
        ? "projects.noLocationsNotice"
        : "projects.allRevokedNotice"
  // Kept as the error itself; its message is chosen at render time.
  const [error, setError] = useState<unknown>(null)

  // Set synchronously when a submit starts, so a second Enter can't start
  // another session. Released on failure.
  const inFlight = useRef(false)
  const [starting, setStarting] = useState(false)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  const start = async (text: string) => {
    if (inFlight.current) throw new Error("already starting a session")
    const location = project.locations.at(locationIndex)
    if (locationIndex < 0 || !location) throw new Error("no location to start a session in")
    inFlight.current = true
    setStarting(true)
    setError(null)
    const providerId = requestedProvider(providerChoice)
    try {
      const session = await createSession.mutateAsync({
        projectId: project.projectId,
        locationIndex,
        clientRequestId: startKey(project.projectId, locationIndex, location, providerId, permissionMode),
        ...(providerId === undefined ? {} : { providerId }),
        permissionMode,
      })
      forgetStartKey(project.projectId)
      // Once the session exists, the message belongs to it: its page shows
      // it sending and, if sending fails, failed with a retry; no other
      // session is created for it.
      outbox.send(session.sessionId, text).catch(() => {})
      // Don't pull the user back if they moved on while this was running.
      if (mounted.current) void navigate(sessionPath(session))
      else inFlight.current = false
    } catch (e) {
      inFlight.current = false
      setStarting(false)
      setError(e)
      // The key was taken by another request (which this page never sends):
      // the next try starts afresh.
      if (isApiError(e, "already_exists")) forgetStartKey(project.projectId)
      // The location is gone on the server: refresh the project's locations.
      if (locationGone(e)) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.projects })
      }
      // The provider is gone: refresh the list, which drops the choice.
      if (isApiError(e) && e.field === "provider_id") {
        void queryClient.invalidateQueries({ queryKey: queryKeys.modelProviders })
      }
      throw e
    }
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        actions={
          <div className="flex items-center gap-1">
            <Button variant="ghost" size="icon-sm" asChild>
              <Link to={ledgerPath(project.projectId)} aria-label={t("ledger.title")} title={t("ledger.title")}>
                <BookOpenIcon />
              </Link>
            </Button>
            <Button variant="ghost" size="icon-sm" asChild>
              <Link to={projectSettingsPath(project.projectId)} aria-label={t("projects.projectSettings")}>
                <SettingsIcon />
              </Link>
            </Button>
          </div>
        }
      >
        <h1 className="max-w-[65%] min-w-0 shrink-0 truncate font-medium">{project.name}</h1>
        {project.repoUrl && <RepoUrl url={project.repoUrl} />}
      </PageHeader>

      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-[760px] flex-col gap-8 px-4 py-8">
          <section className="flex flex-col gap-2" aria-label={t("projects.newSession")}>
            <SessionPrompt
              placeholder={t("projects.startPlaceholder")}
              pending={starting}
              disabledReason={noLocation && t("projects.noUsableLocation")}
              onSend={start}
            />
            {noLocation ? (
              <p className="px-2 text-sm text-muted-foreground">
                <Trans
                  i18nKey={noLocation}
                  components={{
                    settings: (
                      <Link
                        to={projectSettingsPath(project.projectId)}
                        className="rounded-sm text-foreground underline underline-offset-4 outline-none hover:text-primary focus-visible:ring-2 focus-visible:ring-ring/50"
                      />
                    ),
                  }}
                />
              </p>
            ) : (
              <>
                <div className="flex min-w-0 flex-wrap items-center gap-x-1 gap-y-1">
                  <LocationPicker
                    project={project}
                    devices={devices.data}
                    value={locationIndex}
                    disabled={starting}
                    onChange={(i) => setChosen(project.locations[i])}
                  />
                  {providers.data && (
                    <NewSessionProviderPicker
                      project={project}
                      providers={providers.data}
                      value={providerChoice}
                      disabled={starting}
                      onChange={setPickedProvider}
                    />
                  )}
                  <NewSessionPermissionPicker value={permissionMode} disabled={starting} onChange={setPermissionMode} />
                </div>
                {providers.data && choiceIsLocalLogin(providerChoice, project, providers.data) && (
                  <p className="px-2 text-xs text-muted-foreground">{t("providers.select.localLoginHint")}</p>
                )}
              </>
            )}
            {error !== null && (
              <p role="alert" className="text-sm text-destructive">
                {startErrorMessage(error)}
              </p>
            )}
          </section>

          <section className="flex flex-col gap-2">
            <h2 className="px-2 text-xs font-medium text-muted-foreground">{t("projects.sessions")}</h2>
            <SessionList sessions={sessions.data} failed={sessions.isError && !sessions.data} />
          </section>
        </div>
      </div>
    </div>
  )
}

/** The server no longer has the chosen location (or the project changed under it). */
const locationGone = (error: unknown) =>
  isApiError(error) && (error.field === "location_index" || error.field === "last_used_location")

/** Why starting a session failed, as one line. */
function startErrorMessage(error: unknown): string {
  if (locationGone(error)) return i18n.t("projects.locationGone")
  if (isApiError(error) && error.field === "provider_id") return i18n.t("validation.providerGone")
  // The key was used for another request; the page never does that.
  if (isApiError(error, "already_exists")) return i18n.t("projects.startConflict")
  return projectErrorMessage(error)
}

/** The repository URL, subtle and truncated; the full value on hover or focus. */
function RepoUrl({ url }: { url: string }) {
  const className =
    "min-w-0 truncate rounded-sm font-mono text-xs text-muted-foreground outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
  const full = <span className="break-all font-mono">{url}</span>
  if (/^https?:\/\//.test(url)) {
    return (
      <Tooltip>
        <TooltipTrigger asChild>
          <a href={url} target="_blank" rel="noreferrer noopener" className={cn(className, "hover:text-foreground hover:underline")}>
            {url}
          </a>
        </TooltipTrigger>
        <TooltipContent className="max-w-md">{full}</TooltipContent>
      </Tooltip>
    )
  }
  return (
    <InfoTip content={full} contentClassName="max-w-md">
      <span tabIndex={0} className={cn(className, "cursor-default")}>
        {url}
      </span>
    </InfoTip>
  )
}

const locationKey = (l: Location) => `${l.device}\n${l.path}`

function LocationPicker({
  project,
  devices,
  value,
  disabled,
  onChange,
}: {
  project: Project
  devices: Device[] | undefined
  value: number
  disabled: boolean
  onChange: (index: number) => void
}) {
  const { t } = useTranslation()
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  const offline = (device: string) => devices?.find((d) => d.name === device)?.online === false
  const current = project.locations[value]

  const mark = (text: string, alarming = false) => (
    <span className={cn("shrink-0", alarming ? "text-destructive" : "text-muted-foreground")}>{text}</span>
  )
  const content = (l: ProjectLocation) => (
    <span className="flex min-w-0 items-center gap-2">
      <FolderIcon className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
      <span className={cn("truncate font-mono", l.deviceRevoked && "line-through decoration-muted-foreground/60")}>
        {`${l.device}:${locationDirectory(l)}`}
      </span>
      {l.path === "" && mark(t("projects.defaultPathMark"))}
      {l.deviceRevoked && mark(t("projects.deviceRemoved"), true)}
      {!l.deviceRevoked && offline(l.device) && mark(t("common.offline"))}
    </span>
  )

  // A single location has nothing to choose; just say where sessions run.
  if (project.locations.length === 1) {
    return <div className="flex min-w-0 px-2 text-xs text-muted-foreground">{content(current)}</div>
  }
  return (
    <Select
      value={String(value)}
      onValueChange={(v) => onChange(Number(v))}
      onOpenChange={onOpenChange}
      disabled={disabled}
    >
      <SelectTrigger
        ref={triggerRef}
        size="sm"
        aria-label={t("projects.location")}
        className="max-w-full border-transparent bg-transparent px-2 text-xs text-muted-foreground shadow-none hover:bg-muted hover:text-foreground dark:bg-transparent dark:hover:bg-muted/50"
      >
        <SelectValue>{current && content(current)}</SelectValue>
      </SelectTrigger>
      <SelectContent position="popper" align="start" onCloseAutoFocus={onCloseAutoFocus}>
        {project.locations.map((l, i) => (
          <SelectItem key={locationKey(l)} value={String(i)} disabled={l.deviceRevoked} className="text-xs">
            {content(l)}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

function SessionList({ sessions, failed }: { sessions: SessionSummary[] | undefined; failed: boolean }) {
  const { t } = useTranslation()
  const providers = useModelProviders()
  /** The state, and why a queued session waits. */
  const stateTitle = (s: SessionSummary) => {
    const state = t(`status.${s.state}`)
    if (s.state !== "queued" || s.queueReason === "unspecified") return state
    const provider = providers.data?.find((p) => p.providerId === s.providerId)
    return t("session.queue.stateWithReason", { state, reason: queueReasonText(s.queueReason, provider) })
  }
  const note = (text: string) => <p className="px-2 text-sm text-muted-foreground">{text}</p>
  if (failed) return note(t("projects.sessionsLoadFailed"))
  if (!sessions) return null
  if (sessions.length === 0) return note(t("projects.noSessions"))
  const sorted = [...sessions].sort((a, b) => b.lastActivityAtUnixMs - a.lastActivityAtUnixMs)
  return (
    <ul className="flex flex-col">
      {sorted.map((s) => {
        const stopped = s.state === "stopped"
        return (
          <li key={s.sessionId}>
            <Link
              to={sessionPath(s)}
              className={cn(
                "flex min-w-0 items-center gap-3 rounded-md px-2 py-2 text-sm outline-none hover:bg-foreground/5 focus-visible:ring-2 focus-visible:ring-ring/50",
                stopped && "text-muted-foreground",
              )}
            >
              <span className="flex w-3 justify-center" title={stateTitle(s)}>
                <StatusDot state={s.state} />
              </span>
              <span className="min-w-0 flex-1 truncate">{sessionTitle(s.title)}</span>
              <WorkspaceMark workspace={s.workspace} state={s.state} />
              <time
                dateTime={new Date(s.lastActivityAtUnixMs).toISOString()}
                className="w-14 shrink-0 text-right text-xs whitespace-nowrap text-muted-foreground tabular-nums"
              >
                {formatRelative(s.lastActivityAtUnixMs)}
              </time>
            </Link>
          </li>
        )
      })}
    </ul>
  )
}
