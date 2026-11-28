import { useEffect, useRef, useState, type MouseEvent, type ReactNode } from "react"
import { useTranslation } from "react-i18next"
import { cn } from "cn"

import { useUpdateMachine, type MachineUpdateTarget } from "@/api/hooks"
import type { MachineUpdate } from "@/api/types"
import { Button } from "@/components/ui/button"
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { Spinner } from "@/components/ui/spinner"

/**
 * The part of a command's failure worth showing first: the output's last
 * non-empty line, and of a chain of causes ("download x: dial tcp: i/o
 * timeout") the last one. The whole output is in the failure's popover.
 */
function failureSummary(error: string): string {
  const line = error.split("\n").map((l) => l.trim()).filter(Boolean).at(-1) ?? ""
  return line.split(": ").filter(Boolean).at(-1) ?? line
}

type Shown = "none" | "running" | "failed"

/**
 * The update of a device's or worker's machine, on its card beside the
 * online indicator. Both cards of a machine show the same state, since the
 * server sends the machine's update on both rows: offered (a button to
 * update to `updateVersion`), running (a spinner, no button) or failed (a
 * short reason that opens the whole of it, and Retry while an update is
 * offered). Nothing visible otherwise.
 *
 * It stays mounted while nothing shows, for the card that was pressed: its
 * polite live region says when that attempt ends or fails (only on that
 * card, so a machine's two cards don't both speak, and never for states
 * found on page load), and that it runs unless focus moved to the running
 * status, which says so itself. When the element that had focus here is
 * removed (the pressed button, the running status) and focus fell to the
 * page, focus moves to what replaced it (the running status, Retry or the
 * failure), else to the card's menu button; focus the user moved
 * elsewhere is never taken back.
 */
