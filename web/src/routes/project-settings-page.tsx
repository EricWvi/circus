import { useQueryClient } from "@tanstack/react-query"
import { MonitorOffIcon } from "lucide-react"
import { useEffect, useRef, useState, type FormEvent } from "react"
import { useTranslation } from "react-i18next"
import { Link, Navigate, useNavigate, useParams } from "react-router"

import { isApiError } from "@/api/errors"
import {
  useDeleteProject,
  useDevices,
  useLedger,
  useModelProviders,
  useProjects,
  useSessionList,
  useUpdateProject,
} from "@/api/hooks"
import { queryKeys } from "@/api/query-client"
import type { ModelProvider, Project, WorkspaceRemoval } from "@/api/types"
import { defaultPath } from "@/api/validation"
import { DisabledReason } from "@/components/disabled-reason"
import { useShowNotice, type NoticeLine } from "@/components/notice-context"
import { PageHeader } from "@/components/page-header"
import { busyClass } from "@/components/projects/busy"
import { LocationEditor } from "@/components/projects/location-editor"
import { RepoUrlField } from "@/components/projects/repo-url-field"
import { useFolderRemote } from "@/components/projects/use-folder-remote"
import { useProjectForm } from "@/components/projects/use-project-form"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectSeparator, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { i18n } from "@/i18n/i18n"
import { lastInputWasKeyboard } from "@/lib/input-modality"
import {
  formChanged,
  locationsChanged,
  projectErrorMessage,
  rowsOf,
  type FormValues,
} from "@/lib/location-form"
import { deleteBlockedReason } from "@/lib/project-delete"
import { NO_PROVIDER } from "@/lib/provider-choice"
import { projectPath } from "@/lib/tree"
import { showsDetail, workspaceReason } from "@/lib/workspace"

import { BarePage, LoadError } from "./page-states"

const valuesOf = (project: Project): FormValues => ({
  name: project.name,
  repoUrl: project.repoUrl,
  rows: rowsOf(project),
})

/** The default folders the project has on its devices, by device name. */
const workspacesOf = (project: Project) =>
  new Map(project.locations.flatMap((l) => (l.path === "" && l.workspace ? [[l.device, l.workspace] as const] : [])))

/** The directory default locations resolve to, as the server says when it has. */
const defaultDirectory = (project: Project) =>
  project.locations.find((l) => l.path === "")?.effectivePath || defaultPath(project.projectId)

export function ProjectSettingsPage() {
  const { t } = useTranslation()
  const { projectId } = useParams()
  const projects = useProjects()

  if (projects.isError && !projects.data) return <LoadError onRetry={() => void projects.refetch()} />
  if (!projects.data) return <BarePage />
  const project = projects.data.find((p) => p.projectId === projectId)
  if (!project) return <Navigate to="/" replace />

  // The project as the server has it now; undefined once it is gone.
  const reload = async () => {
    const res = await projects.refetch({ throwOnError: true })
    return res.data?.find((p) => p.projectId === project.projectId)
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader>
        <Link
          to={projectPath(project.projectId)}
          className="min-w-0 truncate rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
        >
          {project.name}
        </Link>
        <span className="text-muted-foreground" aria-hidden>
          /
        </span>
        <h1 className="shrink-0 font-medium">{t("projects.settings")}</h1>
      </PageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-[760px] flex-col gap-10 px-4 py-8">
          <ProjectForm
            key={project.projectId}
            project={project}
            otherProjects={projects.data.filter((p) => p.projectId !== project.projectId)}
            reload={reload}
          />
          <DeleteSection project={project} />
        </div>
      </div>
    </div>
  )
}

