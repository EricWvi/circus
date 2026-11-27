import { TriangleAlertIcon } from "lucide-react"
import { memo, useRef } from "react"
import { useTranslation } from "react-i18next"

import { errorMessage, isApiError } from "@/api/errors"
import { useDeleteMachine } from "@/api/hooks"
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
import { Spinner } from "@/components/ui/spinner"

/** A device or worker to delete, as the list shows it. */
export interface DeleteTarget {
  kind: "device" | "worker"
  id: string
  name: string
  online: boolean
}

/**
 * Confirms deleting one of the user's devices or workers, saying what that
 * does (as `circus server device revoke` and `circus server worker revoke`
 * do), and warning when it is online. Once deleted (or found deleted
 * already: the server's not_found), `onDeleted` runs, the dialog closes and
 * the lists catch up. `target` stays set while the dialog closes, so its
 * exit animation shows it. Memoized: the devices page renders again on
 * every status poll, and the dialog has nothing to take from that.
 */
export const DeleteMachineDialog = memo(function DeleteMachineDialog({
  open,
  target,
  onOpenChange,
  onDeleted,
  onCloseAutoFocus,
}: {
  open: boolean
  /** What to delete. */
  target: DeleteTarget | null
  onOpenChange: (open: boolean) => void
  onDeleted: () => void
  onCloseAutoFocus?: (event: Event) => void
}) {
  return (
    <Dialog open={open && target !== null} onOpenChange={onOpenChange}>
      {target && (
        <DeleteMachine
          key={`${target.kind}:${target.id}`}
          target={target}
          onOpenChange={onOpenChange}
          onDeleted={onDeleted}
          onCloseAutoFocus={onCloseAutoFocus}
        />
      )}
    </Dialog>
  )
})

function DeleteMachine({
  target,
  onOpenChange,
  onDeleted,
  onCloseAutoFocus,
}: {
  target: DeleteTarget
  onOpenChange: (open: boolean) => void
  onDeleted: () => void
  onCloseAutoFocus?: (event: Event) => void
}) {
  const { t } = useTranslation()
  const remove = useDeleteMachine()
  const pending = remove.isPending
  const { name } = target
  const device = target.kind === "device"
  const effects = [
    t("devices.delete.revoked"),
    t("devices.delete.nameFreed", { name }),
    device ? t("devices.delete.locations") : t("devices.delete.providers"),
    device ? t("devices.delete.otherRoleDevice") : t("devices.delete.otherRoleWorker"),
    t("devices.delete.service"),
  ]
  const done = () => {
    onDeleted()
    onOpenChange(false)
  }
  // Set at once, before the busy state renders: a second Enter, Space or
  // click while a delete is under way sends nothing.
  const submitted = useRef(false)
  const confirm = () => {
    if (submitted.current) return
    submitted.current = true
    remove.mutate(
      { kind: target.kind, id: target.id },
      {
        onSuccess: done,
        // Deleted already (another tab, or an answer lost and the delete
        // sent again): what was asked for is done.
        onError: (error) => {
          submitted.current = false
          if (isApiError(error, "not_found")) done()
        },
      },
    )
  }
  const failed = remove.error !== null && !isApiError(remove.error, "not_found")
  return (
    <DialogContent
      showCloseButton={false}
      onCloseAutoFocus={onCloseAutoFocus}
      // A delete under way can't be called off: the dialog stays until it is answered.
      onEscapeKeyDown={(event) => pending && event.preventDefault()}
      onInteractOutside={(event) => pending && event.preventDefault()}
    >
      <DialogHeader>
        <DialogTitle className="wrap-anywhere">
          {device ? t("devices.delete.titleDevice", { name }) : t("devices.delete.titleWorker", { name })}
        </DialogTitle>
        <DialogDescription asChild>
          <ul className="flex list-disc flex-col gap-1 pl-5">
            {effects.map((effect) => (
              <li key={effect}>{effect}</li>
            ))}
          </ul>
        </DialogDescription>
      </DialogHeader>
      {target.online && (
        <p className="flex items-start gap-2 border border-destructive/40 bg-destructive/5 px-3 py-2 text-sm text-destructive">
          <TriangleAlertIcon className="mt-0.5 size-4 shrink-0" aria-hidden />
          <span>{t("devices.delete.online", { name })}</span>
        </p>
      )}
      {failed && (
        <p role="alert" className="text-sm text-destructive">
          {t("devices.delete.failed", { error: errorMessage(remove.error) })}
        </p>
      )}
      <DialogFooter>
        <DialogClose asChild>
          <Button variant="outline" disabled={pending}>
            {t("common.cancel")}
          </Button>
        </DialogClose>
        <Button
          variant="destructive"
          aria-busy={pending || undefined}
          className="aria-busy:pointer-events-none"
          onClick={confirm}
        >
          {pending && <Spinner aria-hidden role={undefined} aria-label={undefined} />}
          {pending ? t("devices.delete.deleting") : t("devices.delete.confirm")}
        </Button>
      </DialogFooter>
    </DialogContent>
  )
}