export function MachineUpdateAction({
  target,
  updateVersion,
  update,
}: {
  target: MachineUpdateTarget
  updateVersion: string
  update?: MachineUpdate
}) {
  const { t } = useTranslation()
  const start = useUpdateMachine(target)
  const root = useRef<HTMLSpanElement>(null)
  // The element in this component that has focus, kept when it is removed
  // with focus on it and forgotten when the user moves focus elsewhere.
  const focused = useRef<HTMLElement | null>(null)
  // This card was pressed (with focus on its button, which then moves to
  // the running status): its live region speaks until that attempt ends.
  const [pressed, setPressed] = useState<false | "focused" | "unfocused">(false)
  const [announcement, setAnnouncement] = useState("")
  const shown: Shown = update?.state ?? "none"
  // The state last rendered, to announce its changes (adjusted during render, not in an effect).
  const [seen, setSeen] = useState<{ shown: Shown; target: string; changes: number }>({
    shown,
    target: update?.targetVersion ?? "",
    changes: 0,
  })
  if (seen.shown !== shown) {
    setSeen({ shown, target: update?.targetVersion ?? seen.target, changes: seen.changes + 1 })
    const name = target.name
    if (pressed && shown === "running") {
      // With focus on the pressed button, focus moves to the running status, which says it.
      if (pressed === "unfocused") setAnnouncement(t("devices.update.announceRunning", { name, version: update!.targetVersion }))
    } else if (pressed && shown === "failed") {
      setAnnouncement(t("devices.update.announceFailed", { name }))
      setPressed(false)
    } else if (pressed && seen.shown === "running") {
      setAnnouncement(t("devices.update.announceDone", { name, version: seen.target }))
      setPressed(false)
    }
  }

  const press = (event: MouseEvent<HTMLButtonElement>) => {
    if (start.isPending) return
    setPressed(event.currentTarget === document.activeElement ? "focused" : "unfocused")
    start.mutate(undefined, { onError: () => setPressed(false) })
  }

  // The element that had focus here was removed and focus fell to the
  // page: focus what replaced it, else the card's menu button.
  useEffect(() => {
    const gone = focused.current
    const el = root.current
    if (!gone || gone.isConnected || !el) return
    focused.current = null
    if (document.activeElement && document.activeElement !== document.body) return
    const next =
      ["status", "retry", "failure", "action"]
        .map((name) => el.querySelector<HTMLElement>(`[data-update-focus=${name}]`))
        .find(Boolean) ?? el.closest("li")?.querySelector<HTMLElement>("[data-card-focus]")
    next?.focus({ preventScroll: true })
  })

  const button = (label: string, aria: string, isRetry = false) => (
    <Button
      data-update-focus={isRetry ? "retry" : "action"}
      variant="outline"
      size="xs"
      aria-label={aria}
      aria-busy={start.isPending}
      // Not `disabled`: a browser takes focus off a button as it becomes disabled.
      aria-disabled={start.isPending}
      className="shrink-0 aria-disabled:cursor-not-allowed aria-disabled:opacity-50"
      onClick={press}
    >
      {start.isPending && <Spinner aria-hidden />}
      {label}
    </Button>
  )

  let content: ReactNode = null
  if (update?.state === "running") {
    content = (
      <span
        data-update-focus="status"
        tabIndex={-1}
        className="flex shrink-0 items-center gap-1 rounded-sm text-xs whitespace-nowrap text-muted-foreground outline-hidden focus-visible:ring-2 focus-visible:ring-ring/50"
      >
        <Spinner className="size-3" aria-hidden />
        {t("devices.update.updating", { version: update.targetVersion })}
      </span>
    )
  } else if (update?.state === "failed") {
    const summary =
      update.failure === "timed_out"
        ? t("devices.update.timedOutShort")
        : update.failure === "unreachable"
          ? t("devices.update.unreachableShort")
          : failureSummary(update.error)
            ? t("devices.update.failedShort", { error: failureSummary(update.error) })
            : t("devices.update.failedShortEmpty")
    const detail =
      update.failure === "timed_out"
        ? t("devices.update.timedOutDetail", { version: update.targetVersion })
        : update.failure === "unreachable"
          ? t("devices.update.unreachableDetail")
          : ""
    content = (
      <span className="flex min-w-0 items-center gap-2">
        <Popover>
          <PopoverTrigger asChild>
            <button
              data-update-focus="failure"
              type="button"
              aria-label={t("devices.update.failureLabel", { name: target.name, summary })}
              className="min-w-0 cursor-pointer truncate rounded-sm text-left text-xs text-destructive outline-hidden focus-visible:ring-2 focus-visible:ring-ring/50 sm:max-w-64"
            >
              {summary}
            </button>
          </PopoverTrigger>
          <PopoverContent
            align="end"
            collisionPadding={16}
            tabIndex={-1}
            // Focus the popover itself, not the output inside it (Tab reaches that).
            onOpenAutoFocus={(event) => {
              event.preventDefault()
              ;(event.currentTarget as HTMLElement).focus()
            }}
            className="w-[min(28rem,calc(100vw-2rem))] gap-2 text-xs"
            aria-label={t("devices.update.failedTitle", { version: update.targetVersion })}
          >
            <p className="font-medium">{t("devices.update.failedTitle", { version: update.targetVersion })}</p>
            {detail && <p className="text-muted-foreground">{detail}</p>}
            {update.error && (
              <pre
                tabIndex={0}
                className="max-h-[min(50vh,20rem)] overflow-auto rounded-md bg-muted p-2 font-mono text-xs whitespace-pre-wrap [overflow-wrap:anywhere] outline-hidden select-text focus-visible:ring-2 focus-visible:ring-ring/50"
              >
                {update.error}
              </pre>
            )}
          </PopoverContent>
        </Popover>
        {updateVersion &&
          button(
            t("devices.update.retry"),
            t("devices.update.retryLabel", { name: target.name, version: updateVersion }),
            true,
          )}
      </span>
    )
  } else if (updateVersion) {
    content = button(
      t("devices.update.action", { version: updateVersion }),
      t("devices.update.actionLabel", { name: target.name, version: updateVersion }),
    )
  }

  return (
    <span
      ref={root}
      data-slot="machine-update"
      className={cn("flex min-w-0 items-center", !content && "sr-only")}
      onFocus={(event) => {
        focused.current = event.target
      }}
      onBlur={(event) => {
        const left = event.target
        if (event.relatedTarget instanceof Node && event.currentTarget.contains(event.relatedTarget)) return
        // Still on the page after the blur: the user moved focus away, not a removal.
        queueMicrotask(() => {
          if (left.isConnected && focused.current === left) focused.current = null
        })
      }}
    >
      {content}
      <span className="sr-only" aria-live="polite" aria-atomic="true">
        {announcement}
      </span>
    </span>
  )
}
