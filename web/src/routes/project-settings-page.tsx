import { useQueryClient } from "@tanstack/react-query"
import { nanoid } from "nanoid"
import { useEffect, useState, type FormEvent } from "react"
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
import { DisabledReason } from "@/components/disabled-reason"
import { PageHeader } from "@/components/page-header"
import { LocationEditor } from "@/components/projects/location-editor"
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
import {
  formChanged,
  projectErrorMessage,
  sameProjectFields,
  type LocationRow,
} from "@/lib/location-form"
import { deleteBlockedReason } from "@/lib/project-delete"
import { projectPath } from "@/lib/tree"

import { BarePage, LoadError } from "./page-states"

const rowsOf = (project: Project): LocationRow[] =>
  project.locations.map((l) => ({ key: nanoid(), device: l.device, path: l.path }))

export function ProjectSettingsPage() {
  const { t } = useTranslation()
  const { projectId } = useParams()
  const projects = useProjects()

  if (projects.isError && !projects.data) return <LoadError onRetry={() => void projects.refetch()} />
  if (!projects.data) return <BarePage />
  const project = projects.data.find((p) => p.projectId === projectId)
  if (!project) return <Navigate to="/" replace />

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
          />
          <DeleteSection project={project} />
        </div>
      </div>
    </div>
  )
}

function ProjectForm({
  project,
  otherProjects,
}: {
  project: Project
  otherProjects: Project[]
}) {
  const { t } = useTranslation()
  const devices = useDevices()
  const update = useUpdateProject()
  const [savedAt, setSavedAt] = useState(0)
  const form = useProjectForm({
    initialName: project.name,
    initialRows: () => rowsOf(project),
    devices: devices.data,
    otherProjects,
  })

  // The server value the form was last in step with. When the project
  // changes elsewhere, an untouched form follows; edits in progress stay.
  const [base, setBase] = useState(project)
  if (!sameProjectFields(base, project)) {
    if (!formChanged(base, form, devices.data)) form.reset(project.name, rowsOf(project))
    setBase(project)
  }
  const changed = formChanged(project, form, devices.data)

  const submit = (event: FormEvent) => {
    event.preventDefault()
    const req = form.submit()
    if (!req) return
    update.mutate(
      { projectId: project.projectId, ...req },
      {
        onSuccess: (next) => {
          setBase(next)
          form.reset(next.name, rowsOf(next))
          setSavedAt(Date.now())
        },
        onError: form.serverError,
      },
    )
  }

  return (
    <form onSubmit={submit} noValidate className="flex flex-col gap-10">
      <section className="flex flex-col gap-4">
        <h2 className="text-sm font-medium">{t("projects.general")}</h2>
        <Field className="max-w-sm">
          <FieldLabel htmlFor="project-name">{t("common.name")}</FieldLabel>
          <Input
            id="project-name"
            autoComplete="off"
            value={form.name}
            disabled={update.isPending}
            aria-invalid={form.errors.name ? true : undefined}
            onChange={(e) => form.setName(e.target.value)}
            onBlur={() => form.touch("name")}
          />
          {form.errors.name && <FieldError>{form.errors.name}</FieldError>}
        </Field>
      </section>

      <section className="flex flex-col gap-4">
        <h2 className="text-sm font-medium">{t("projects.locations")}</h2>
        <LocationEditor
          rows={form.rows}
          devices={devices.data}
          errors={form.errors.rows}
          disabled={update.isPending}
          onChange={form.setRows}
          onTouch={(key, field) => form.touch(`${key}.${field}`)}
        />
      </section>

      <div className="flex flex-col gap-3">
        {form.errors.form && (
          <p role="alert" className="text-sm text-destructive">
            {form.errors.form}
          </p>
        )}
        <div className="flex items-center gap-3">
          <Button type="submit" disabled={!changed || update.isPending}>
            {update.isPending && <Spinner aria-label={t("common.saving")} />}
            {t("common.save")}
          </Button>
          <SavedNote at={changed ? 0 : savedAt} />
        </div>
      </div>
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