export function ProjectForm({
  project,
  otherProjects,
  reload,
}: {
  project: Project
  otherProjects: Project[]
  /** Fetches the project again; resolves to undefined if it is gone. */
  reload: () => Promise<Project | undefined>
}) {
  const { t } = useTranslation()
  const devices = useDevices()
  const update = useUpdateProject()
  const [savedAt, setSavedAt] = useState(0)
  // The project the form was loaded from. A save expects the server to
  // still have this version, so edits made on an older version never
  // overwrite a newer one.
  const [base, setBase] = useState(project)
  const form = useProjectForm({
    initial: () => valuesOf(project),
    devices: devices.data,
    otherProjects,
    savedLocations: base.locations,
  })
  // The default provider: "" for none. Kept beside the form's other values.
  const providers = useModelProviders()
  const [providerId, setProviderId] = useState(project.defaultProviderId)
  // A save refused because of the provider (gone since), shown until it changes.
  const [providerError, setProviderError] = useState(false)
  const chooseProvider = (id: string) => {
    setProviderError(false)
    setProviderId(id)
  }
  // A save was refused because the project changed elsewhere.
  const [stale, setStale] = useState(false)
  const [confirmOpen, setConfirmOpen] = useState(false)
  const confirmFocus = useOverlayFocus()
  const reloadButton = useRef<HTMLButtonElement>(null)
  const [reloading, setReloading] = useState(false)
  const [reloadError, setReloadError] = useState<unknown>(null)
  const nameInput = useRef<HTMLInputElement>(null)
  // Set by a reload: the control that had focus (the Reload button) may be
  // gone, so focus goes to the name if nothing else has it.
  const [reloaded, setReloaded] = useState(0)
  useEffect(() => {
    if (reloaded === 0) return
    const active = document.activeElement
    if (active === null || active === document.body) {
      nameInput.current?.focus({ focusVisible: lastInputWasKeyboard() } as FocusOptions)
    }
  }, [reloaded])

  const busy = update.isPending || reloading
  // The saved URL counts as typed: never overwritten, only hinted at.
  const remote = useFolderRemote({
    rows: form.rows,
    devices: devices.data,
    repoUrl: form.repoUrl,
    setRepoUrl: form.setRepoUrl,
    busy,
    repoUrlId: "project-repo-url",
  })

  // When the project changes elsewhere, an untouched form follows, and so
  // does one that already holds the new values; edits in progress stay on
  // the version they started from.
  if (project.updatedAtUnixMs !== base.updatedAtUnixMs) {
    const untouched = !formChanged(base, form.values, devices.data) && providerId === base.defaultProviderId
    const caughtUp = !formChanged(project, form.values, devices.data) && providerId === project.defaultProviderId
    if (untouched || caughtUp) {
      if (untouched) {
        form.reset(valuesOf(project))
        remote.reset()
        setProviderId(project.defaultProviderId)
      }
      setBase(project)
      setStale(false)
    }
  }
  const providerChanged = providerId !== base.defaultProviderId
  const dirty = formChanged(base, form.values, devices.data) || providerChanged

  const submit = (event: FormEvent) => {
    event.preventDefault()
    remote.submitStarted()
    // Save stays focusable while it can't be used, so Enter can land here.
    if (!dirty || busy || stale) return
    const req = form.submit()
    if (!req) return
    const sendLocations = locationsChanged(base.locations, form.rows, devices.data)
    update.mutate(
      {
        projectId: project.projectId,
        name: req.name,
        repoUrl: req.repoUrl,
        ...(sendLocations ? { locations: req.locations } : {}),
        ...(providerChanged ? { defaultProviderId: providerId } : {}),
        expectedUpdatedAt: base.updatedAtUnixMs,
      },
      {
        onSuccess: (next) => {
          setBase(next)
          form.reset(valuesOf(next))
          remote.reset()
          setProviderId(next.defaultProviderId)
          setSavedAt(Date.now())
        },
        onError: (error) => {
          if (isApiError(error, "aborted")) setStale(true)
          else if (isApiError(error) && error.field === "default_provider_id") {
            setProviderError(true)
            void providers.refetch()
          } else form.serverError(error)
        },
      },
    )
  }

  /** Refetches the project and starts the form over from it. */
  const reloadNow = async () => {
    if (reloading) return
    setReloading(true)
    setReloadError(null)
    try {
      const fresh = await reload()
      // A project that is gone takes the page away.
      if (fresh) {
        setBase(fresh)
        form.reset(valuesOf(fresh))
        remote.reset()
        chooseProvider(fresh.defaultProviderId)
      }
      setStale(false)
    } catch (error) {
      setReloadError(error)
    } finally {
      setReloading(false)
      setReloaded((n) => n + 1)
    }
  }

  const changeConfirm = (open: boolean) => {
    confirmFocus.onOpenChange(open)
    setConfirmOpen(open)
  }

  const directory = defaultDirectory(project)

  return (
    <form
      onSubmit={submit}
      onPointerDownCapture={remote.formPointerDown}
      noValidate
      className="flex flex-col gap-10"
    >
      <section className="flex flex-col gap-4">
        <h2 className="text-sm font-medium">{t("projects.general")}</h2>
        <Field className="max-w-sm">
          <FieldLabel htmlFor="project-name">{t("common.name")}</FieldLabel>
          <Input
            id="project-name"
            ref={nameInput}
            autoComplete="off"
            value={form.name}
            readOnly={busy}
            aria-busy={busy || undefined}
            aria-invalid={form.errors.name ? true : undefined}
            onChange={(e) => form.setName(e.target.value)}
            onBlur={() => form.touch("name")}
          />
          {form.errors.name && <FieldError>{form.errors.name}</FieldError>}
        </Field>
        <RepoUrlField
          id="project-repo-url"
          className="max-w-lg"
          value={form.repoUrl}
          error={form.errors.repoUrl}
          busy={busy}
          suggestion={remote.suggestion}
          onChange={remote.editRepoUrl}
          onBlur={() => form.touch("repoUrl")}
          onUseSuggestion={remote.applySuggested}
        />
        <DefaultProviderField
          value={providerId}
          providers={providers.data}
          busy={busy}
          error={providerError ? t("validation.providerGone") : undefined}
          onChange={chooseProvider}
        />
      </section>

      <section className="flex flex-col gap-4">
        <h2 className="text-sm font-medium">{t("projects.locations")}</h2>
        <LocationEditor
          rows={form.rows}
          devices={devices.data}
          errors={form.errors.rows}
          busy={busy}
          defaultPath={directory}
          defaultPathHint={t("projects.defaultPathHint", { path: directory })}
          onChange={form.setRows}
          onTouch={(key, field) => form.touch(`${key}.${field}`)}
          onPathCommit={remote.pathCommitted}
          workspaces={workspacesOf(project)}
          otherProjects={otherProjects}
        />
      </section>

      <div className="flex flex-col gap-3">
        {stale && (
          <div role="alert" className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm">
            <span className="text-destructive">{t("projects.changedElsewhere")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              ref={reloadButton}
              aria-disabled={reloading || undefined}
              className={busyClass}
              onClick={() => {
                if (reloading) return
                if (dirty) changeConfirm(true)
                else void reloadNow()
              }}
            >
              {reloading && <Spinner aria-label={t("common.loading")} />}
              {t("projects.reload")}
            </Button>
            {reloadError !== null && (
              <span className="text-destructive">{projectErrorMessage(reloadError)}</span>
            )}
          </div>
        )}
        {form.errors.form && (
          <p role="alert" className="text-sm text-destructive">
            {form.errors.form}
          </p>
        )}
        <div className="flex items-center gap-3">
          <DisabledReason reason={stale ? t("projects.changedElsewhere") : null}>
            {/* Unavailable while unchanged or saving, but still focusable, so
                focus stays put after a save; a refused save disables it for real. */}
            <Button
              type="submit"
              disabled={stale}
              aria-disabled={!dirty || busy || undefined}
              className={busyClass}
            >
              {update.isPending && <Spinner aria-label={t("common.saving")} />}
              {t("common.save")}
            </Button>
          </DisabledReason>
          <SavedNote at={dirty ? 0 : savedAt} />
        </div>
      </div>

      <Dialog open={confirmOpen} onOpenChange={changeConfirm}>
        <DialogContent
          showCloseButton={false}
          onCloseAutoFocus={(event) => {
            if (reloadButton.current?.isConnected) {
              confirmFocus.onCloseAutoFocus(event)
              return
            }
            // The reload finished while the dialog was closing and took the
            // Reload button with it.
            event.preventDefault()
            nameInput.current?.focus({ focusVisible: lastInputWasKeyboard() } as FocusOptions)
          }}
        >
          <DialogHeader>
            <DialogTitle>{t("projects.discardTitle")}</DialogTitle>
            <DialogDescription>{t("projects.discardDescription")}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                {t("common.cancel")}
              </Button>
            </DialogClose>
            <Button
              type="button"
              variant="destructive"
              onClick={() => {
                changeConfirm(false)
                void reloadNow()
              }}
            >
              {t("projects.discardAndReload")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </form>
  )
}

/** The project's default provider: none, or one of the user's providers. */
function DefaultProviderField({
  value,
  providers,
  busy,
  error,
  onChange,
}: {
  /** "" for none. */
  value: string
  providers: ModelProvider[] | undefined
  busy: boolean
  error: string | undefined
  onChange: (providerId: string) => void
}) {
  const { t } = useTranslation()
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  // Unknown until the list loads; then a provider it doesn't hold was deleted.
  const deleted = value !== "" && providers !== undefined && !providers.some((p) => p.providerId === value)
  const name = (id: string) =>
    id === ""
      ? t("projects.defaultProviderNone")
      : (providers?.find((p) => p.providerId === id)?.name ??
        (providers ? t("projects.defaultProviderDeleted") : ""))
  return (
    <Field className="max-w-sm">
      <FieldLabel htmlFor="project-default-provider">{t("projects.defaultProvider")}</FieldLabel>
      <Select
        value={value === "" ? NO_PROVIDER : value}
        disabled={busy || providers === undefined}
        onValueChange={(v) => onChange(v === NO_PROVIDER ? "" : v)}
        onOpenChange={onOpenChange}
      >
        <SelectTrigger
          id="project-default-provider"
          ref={triggerRef}
          className="w-full"
          aria-invalid={error || deleted ? true : undefined}
          aria-describedby="project-default-provider-hint"
        >
          <SelectValue>
            <span className={deleted ? "text-destructive" : undefined}>{name(value)}</span>
          </SelectValue>
        </SelectTrigger>
        <SelectContent position="popper" onCloseAutoFocus={onCloseAutoFocus}>
          <SelectItem value={NO_PROVIDER}>{t("projects.defaultProviderNone")}</SelectItem>
          {(providers?.length ?? 0) > 0 && <SelectSeparator />}
          {deleted && (
            <SelectItem value={value} disabled>
              {t("projects.defaultProviderDeleted")}
            </SelectItem>
          )}
          {providers?.map((p) => (
            <SelectItem key={p.providerId} value={p.providerId}>
              {p.name}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {error ? (
        <FieldError>{error}</FieldError>
      ) : (
        <FieldDescription id="project-default-provider-hint" className="text-xs">
          {value === "" ? t("projects.defaultProviderNoneHint") : t("projects.defaultProviderHint")}
        </FieldDescription>
      )}
    </Field>
  )
}

const SAVED_VISIBLE_MS = 2000
const SAVED_FADE_MS = 500

/** "Saved" next to the button for a moment, then fades and is removed. */
function SavedNote({ at }: { at: number }) {
  const { t } = useTranslation()
  const [phase, setPhase] = useState<{ at: number; step: "fading" | "gone" } | null>(null)
  useEffect(() => {
    if (at === 0) return
    const fade = window.setTimeout(() => setPhase({ at, step: "fading" }), SAVED_VISIBLE_MS)
    const gone = window.setTimeout(
      () => setPhase({ at, step: "gone" }),
      SAVED_VISIBLE_MS + SAVED_FADE_MS,
    )
    return () => {
      window.clearTimeout(fade)
      window.clearTimeout(gone)
    }
  }, [at])
  const step = at === 0 ? "gone" : phase?.at === at ? phase.step : "visible"
  return (
    <span
      role="status"
      className={`text-sm text-muted-foreground transition-opacity duration-500 ${step === "visible" ? "opacity-100" : "opacity-0"}`}
    >
      {step !== "gone" && t("common.saved")}
    </span>
  )
}

/** What happened to a folder the deletion was asked to remove, when it wasn't removed. */
function removalLine(projectId: string, r: WorkspaceRemoval): NoticeLine | null {
  const path = r.path || defaultPath(projectId)
  if (r.outcome === "kept") return { text: i18n.t("projects.folderKept", { path, device: r.device }) }
  if (r.outcome !== "failed") return null
  const reason = workspaceReason(r.error, { device: r.device, detail: r.errorDetail })
  return {
    text: i18n.t("projects.folderNotDeleted", { path, device: r.device, reason }),
    ...(showsDetail(r) ? { detail: r.errorDetail } : {}),
  }
}

function DeleteSection({ project }: { project: Project }) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const sessions = useSessionList(project.projectId)
  const ledger = useLedger(project.projectId)
  const decisions = ledger.data?.entries.length ?? 0
  const remove = useDeleteProject()
  const showNotice = useShowNotice()
  const [open, setOpen] = useState(false)
  // Devices whose folder is to be deleted too; none until ticked.
  const [ticked, setTicked] = useState<ReadonlySet<string>>(new Set())
  const devices = useDevices({ live: open })
  const focus = useOverlayFocus()
  const blocked = sessions.data ? deleteBlockedReason(sessions.data) : t("projects.deleteBlockedLoading")

  // Ready default folders: deletable with the project while their device
  // is online; otherwise they stay on it, which the dialog says. A folder
  // that isn't ready was perhaps never created, so nothing is said of it.
  const online = (device: string) => devices.data?.find((d) => d.name === device)?.online
  const folders = project.locations.flatMap((l) =>
    l.path === "" && l.workspace?.state === "ready" && !l.deviceRevoked
      ? [{ device: l.device, workspace: l.workspace }]
      : [],
  )
  const removable = folders.filter((f) => online(f.device) === true)
  const staying = folders.filter((f) => online(f.device) === false)

  // A device that goes offline while the dialog is open leaves its tick
  // behind (it isn't sent, and isn't ticked again should it come back);
  // its line says the folder stays instead.
  const removableKey = removable.map((f) => f.device).join("\n")
  const [shownKey, setShownKey] = useState(removableKey)
  if (shownKey !== removableKey) {
    setShownKey(removableKey)
    const still = new Set(removable.map((f) => f.device))
    if ([...ticked].some((d) => !still.has(d))) setTicked(new Set([...ticked].filter((d) => still.has(d))))
  }
  // If its checkbox had focus, focus goes to the next one, or Delete
  // (checked after every render: it is about elements that just went).
  const checkboxes = useRef(new Map<string, HTMLButtonElement>())
  const deleteButton = useRef<HTMLButtonElement>(null)
  const focused = useRef<string | null>(null)
  useEffect(() => {
    const lost = focused.current
    if (lost === null || checkboxes.current.has(lost)) return
    focused.current = null
    const active = document.activeElement
    if (active && active !== document.body && !active.matches('[role="dialog"]')) return
    const next = [...checkboxes.current.values()][0] ?? deleteButton.current
    next?.focus({ focusVisible: lastInputWasKeyboard() } as FocusOptions)
  })

  const change = (next: boolean) => {
    focus.onOpenChange(next)
    if (next) {
      remove.reset()
      setTicked(new Set())
    }
    setOpen(next)
  }

  const tick = (device: string, on: boolean) =>
    setTicked((set) => {
      const next = new Set(set)
      if (on) next.add(device)
      else next.delete(device)
      return next
    })

  const confirm = () =>
    remove.mutate(
      {
        projectId: project.projectId,
        removeWorkspacesOn: removable.map((f) => f.device).filter((d) => ticked.has(d)),
      },
      {
        onSuccess: ({ workspaceRemovals }) => {
          if (workspaceRemovals.some((r) => removalLine(project.projectId, r) !== null)) {
            const name = project.name
            showNotice({
              // Made when shown, in the language of the moment.
              content: () => ({
                title: i18n.t("projects.deleted", { name }),
                lines: workspaceRemovals.flatMap((r) => removalLine(project.projectId, r) ?? []),
              }),
              failure: true,
            })
          }
          void navigate("/", { replace: true })
        },
        // Raced with a new session: refresh the list so the button and its
        // reason catch up.
        onError: (e) => {
          if (isApiError(e, "failed_precondition")) {
            void queryClient.invalidateQueries({ queryKey: queryKeys.sessions(project.projectId) })
          }
        },
      },
    )

  const error =
    remove.error &&
    (isApiError(remove.error, "failed_precondition")
      ? t("projects.deleteRaced")
      : projectErrorMessage(remove.error))

  return (
    <section className="flex flex-col items-start gap-3 border-t pt-8">
      <DisabledReason reason={blocked}>
        <Button
          type="button"
          variant="outline"
          className="border-destructive/40 text-destructive hover:bg-destructive/10 hover:text-destructive dark:border-destructive/40 dark:hover:bg-destructive/15"
          disabled={blocked !== null}
          onClick={() => change(true)}
        >
          {t("projects.deleteProject")}
        </Button>
      </DisabledReason>
      <Dialog open={open} onOpenChange={change}>
        <DialogContent showCloseButton={false} onCloseAutoFocus={focus.onCloseAutoFocus}>
          <DialogHeader>
            <DialogTitle>{t("projects.deleteTitle", { name: project.name })}</DialogTitle>
            <DialogDescription>
              {decisions > 0
                ? t("projects.deleteDescriptionLedger", { count: decisions })
                : t("projects.deleteDescription")}
            </DialogDescription>
          </DialogHeader>
          {folders.length > 0 && devices.data === undefined && (
            // Which folders can go depends on which devices are online.
            <p role="status" className="flex items-center gap-2.5 text-muted-foreground">
              {devices.isError ? (
                t("projects.devicesUnknown")
              ) : (
                <>
                  <Spinner aria-hidden />
                  {t("projects.checkingDevices")}
                </>
              )}
            </p>
          )}
          {(removable.length > 0 || staying.length > 0) && (
            <div className="flex flex-col gap-2.5">
              {removable.map((f) => (
                <label key={f.device} className="flex cursor-default items-start gap-2.5">
                  <Checkbox
                    ref={(el) => {
                      if (el) checkboxes.current.set(f.device, el)
                      else checkboxes.current.delete(f.device)
                    }}
                    onFocus={() => (focused.current = f.device)}
                    className="mt-0.5"
                    checked={ticked.has(f.device)}
                    disabled={remove.isPending}
                    onCheckedChange={(checked) => tick(f.device, checked === true)}
                  />
                  <span className="min-w-0 [overflow-wrap:anywhere]">
                    {t("projects.alsoDeleteFolder", { path: f.workspace.path, device: f.device })}
                  </span>
                </label>
              ))}
              {staying.map((f) => (
                <p key={f.device} className="flex items-start gap-2.5 text-muted-foreground">
                  <MonitorOffIcon className="mt-0.5 size-4 shrink-0" aria-hidden />
                  <span className="min-w-0 [overflow-wrap:anywhere]">
                    {t("projects.folderStaysOffline", { path: f.workspace.path, device: f.device })}
                  </span>
                </p>
              ))}
            </div>
          )}
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline">{t("common.cancel")}</Button>
            </DialogClose>
            <Button ref={deleteButton} variant="destructive" disabled={remove.isPending} onClick={confirm}>
              {remove.isPending && <Spinner aria-label={t("projects.deleting")} />}
              {t("common.delete")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  )
}
