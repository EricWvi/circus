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
import { useMenuFocus } from "@/hooks/use-menu-focus"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"

/** The session header's actions: stopping the session, after confirming. */
export function SessionMenu({
  stopped,
  disabledReason,
  onStopSession,
}: {
  stopped: boolean
  /** Why the actions can't be used right now; null when they can. */
  disabledReason: string | null
  onStopSession: () => void
}) {
  const { t } = useTranslation()
  const menuFocus = useMenuFocus()
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  const [confirming, setConfirming] = useState(false)
  const change = (open: boolean) => {
    onOpenChange(open)
    setConfirming(open)
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
        </DropdownMenuContent>
      </DropdownMenu>
      <Dialog open={confirming} onOpenChange={change}>
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
