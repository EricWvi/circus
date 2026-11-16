import { useState, type FormEvent, type ReactNode } from "react"
import { useNavigate } from "react-router"

import { useCreateProject, useDevices, useProjects } from "@/api/hooks"
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

import { LocationEditor } from "./location-editor"
import { NewProjectContext } from "./new-project-context"
import { useProjectForm } from "./use-project-form"

/** Provides the new-project dialog to everything below it. */
export function NewProjectDialogProvider({ children }: { children: ReactNode }) {
  const [open, setOpen] = useState(false)
  const focus = useOverlayFocus()

  const change = (next: boolean) => {
    focus.onOpenChange(next)
    setOpen(next)
  }

  return (
    <NewProjectContext.Provider value={() => change(true)}>
      {children}
      <Dialog open={open} onOpenChange={change}>
        <DialogContent className="gap-5 sm:max-w-lg" onCloseAutoFocus={focus.onCloseAutoFocus}>
          <NewProjectForm onDone={() => change(false)} />
        </DialogContent>
      </Dialog>
    </NewProjectContext.Provider>
  )
}

// Mounted with the dialog content, so every opening starts empty.
function NewProjectForm({ onDone }: { onDone: () => void }) {
  const navigate = useNavigate()
  const devices = useDevices()
  const projects = useProjects()
  const create = useCreateProject()
  const quietAutoFocus = useQuietAutoFocus()
  const form = useProjectForm({
    initialName: "",
    initialRows: () => [newRow(devices.data ?? [])],
    devices: devices.data,
    otherProjects: projects.data ?? [],
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
    <form onSubmit={submit} noValidate className="flex flex-col gap-5">
      <DialogHeader>
        <DialogTitle>New project</DialogTitle>
      </DialogHeader>
      <Field>
        <FieldLabel htmlFor="new-project-name">Name</FieldLabel>
        <Input
          id="new-project-name"
          {...quietAutoFocus}
          autoComplete="off"
          value={form.name}
          aria-invalid={form.errors.name ? true : undefined}
          onChange={(e) => form.setName(e.target.value)}
          onBlur={(e) => {
            quietAutoFocus.onBlur()
            if (e.target.value !== "") form.touch("name")
          }}
        />
        {form.errors.name && <FieldError>{form.errors.name}</FieldError>}
      </Field>
      <FieldSet className="gap-2">
        <FieldLegend variant="label" className="mb-0">
          Locations
        </FieldLegend>
        <LocationEditor
          rows={form.rows}
          devices={devices.data}
          errors={form.errors.rows}
          disabled={create.isPending}
          onChange={form.setRows}
          onTouch={(key, field) => form.touch(`${key}.${field}`)}
        />
      </FieldSet>
      {form.errors.form && (
        <p role="alert" className="text-sm text-destructive">
          {form.errors.form}
        </p>
      )}
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            Cancel
          </Button>
        </DialogClose>
        <Button type="submit" disabled={create.isPending}>
          {create.isPending && <Spinner aria-label="Creating project" />}
          Create
        </Button>
      </DialogFooter>
    </form>
  )
}
