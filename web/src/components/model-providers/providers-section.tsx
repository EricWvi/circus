import { KeyRoundIcon, PlusIcon, Trash2Icon, TriangleAlertIcon } from "lucide-react"
import { memo, useImperativeHandle, useRef, useState, type Ref } from "react"
import { useTranslation } from "react-i18next"

import { isApiError } from "@/api/errors"
import { useDeleteModelProvider, useModelProviders, useWorkers } from "@/api/hooks"
import type { ModelProvider, Worker } from "@/api/types"
import { ProviderForm } from "@/components/model-providers/provider-dialog"
import { DeviceStatusDot } from "@/components/status-dot"
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
import { Empty, EmptyContent, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { useReturnFocus } from "@/hooks/use-return-focus"
import { maskedSecret, providerErrorMessage } from "@/lib/model-provider-form"

/** What the provider dialog shows: a new provider, or one to edit. */
type Editing = { kind: "new" } | { kind: "edit"; provider: ModelProvider }

/** What the page asks of the section: the new-provider form, opened from `trigger` (where focus returns). */
export interface ProvidersSectionHandle {
  addProvider: (trigger: HTMLElement | null) => void
}

/**
 * The signed-in user's model providers, a section of the devices page:
 * list, edit, delete, and add (from the page's Add menu through `ref`, or
 * the empty state). Memoized, with stable props: the page's renders (each
 * change of the devices) leave it, and its dialogs, alone. `onAddWorker`
 * opens the page's add dialog for a worker, from `trigger` (where focus
 * returns); `fallbackFocus` is where focus goes once a dialog's opener is
 * gone and the empty state isn't there either.
 */
export const ProvidersSection = memo(function ProvidersSection({
  ref,
  onAddWorker,
  fallbackFocus,
}: {
  ref?: Ref<ProvidersSectionHandle>
  onAddWorker: (trigger: HTMLElement | null) => void
  fallbackFocus: () => HTMLElement | null
}) {
  const { t } = useTranslation()
  const providers = useModelProviders({ live: true })
  const workers = useWorkers()
  const [editing, setEditing] = useState<Editing | null>(null)
  const [deleting, setDeleting] = useState<ModelProvider | null>(null)
  // Both dialogs return focus to the button that opened them; once that is
  // gone (its provider deleted), to the empty state's Add provider, else the
  // page's.
  const focus = useReturnFocus()
  const addRef = useRef<HTMLButtonElement>(null)
  const restoreFocus = (event: Event) => focus.restore(event, () => addRef.current ?? fallbackFocus())

  const openForm = (next: Editing, trigger: HTMLElement | null) => {
    focus.opened(trigger)
    setEditing(next)
  }
  useImperativeHandle(ref, () => ({ addProvider: (trigger) => openForm({ kind: "new" }, trigger) }))
  const closeForm = () => setEditing(null)
  const openDelete = (next: ModelProvider, trigger: HTMLElement) => {
    focus.opened(trigger)
    setDeleting(next)
  }
  // The form has no workers to offer: it closes for the add dialog, which
  // returns focus to the form's opener.
  const addWorker = () => {
    const opener = focus.handOff()
    closeForm()
    onAddWorker(opener)
  }
  // The provider as the server has it now; undefined once it is gone.
  const reload = async (providerId: string) => {
    const res = await providers.refetch({ throwOnError: true })
    return res.data?.find((p) => p.providerId === providerId)
  }

  let body
  // Only a failed first load shows the error; a failed refetch keeps the list.
  if (providers.isError && !providers.data) {
    body = (
      <div className="flex items-center gap-3">
        <p className="text-sm text-muted-foreground">{t("providers.loadFailed")}</p>
        <Button variant="outline" size="sm" onClick={() => void providers.refetch()}>
          {t("common.tryAgain")}
        </Button>
      </div>
    )
  } else if (!providers.data) {
    body = <LoadingList />
  } else if (providers.data.length === 0) {
    body = <NoProviders onAdd={(trigger) => openForm({ kind: "new" }, trigger)} addRef={addRef} />
  } else {
    body = (
      <ul className="flex flex-col divide-y border">
        {providers.data.map((p) => (
          <ProviderRow
            key={p.providerId}
            provider={p}
            workers={workers.data}
            onEdit={(trigger) => openForm({ kind: "edit", provider: p }, trigger)}
            onDelete={(trigger) => openDelete(p, trigger)}
          />
        ))}
      </ul>
    )
  }

  return (
    <section
      id="providers"
      tabIndex={-1}
      className="flex scroll-mt-8 flex-col gap-3 outline-none"
      aria-labelledby="providers-heading"
    >
      {/* Providers are added from the page header's Add menu, or the empty state. */}
      <h2 id="providers-heading" className="text-sm font-medium">
        {t("providers.title")}
      </h2>
      {body}

      <Dialog open={editing !== null} onOpenChange={(open) => !open && closeForm()}>
        <DialogContent
          className="max-h-[calc(100svh-2rem)] gap-5 overflow-y-auto sm:max-w-xl"
          onCloseAutoFocus={restoreFocus}
        >
          {editing && (
            <ProviderForm
              editing={editing.kind === "edit" ? editing.provider : undefined}
              providers={providers.data ?? []}
              onDone={closeForm}
              reload={reload}
              onAddWorker={addWorker}
            />
          )}
        </DialogContent>
      </Dialog>
      <DeleteDialog
        provider={deleting}
        onClose={() => setDeleting(null)}
        onDeleted={focus.forget}
        onCloseAutoFocus={restoreFocus}
      />
    </section>
  )
})

/** "https://host/path", or "Default endpoint". */
function Endpoint({ provider }: { provider: ModelProvider }) {
  const { t } = useTranslation()
  if (provider.baseUrl === "") return <span>{t("providers.defaultEndpoint")}</span>
  return <span className="min-w-0 truncate font-mono">{provider.baseUrl}</span>
}

function ProviderRow({
  provider,
  workers,
  onEdit,
  onDelete,
}: {
  provider: ModelProvider
  workers: Worker[] | undefined
  /** Each gets the button pressed, where focus returns. */
  onEdit: (trigger: HTMLElement) => void
  onDelete: (trigger: HTMLElement) => void
}) {
  const { t } = useTranslation()
  const assigned = provider.workerIds.map(
    (id) => workers?.find((w) => w.workerId === id) ?? { workerId: id, name: id, online: false },
  )
  const sessions =
    provider.maxSessions === 0
      ? t("providers.sessionsUnlimited", { leased: provider.leasedSessions })
      : t("providers.sessions", { leased: provider.leasedSessions, max: provider.maxSessions })
  return (
    <li className="flex flex-col gap-2 px-4 py-3">
      <div className="flex min-w-0 items-center gap-2">
        <span className="truncate text-sm font-medium">{provider.name}</span>
        {provider.isDefault && (
          <span className="shrink-0 border px-1.5 text-xs leading-5 text-muted-foreground">
            {t("providers.defaultMark")}
          </span>
        )}
        <span className="flex-1" />
        <span className="shrink-0 text-xs text-muted-foreground tabular-nums">{sessions}</span>
        <Button variant="ghost" size="sm" aria-label={t("providers.editFor", { name: provider.name })} onClick={(e) => onEdit(e.currentTarget)}>
          {t("providers.edit")}
        </Button>
        <Button
          variant="ghost"
          size="icon-sm"
          aria-label={t("providers.deleteFor", { name: provider.name })}
          title={t("providers.deleteFor", { name: provider.name })}
          onClick={(e) => onDelete(e.currentTarget)}
        >
          <Trash2Icon />
        </Button>
      </div>
      <p className="flex min-w-0 flex-wrap gap-x-3 gap-y-1 text-xs text-muted-foreground">
        <Endpoint provider={provider} />
        <span>{t(`providers.schemes.${provider.scheme}`)}</span>
        <span className="font-mono">{t("providers.keyLabel", { masked: maskedSecret(provider.keyLast4) })}</span>
        {provider.env.length > 0 && (
          <span title={provider.env.map((v) => v.name).join("\n")}>
            {t("providers.variables", { count: provider.env.length })}
          </span>
        )}
      </p>
      {assigned.length === 0 ? (
        <p className="text-xs text-muted-foreground">{t("providers.noWorkers")}</p>
      ) : (
        <ul aria-label={t("providers.workersLabel")} className="flex min-w-0 flex-wrap gap-1.5">
          {assigned.map((w) => (
            <li key={w.workerId} className="flex h-6 min-w-0 items-center gap-1.5 border px-2 text-xs text-muted-foreground">
              <DeviceStatusDot online={w.online} className="size-2" />
              <span className="truncate">{w.name}</span>
            </li>
          ))}
        </ul>
      )}
      {provider.secretsUnreadable && (
        <div role="alert" className="flex items-start gap-2 border border-destructive/40 px-3 py-2 text-sm text-destructive">
          <TriangleAlertIcon className="mt-0.5 size-4 shrink-0" aria-hidden />
          <span className="min-w-0 flex-1">{t("providers.secretsUnreadable")}</span>
          <Button variant="outline" size="sm" className="shrink-0" onClick={(e) => onEdit(e.currentTarget)}>
            {t("providers.reenterSecrets")}
          </Button>
        </div>
      )}
    </li>
  )
}

function DeleteDialog({
  provider,
  onClose,
  onDeleted,
  onCloseAutoFocus,
}: {
  provider: ModelProvider | null
  onClose: () => void
  /** The provider was deleted: its row, and the button that opened this, go. */
  onDeleted: () => void
  onCloseAutoFocus: (event: Event) => void
}) {
  const { t } = useTranslation()
  const remove = useDeleteModelProvider()
  // Kept while the dialog closes, so its text doesn't vanish mid-animation.
  const [shown, setShown] = useState(provider)
  if (provider && provider !== shown) {
    setShown(provider)
    remove.reset()
  }
  const error =
    remove.error &&
    (isApiError(remove.error, "failed_precondition") ? t("providers.deleteBlocked") : providerErrorMessage(remove.error))
  return (
    <Dialog open={provider !== null} onOpenChange={(open) => !open && onClose()}>
      <DialogContent showCloseButton={false} onCloseAutoFocus={onCloseAutoFocus}>
        <DialogHeader>
          <DialogTitle>{t("providers.deleteTitle", { name: shown?.name ?? "" })}</DialogTitle>
          <DialogDescription>{t("providers.deleteDescription")}</DialogDescription>
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
          <Button
            variant="destructive"
            disabled={remove.isPending}
            onClick={() =>
              shown &&
              remove.mutate(
                { providerId: shown.providerId },
                {
                  onSuccess: () => {
                    onDeleted()
                    onClose()
                  },
                },
              )
            }
          >
            {remove.isPending && <Spinner aria-label={t("providers.deleting")} />}
            {t("common.delete")}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function LoadingList() {
  const { t } = useTranslation()
  return (
    <ul aria-busy="true" aria-label={t("providers.loading")} className="flex flex-col divide-y border">
      {[0, 1].map((i) => (
        <li key={i} className="flex flex-col gap-2.5 px-4 py-3.5">
          <Skeleton className="h-3.5 w-32" />
          <Skeleton className="h-3 w-64" />
          <Skeleton className="h-5 w-40" />
        </li>
      ))}
    </ul>
  )
}

/** No providers: what one is, and the way to add one. */
function NoProviders({ onAdd, addRef }: { onAdd: (trigger: HTMLElement) => void; addRef: Ref<HTMLButtonElement> }) {
  const { t } = useTranslation()
  return (
    <Empty className="border border-dashed px-4 py-8 sm:px-6 sm:py-10">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <KeyRoundIcon />
        </EmptyMedia>
        <EmptyTitle>{t("providers.emptyTitle")}</EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button ref={addRef} onClick={(e) => onAdd(e.currentTarget)}>
          <PlusIcon />
          {t("providers.addProvider")}
        </Button>
      </EmptyContent>
    </Empty>
  )
}
