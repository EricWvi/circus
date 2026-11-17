import { useQueryClient } from "@tanstack/react-query"
import { FolderIcon, SettingsIcon } from "lucide-react"
import { useEffect, useRef, useState } from "react"
import { Trans, useTranslation } from "react-i18next"
import { Link, Navigate, useNavigate, useParams } from "react-router"

import { isApiError } from "@/api/errors"
import { useCreateSession, useDevices, useProjects, useSessionList } from "@/api/hooks"
import { queryKeys } from "@/api/query-client"
import type { Device, Location, Project, ProjectLocation, SessionSummary } from "@/api/types"
import { InfoTip } from "@/components/info-tip"
import { PageHeader } from "@/components/page-header"
import { SessionPrompt } from "@/components/session/session-prompt"
import { StatusDot } from "@/components/status-dot"
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
import { locationDirectory, pickLocation, projectErrorMessage } from "@/lib/location-form"
import { formatRelative } from "@/lib/time"
import { projectSettingsPath, sessionPath } from "@/lib/tree"
import type { ImageAttachment } from "@/lib/types"
import { cn } from "@/lib/utils"
import { useConversations } from "@/state/conversations-context"

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
  const { send } = useConversations()
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

  // Set synchronously when a submit starts, so a second Enter (images take
  // a moment to read) can't start another session. Released on failure.
  const inFlight = useRef(false)
  // A session created for this input whose first message hasn't gone out
  // yet; a retry sends into it instead of creating another one.
  const created = useRef<SessionSummary | null>(null)
  const mounted = useRef(true)
  useEffect(() => {
    mounted.current = true
    return () => {
      mounted.current = false
    }
  }, [])

  const start = async (text: string, attachments: ImageAttachment[]) => {
    if (inFlight.current) throw new Error("already starting a session")
    const location = project.locations.at(locationIndex)
    if (locationIndex < 0 || !location) throw new Error("no location to start a session in")
    inFlight.current = true
    setError(null)
    try {
      const reusable =
        created.current?.device === location.device &&
        created.current.cwd === locationDirectory(location)
      const session = reusable && created.current
        ? created.current
        : await createSession.mutateAsync({ projectId: project.projectId, locationIndex })
      created.current = session
      // The mock's send is synchronous and cannot fail. Once it is an RPC,
      // await it here so a failure keeps `created` for the retry.
      send(session.sessionId, text, attachments)
      created.current = null
      // Don't pull the user back if they moved on while this was running.
      if (mounted.current) void navigate(sessionPath(session))
      else inFlight.current = false
    } catch (e) {
      inFlight.current = false
      setError(e)
      // The location is gone on the server: refresh the project's locations.
      if (locationGone(e)) {
        void queryClient.invalidateQueries({ queryKey: queryKeys.projects })
      }
      throw e
    }
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        actions={
          <Button variant="ghost" size="icon-sm" asChild>
            <Link to={projectSettingsPath(project.projectId)} aria-label={t("projects.projectSettings")}>
              <SettingsIcon />
            </Link>
          </Button>
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
              pending={createSession.isPending}
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
              <LocationPicker
                project={project}
                devices={devices.data}
                value={locationIndex}
                disabled={createSession.isPending}
                onChange={(i) => setChosen(project.locations[i])}
              />
            )}
            {error !== null && (
              <p role="alert" className="text-sm text-destructive">
                {locationGone(error) ? t("projects.locationGone") : projectErrorMessage(error)}
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
  isApiError(error) && (error.field === "locationIndex" || error.field === "last_used_location")

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
              <span className="flex w-3 justify-center" title={t(`status.${s.state}`)}>
                <StatusDot state={s.state} />
              </span>
              <span className="min-w-0 flex-1 truncate">{s.title}</span>
              <span className="max-w-28 shrink-0 truncate text-xs text-muted-foreground">{s.device}</span>
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
