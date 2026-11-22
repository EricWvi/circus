import { KeyRoundIcon, PlusIcon, Trash2Icon, TriangleAlertIcon } from "lucide-react"
import { useState } from "react"
import { useTranslation } from "react-i18next"

import { isApiError } from "@/api/errors"
import { useDeleteModelProvider, useModelProviders, useWorkers } from "@/api/hooks"
import type { ModelProvider, Worker } from "@/api/types"
import { ProviderForm } from "@/components/model-providers/provider-dialog"
import { PageHeader } from "@/components/page-header"
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
import { Empty, EmptyContent, EmptyDescription, EmptyHeader, EmptyMedia, EmptyTitle } from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { maskedSecret, providerErrorMessage } from "@/lib/model-provider-form"

/** What the provider dialog shows: a new provider, or one to edit. */
type Editing = { kind: "new" } | { kind: "edit"; provider: ModelProvider }

/** The signed-in user's model providers: list, add, edit, delete. */
export function ProvidersPage() {
  const { t } = useTranslation()
  const providers = useModelProviders({ live: true })
  const workers = useWorkers()
  const [editing, setEditing] = useState<Editing | null>(null)
  const [deleting, setDeleting] = useState<ModelProvider | null>(null)
  const formFocus = useOverlayFocus()
  const deleteFocus = useOverlayFocus()

  const openForm = (next: Editing | null) => {
    formFocus.onOpenChange(next !== null)
    setEditing(next)
  }
  const openDelete = (next: ModelProvider | null) => {
    deleteFocus.onOpenChange(next !== null)
    setDeleting(next)
  }
  // The provider as the server has it now; undefined once it is gone.
  const reload = async (providerId: string) => {
    const res = await providers.refetch({ throwOnError: true })
    return res.data?.find((p) => p.providerId === providerId)
  }

  let body
  if (providers.isError && !providers.data) {
    body = (
      <div className="flex flex-col items-center gap-4 py-12">
        <p className="text-sm text-muted-foreground">{t("providers.loadFailed")}</p>
        <Button variant="outline" onClick={() => void providers.refetch()}>
          {t("common.tryAgain")}
        </Button>
      </div>
    )
  } else if (!providers.data) {
    body = <LoadingList />
  } else if (providers.data.length === 0) {
    body = <NoProviders onAdd={() => openForm({ kind: "new" })} />
  } else {
    body = (
      <ul className="flex flex-col divide-y border">
        {providers.data.map((p) => (
          <ProviderRow
            key={p.providerId}
            provider={p}
            workers={workers.data}
            onEdit={() => openForm({ kind: "edit", provider: p })}
            onDelete={() => openDelete(p)}
          />
        ))}
      </ul>
    )
  }

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        actions={
          providers.data && providers.data.length > 0 ? (
            <Button variant="outline" size="sm" onClick={() => openForm({ kind: "new" })}>
              <PlusIcon />
              {t("providers.addProvider")}
            </Button>
          ) : undefined
        }
      >
        <h1 className="min-w-0 truncate font-medium">{t("providers.title")}</h1>
      </PageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-[760px] flex-col px-4 py-8">{body}</div>
      </div>

      <Dialog open={editing !== null} onOpenChange={(open) => !open && openForm(null)}>
        <DialogContent
          className="max-h-[calc(100svh-2rem)] gap-5 overflow-y-auto sm:max-w-xl"
          onCloseAutoFocus={formFocus.onCloseAutoFocus}
        >
          {editing && (
            <ProviderForm
              editing={editing.kind === "edit" ? editing.provider : undefined}
              providers={providers.data ?? []}
              onDone={() => openForm(null)}
              reload={reload}
            />
          )}
        </DialogContent>
      </Dialog>
      <DeleteDialog provider={deleting} onClose={() => openDelete(null)} onCloseAutoFocus={deleteFocus.onCloseAutoFocus} />
    </div>
  )
}

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
  onEdit: () => void
  onDelete: () => void
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
        <Button variant="ghost" size="sm" aria-label={t("providers.editFor", { name: provider.name })} onClick={onEdit}>
          {t("providers.edit")}
        </Button>
        <Button
          variant="ghost"
          size="icon-sm"
          aria-label={t("providers.deleteFor", { name: provider.name })}
          title={t("providers.deleteFor", { name: provider.name })}
          onClick={onDelete}
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
          <Button variant="outline" size="sm" className="shrink-0" onClick={onEdit}>
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
  onCloseAutoFocus,
}: {
  provider: ModelProvider | null
  onClose: () => void
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
            onClick={() => shown && remove.mutate({ providerId: shown.providerId }, { onSuccess: onClose })}
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
function NoProviders({ onAdd }: { onAdd: () => void }) {
  const { t } = useTranslation()
  return (
    <Empty className="border border-dashed px-4 py-8 sm:px-6 sm:py-10">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <KeyRoundIcon />
        </EmptyMedia>
        <EmptyTitle>{t("providers.emptyTitle")}</EmptyTitle>
        <EmptyDescription>{t("providers.emptyDescription")}</EmptyDescription>
      </EmptyHeader>
      <EmptyContent>
        <Button onClick={onAdd}>
          <PlusIcon />
          {t("providers.addProvider")}
        </Button>
      </EmptyContent>
    </Empty>
  )
}
