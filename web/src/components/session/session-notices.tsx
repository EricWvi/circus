import { MonitorOffIcon, TriangleAlertIcon, UnplugIcon } from "lucide-react"
import { useRef, useState } from "react"
import { useTranslation } from "react-i18next"

import type { Device } from "@/api/types"
import { DisabledReason } from "@/components/disabled-reason"
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
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { useSeconds } from "@/hooks/use-seconds"
import { formatTimeAgo } from "@/lib/time"
import { workerDisconnectedText } from "@/lib/worker-link"

/** Takes the composer's place in a stopped session. */
export function StoppedNotice() {
  const { t } = useTranslation()
  return <p className="py-3 text-center text-sm text-muted-foreground">{t("session.stopped")}</p>
}

/** Takes the composer's place once the session was deleted. */
export function GoneNotice() {
  const { t } = useTranslation()
  return <p className="py-3 text-center text-sm text-muted-foreground">{t("session.gone")}</p>
}

/**
 * A broken session: part of its history couldn't be saved. The user can
 * wait for the worker to repair it, continue without the missing part
 * (after confirming), or stop the session.
 */
export function BrokenCard({
  disabledReason,
  onContinueWithout,
  onStopSession,
  onContinued,
}: {
  /** Why the actions can't be used right now; null when they can. */
  disabledReason: string | null
  onContinueWithout: () => void
  onStopSession: () => void
  /** Where focus goes once the dialog closed after continuing (the card is gone then). */
  onContinued: () => void
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const continued = useRef(false)
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  const change = (next: boolean) => {
    onOpenChange(next)
    setOpen(next)
  }
  const disabled = disabledReason !== null

  return (
    <section className="flex flex-col gap-3 border border-destructive/40 p-3 text-sm">
      <div className="flex gap-2">
        <TriangleAlertIcon className="mt-0.5 size-4 shrink-0 text-destructive" aria-hidden />
        <div className="flex flex-col gap-1">
          <h2 className="font-medium text-destructive">{t("session.broken.title")}</h2>
          <p className="text-pretty text-muted-foreground">{t("session.broken.body")}</p>
        </div>
      </div>
      <div className="flex flex-wrap justify-end gap-2">
        <DisabledReason reason={disabledReason}>
          <Button
            variant="outline"
            size="sm"
            className="border-destructive/40 text-destructive hover:bg-destructive/10 hover:text-destructive dark:border-destructive/40 dark:hover:bg-destructive/15"
            disabled={disabled}
            onClick={onStopSession}
          >
            {t("session.broken.stopSession")}
          </Button>
        </DisabledReason>
        <DisabledReason reason={disabledReason}>
          <Button ref={triggerRef} size="sm" disabled={disabled} onClick={() => change(true)}>
            {t("session.broken.continueWithout")}
          </Button>
        </DisabledReason>
      </div>
      <Dialog open={open} onOpenChange={change}>
        <DialogContent
          showCloseButton={false}
          onCloseAutoFocus={(event) => {
            if (!continued.current) return onCloseAutoFocus(event)
            event.preventDefault()
            onContinued()
          }}
        >
          <DialogHeader>
            <DialogTitle>{t("session.broken.confirmTitle")}</DialogTitle>
            <DialogDescription>{t("session.broken.confirmBody")}</DialogDescription>
          </DialogHeader>
          <DialogFooter>
            <DialogClose asChild>
              <Button variant="outline">{t("common.cancel")}</Button>
            </DialogClose>
            <Button
              disabled={disabled}
              onClick={() => {
                continued.current = true
                change(false)
                onContinueWithout()
              }}
            >
              {t("session.broken.confirm")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  )
}

/** The session's device is offline: Claude can reply, its tools can't run. */
export function DeviceOfflineNotice({ device }: { device: Device }) {
  const { t } = useTranslation()
  return (
    <div className="flex gap-2 border px-3 py-2 text-sm text-muted-foreground">
      <MonitorOffIcon className="mt-0.5 size-4 shrink-0" aria-hidden />
      <p className="text-pretty">
        {t("session.deviceOffline", { device: device.name, ago: formatTimeAgo(device.lastSeenUnixMs) })}
      </p>
    </div>
  )
}

/**
 * The worker holding the session lost its connection to the server. Its
 * lease lasts 40 to 60 seconds more, so it can come back and carry on;
 * with a turn running, the turn is interrupted if it doesn't. The time is
 * the server's: `serverNow` is its clock, which the stream tells
 * (`clockKnown`); until it has, the notice says no time rather than one
 * this browser's clock may get wrong.
 */
export function WorkerDisconnectedNotice({
  disconnectedAtUnixMs,
  turnRunning,
  serverNow,
  clockKnown,
}: {
  disconnectedAtUnixMs: number
  turnRunning: boolean
  serverNow: () => number
  clockKnown: boolean
}) {
  // Subscribes the notice to language changes.
  useTranslation()
  // Re-renders every 15 seconds; each render reads the clock as it is
  // then, so the time is right at once when the stream tells the clock.
  useSeconds(true, serverNow, 15_000)
  const now = clockKnown ? serverNow() : null
  return (
    <div role="status" data-worker-disconnected className="flex gap-2 border px-3 py-2 text-sm text-muted-foreground">
      <UnplugIcon className="mt-0.5 size-4 shrink-0" aria-hidden />
      <p className="text-pretty">{workerDisconnectedText(disconnectedAtUnixMs, turnRunning, now)}</p>
    </div>
  )
}
