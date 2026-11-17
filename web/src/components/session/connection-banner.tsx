import { WifiOffIcon } from "lucide-react"
import { useEffect, useReducer } from "react"
import { useTranslation } from "react-i18next"

import { Button } from "@/components/ui/button"
import { Spinner } from "@/components/ui/spinner"
import type { Connection } from "@/state/connection"

/** Re-renders every `ms` while `active`. */
function useTick(active: boolean, ms: number) {
  const [, tick] = useReducer((n: number) => n + 1, 0)
  useEffect(() => {
    if (!active) return
    const timer = window.setInterval(tick, ms)
    return () => window.clearInterval(timer)
  }, [active, ms])
}

/**
 * Says the conversation isn't live once the loss has been noticed (the
 * provider decides when, from when the connection was lost, so switching
 * pages doesn't hide it): reconnecting, with a countdown to the next attempt
 * and a way to try now, or offline. Disappears as soon as the connection is
 * back. The live region before it stays mounted, so announcements are made.
 */
export function ConnectionBanner({
  connection,
  noticed,
  onRetryNow,
}: {
  connection: Connection
  /** The loss has lasted long enough to show. */
  noticed: boolean
  onRetryNow: () => void
}) {
  const { t } = useTranslation()
  const visible = noticed && connection.kind !== "connected"
  const counting = visible && connection.kind === "reconnecting" && connection.nextRetryAt !== null
  useTick(counting, 250)

  let announcement = ""
  if (visible) {
    announcement =
      connection.kind === "offline" ? t("session.connection.offline") : t("session.connection.reconnectingNow")
  }

  let banner = null
  if (visible && connection.kind === "offline") {
    banner = (
      <div className="flex shrink-0 items-center gap-2 border-b bg-muted/60 px-4 py-2 text-sm">
        <WifiOffIcon className="size-4 shrink-0 text-muted-foreground" aria-hidden />
        <span aria-hidden>{t("session.connection.offline")}</span>
      </div>
    )
  } else if (visible && connection.kind === "reconnecting") {
    const attempting = connection.nextRetryAt === null
    // Read at render, so the countdown is right on the render that brings a
    // new deadline; the tick above re-renders it.
    // oxlint-disable-next-line react/purity -- a countdown is a function of the clock at render
    const now = Date.now()
    const seconds = attempting ? 0 : Math.max(0, Math.ceil((connection.nextRetryAt! - now) / 1000))
    banner = (
      <div className="flex shrink-0 items-center gap-3 border-b bg-muted/60 px-4 py-1.5 text-sm">
        <Spinner className="text-muted-foreground" aria-hidden />
        {/* The countdown changes every second; only the status before it is announced. */}
        <span className="min-w-0 flex-1 tabular-nums" aria-hidden>
          {seconds > 0
            ? t("session.connection.reconnecting", { seconds })
            : t("session.connection.reconnectingNow")}
        </span>
        {/* aria-disabled rather than disabled while an attempt runs, so a
            keyboard user's focus stays on the button. */}
        <Button
          variant="outline"
          size="sm"
          className="h-7 shrink-0 bg-background aria-disabled:opacity-50"
          aria-disabled={attempting || undefined}
          onClick={() => {
            if (!attempting) onRetryNow()
          }}
        >
          {t("session.connection.retryNow")}
        </Button>
      </div>
    )
  }

  return (
    <>
      <span role="status" className="sr-only">
        {announcement}
      </span>
      {banner}
    </>
  )
}
