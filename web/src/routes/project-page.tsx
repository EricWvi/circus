import { useQueryClient } from "@tanstack/react-query"
import { FolderIcon, SettingsIcon } from "lucide-react"
import { useEffect, useRef, useState } from "react"
import { useTranslation } from "react-i18next"
import { Link, Navigate, useNavigate, useParams } from "react-router"

import { isApiError } from "@/api/errors"
import { useCreateSession, useDevices, useProjects, useSessionList } from "@/api/hooks"
import { queryKeys } from "@/api/query-client"
import type { Device, Location, Project, SessionSummary } from "@/api/types"
import { PageHeader } from "@/components/page-header"
import { SessionPrompt } from "@/components/session/session-prompt"
import { StatusDot } from "@/components/status-dot"
import { Button } from "@/components/ui/button"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { pickLocation, projectErrorMessage } from "@/lib/location-form"
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
  const [chosen, setChosen] = useState<Location | null>(null)
  const locationIndex = pickLocation(project, chosen)
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
    inFlight.current = true
    setError(null)
    try {
      const location = project.locations[locationIndex]
      const reusable =
        created.current?.device === location.device && created.current.cwd === location.path
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
      if (isApiError(e) && e.field === "locationIndex") {
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
        <h1 className="min-w-0 truncate font-medium">{project.name}</h1>
      </PageHeader>

      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-[760px] flex-col gap-8 px-4 py-8">
          <section className="flex flex-col gap-2" aria-label={t("projects.newSession")}>
            <SessionPrompt
              placeholder={t("projects.startPlaceholder")}
              pending={createSession.isPending}
              onSend={start}
            />
            <LocationPicker
              project={project}
              devices={devices.data}
              value={locationIndex}
              disabled={createSession.isPending}
              onChange={(i) => setChosen(project.locations[i])}
            />
            {error !== null && (
              <p role="alert" className="text-sm text-destructive">
                {isApiError(error) && error.field === "locationIndex"
                  ? t("projects.locationGone")
                  : projectErrorMessage(error)}
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

const locationLabel = (l: { device: string; path: string }) => `${l.device}:${l.path}`

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

  const content = (l: { device: string; path: string }) => (
    <span className="flex min-w-0 items-center gap-2">
      <FolderIcon className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
      <span className="truncate font-mono">{locationLabel(l)}</span>
      {offline(l.device) && <span className="shrink-0 text-muted-foreground">{t("common.offline")}</span>}
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
          <SelectItem key={locationLabel(l)} value={String(i)} className="text-xs">
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
