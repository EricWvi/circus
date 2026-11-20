import { useQueryClient } from "@tanstack/react-query"
import { useEffect, useRef, useState, type FormEvent } from "react"
import { useTranslation } from "react-i18next"
import { Link, Navigate, useNavigate, useParams } from "react-router"

import { isApiError } from "@/api/errors"
import {
  useDeleteProject,
  useDevices,
  useProjects,
  useSessionList,
  useUpdateProject,
} from "@/api/hooks"
import { queryKeys } from "@/api/query-client"
import type { Project } from "@/api/types"
import { defaultPath } from "@/api/validation"
import { DisabledReason } from "@/components/disabled-reason"
import { PageHeader } from "@/components/page-header"
import { busyClass } from "@/components/projects/busy"
import { LocationEditor } from "@/components/projects/location-editor"
import { RepoUrlField } from "@/components/projects/repo-url-field"
import { useFolderRemote } from "@/components/projects/use-folder-remote"
import { useProjectForm } from "@/components/projects/use-project-form"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { lastInputWasKeyboard } from "@/lib/input-modality"
import {
  formChanged,
  locationsChanged,
  projectErrorMessage,
  rowsOf,
  type FormValues,
} from "@/lib/location-form"
import { deleteBlockedReason } from "@/lib/project-delete"
import { projectPath } from "@/lib/tree"

import { BarePage, LoadError } from "./page-states"

const valuesOf = (project: Project): FormValues => ({
  name: project.name,
  repoUrl: project.repoUrl,
  rows: rowsOf(project),
})

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
    const untouched = !formChanged(base, form.values, devices.data)
    if (untouched || !formChanged(project, form.values, devices.data)) {
      if (untouched) {
        form.reset(valuesOf(project))
        remote.reset()
      }
      setBase(project)
      setStale(false)
    }
  }
  const dirty = formChanged(base, form.values, devices.data)

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
        expectedUpdatedAt: base.updatedAtUnixMs,
      },
      {
        onSuccess: (next) => {
          setBase(next)
          form.reset(valuesOf(next))
          remote.reset()
          setSavedAt(Date.now())
        },
        onError: (error) => {
          if (isApiError(error, "aborted")) setStale(true)
          else form.serverError(error)
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

function DeleteSection({ project }: { project: Project }) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const sessions = useSessionList(project.projectId)
  const remove = useDeleteProject()
  const [open, setOpen] = useState(false)
  const focus = useOverlayFocus()
  const blocked = sessions.data ? deleteBlockedReason(sessions.data) : t("projects.deleteBlockedLoading")

  const change = (next: boolean) => {
    focus.onOpenChange(next)
    if (next) remove.reset()
    setOpen(next)
  }

  const confirm = () =>
    remove.mutate(
      { projectId: project.projectId },
      {
        onSuccess: () => void navigate("/", { replace: true }),
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
            <DialogDescription>{t("projects.deleteDescription")}</DialogDescription>
          </DialogHeader>
          {error && (
            <p role="alert" className="text-sm text-destructive">
              {error}
            </p>
          )}
          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline">{t("common.cancel")}</Button>
            </DialogClose>
            <Button variant="destructive" disabled={remove.isPending} onClick={confirm}>
              {remove.isPending && <Spinner aria-label={t("projects.deleting")} />}
              {t("common.delete")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  )
}
