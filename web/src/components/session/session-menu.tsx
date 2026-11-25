import { EllipsisIcon } from "lucide-react"
import { useState } from "react"
import { useTranslation } from "react-i18next"

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
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import type { SessionLedgerStatus } from "@/api/types"
import { useMenuFocus } from "@/hooks/use-menu-focus"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"

import { DeleteSessionDialog } from "./delete-session-dialog"

/**
 * The session header's actions, each after confirming: stopping the
 * session, and deleting it once stopped.
 */
export function SessionMenu({
  sessionId,
  projectId,
  ledger,
  stopped,
  disabledReason,
  onStopSession,
}: {
  sessionId: string
  projectId: string
  /** The session's snapshot, for warning up front that deleting discards its changes. */
  ledger: SessionLedgerStatus | undefined
  stopped: boolean
  /** Why the actions can't be used right now; null when they can. */
  disabledReason: string | null
  onStopSession: () => void
}) {
  const { t } = useTranslation()
  const menuFocus = useMenuFocus()
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  const [confirming, setConfirming] = useState<"stop" | "delete" | null>(null)
  const change = (open: boolean, action: "stop" | "delete" = "stop") => {
    onOpenChange(open)
    setConfirming(open ? action : null)
  }

  return (
    <>
      <DropdownMenu>
        <DropdownMenuTrigger asChild {...menuFocus.trigger}>
          <Button ref={triggerRef} variant="ghost" size="icon-sm" aria-label={t("session.actions")}>
            <EllipsisIcon />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent align="end" className="w-auto min-w-44" {...menuFocus.content}>
          <DropdownMenuItem
            variant="destructive"
            disabled={stopped || disabledReason !== null}
            onSelect={() => change(true)}
          >
            {t("session.stopSession")}
          </DropdownMenuItem>
          <DropdownMenuItem
            variant="destructive"
            disabled={!stopped || disabledReason !== null}
            onSelect={() => change(true, "delete")}
          >
            {t("session.delete.menu")}
          </DropdownMenuItem>
        </DropdownMenuContent>
      </DropdownMenu>
      <DeleteSessionDialog
        open={confirming === "delete"}
        onOpenChange={(open) => change(open, "delete")}
        sessionId={sessionId}
        projectId={projectId}
        ledger={ledger}
        disabledReason={disabledReason}
        onCloseAutoFocus={onCloseAutoFocus}
      />
      <Dialog open={confirming === "stop"} onOpenChange={(open) => change(open)}>
        <DialogContent showCloseButton={false} onCloseAutoFocus={onCloseAutoFocus}>
          <DialogHeader>
            <DialogTitle>{t("session.stopConfirmTitle")}</DialogTitle>
            <DialogDescription>{t("session.stopConfirmBody")}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline">{t("common.cancel")}</Button>
            </DialogClose>
            <Button
              variant="destructive"
              disabled={disabledReason !== null}
              onClick={() => {
                change(false)
                onStopSession()
              }}
            >
              {t("session.stopSession")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
