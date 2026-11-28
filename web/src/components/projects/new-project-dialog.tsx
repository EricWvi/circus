import { useRef, useState, type FormEvent, type ReactNode } from "react"
import { useTranslation } from "react-i18next"
import { useNavigate } from "react-router"

import { useCreateProject, useDevices, useProjects } from "@/api/hooks"
import { DEFAULT_PATH_PREFIX } from "@/api/validation"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Field, FieldError, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { useQuietAutoFocus } from "@/hooks/use-quiet-autofocus"
import { defaultDevice, newRow } from "@/lib/location-form"

import { busyClass } from "./busy"
import { LocationEditor } from "./location-editor"
import { NewProjectContext } from "./new-project-context"
import { PickerFrameContext } from "./picker-frame"
import { RepoUrlField } from "./repo-url-field"
import { useFolderRemote } from "./use-folder-remote"
import { useProjectForm } from "./use-project-form"

/** Provides the new-project dialog to everything below it. */
export function NewProjectDialogProvider({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false)
  // Each opening gets a form of its own, even one that comes while the
  // last one is still animating out after Create.
  const [opening, setOpening] = useState(0)
  const focus = useOverlayFocus()
  // Folder pickers center on the dialog rather than on their path field.
  const panel = useRef<HTMLDivElement>(null)

  const change = (next: boolean) => {
    focus.onOpenChange(next)
    if (next) setOpening((n) => n + 1)
    setOpen(next)
  }

  return (
    <NewProjectContext.Provider value={() => change(true)}>
      {children}
      <Dialog open={open} onOpenChange={change}>
        <DialogContent ref={panel} className="gap-5 sm:max-w-xl" onCloseAutoFocus={focus.onCloseAutoFocus}>
          <PickerFrameContext.Provider value={panel}>
            <NewProjectForm key={opening} onDone={() => change(false)} />
          </PickerFrameContext.Provider>
        </DialogContent>
      </Dialog>
    </NewProjectContext.Provider>
  )
}

// Keyed by opening, so every opening starts empty.
function NewProjectForm({ onDone }: { onDone: () => void }) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const devices = useDevices()
  const projects = useProjects()
  const create = useCreateProject()
  const quietAutoFocus = useQuietAutoFocus()
  // The projects as they were before Create: once created, the new project
  // joins the list while the dialog closes, and must not show its name as
  // taken or its locations as shared.
  const [others, setOthers] = useState(projects.data)
  if (!create.isPending && !create.isSuccess && others !== projects.data) setOthers(projects.data)
  const form = useProjectForm({
    initial: () => ({ name: "", repoUrl: "", rows: [newRow(devices.data ?? [])] }),
    devices: devices.data,
    otherProjects: others ?? [],
    deriveName: true,
  })
  const remote = useFolderRemote({
    rows: form.rows,
    devices: devices.data,
    repoUrl: form.repoUrl,
    setRepoUrl: form.setRepoUrl,
    busy: create.isPending,
    repoUrlId: "new-project-repo-url",
  })

  // Rows created before the devices loaded get the default device now,
  // without touching anything already typed.
  const [defaulted, setDefaulted] = useState(devices.data !== undefined)
  if (!defaulted && devices.data) {
    setDefaulted(true)
    form.fillDevice(defaultDevice(devices.data))
  }

  const submit = (event: FormEvent) => {
    event.preventDefault()
    remote.submitStarted()
    if (create.isPending) return
    const req = form.submit()
    if (!req) return
    create.mutate(req, {
      onSuccess: (project) => {
        onDone()
        void navigate(`/p/${encodeURIComponent(project.projectId)}`)
      },
      onError: form.serverError,
    })
  }

  return (
    <form
      onSubmit={submit}
      onPointerDownCapture={remote.formPointerDown}
      noValidate
      className="flex flex-col gap-5"
    >
      <DialogHeader>
        <DialogTitle>{t("projects.newProject")}</DialogTitle>
      </DialogHeader>
      <FieldSet className="gap-2">
        <FieldLegend variant="label" className="mb-2">
          {t("projects.locations")}
        </FieldLegend>
        <LocationEditor
          rows={form.rows}
          devices={devices.data}
          errors={form.errors.rows}
          busy={create.isPending}
          defaultPath={DEFAULT_PATH_PREFIX + "…"}
          defaultPathHint={t("projects.defaultPathHint", {
            path: DEFAULT_PATH_PREFIX + t("projects.projectIdPlaceholder"),
          })}
          onChange={form.setRows}
          onTouch={(key, field) => form.touch(`${key}.${field}`)}
          onPathCommit={remote.pathCommitted}
          otherProjects={others}
          firstPathFocus={quietAutoFocus}
        />
      </FieldSet>
      <RepoUrlField
        id="new-project-repo-url"
        value={form.repoUrl}
        error={form.errors.repoUrl}
        busy={create.isPending}
        suggestion={remote.suggestion}
        onChange={remote.editRepoUrl}
        onBlur={() => {
          if (form.repoUrl !== "") form.touch("repoUrl")
        }}
        onUseSuggestion={remote.applySuggested}
      />
      <Field>
        <FieldLabel htmlFor="new-project-name">{t("common.name")}</FieldLabel>
        <Input
          id="new-project-name"
          autoComplete="off"
          value={form.name}
          aria-invalid={form.errors.name ? true : undefined}
          onChange={(e) => form.setName(e.target.value)}
          onFocus={form.nameEntered}
          onBlur={form.nameLeft}
        />
        {form.errors.name && <FieldError>{form.errors.name}</FieldError>}
      </Field>
      {form.errors.form && (
        <p role="alert" className="text-sm text-destructive">
          {form.errors.form}
        </p>
      )}
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t("common.cancel")}
          </Button>
        </DialogClose>
        <Button type="submit" aria-disabled={create.isPending || undefined} className={busyClass}>
          {create.isPending && <Spinner aria-label={t("projects.creating")} />}
          {t("common.create")}
        </Button>
      </DialogFooter>
    </form>
  )
}
