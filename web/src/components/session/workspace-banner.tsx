import {
  ChevronDownIcon,
  CircleArrowUpIcon,
  FolderClockIcon,
  MonitorOffIcon,
  TriangleAlertIcon,
} from "lucide-react"
import { useTranslation } from "react-i18next"

import type { WorkspaceStatus } from "@/api/types"
import { DisabledReason } from "@/components/disabled-reason"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Spinner } from "@/components/ui/spinner"
import { useSeconds } from "@/hooks/use-seconds"
import { cn } from "@/lib/utils"
import {
  formatElapsed,
  showsDetail,
  waitingText,
  workspaceReason,
} from "@/lib/workspace"

import { NoteText } from "./event-list"

const localClock = () => Date.now()

/**
 * Says why the session hasn't started working while its folder isn't
 * ready: what it waits for, the preparation under way (with the time it
 * has taken so far), or why it failed, with the device's own words and a
 * way to try again.
 */
export function WorkspaceBanner({
  workspace,
  retryDisabledReason,
  retrying,
  onRetry,
  serverNow = localClock,
}: {
  workspace: WorkspaceStatus
  /**
   * The server's clock, which the attempt's start is on; read when the
   * time it took is shown. The session page announces the states itself.
   */
  serverNow?: () => number
  /** Why Retry can't be used right now; null when it can. */
  retryDisabledReason: string | null
  /** A retry is under way. */
  retrying: boolean
  onRetry: () => void
}) {
  const { t } = useTranslation()
  const preparing = workspace.state === "preparing"
  const now = useSeconds(preparing, serverNow)
  const { path, repoUrl: repo, device } = workspace

  let icon = <FolderClockIcon className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
  let text: string
  switch (workspace.state) {
    case "preparing": {
      const elapsed = formatElapsed(workspace.attemptStartedAtUnixMs || workspace.updatedAtUnixMs, now)
      text = repo === "" ? t("workspace.creating", { path, elapsed }) : t("workspace.cloning", { repo, path, elapsed })
      icon = <Spinner className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
      break
    }
    case "failed":
      text = t("workspace.failed", {
        path,
        reason: workspaceReason(workspace.error, { device, detail: workspace.errorDetail }),
      })
      icon = <TriangleAlertIcon className="mt-0.5 size-4 shrink-0 text-destructive" aria-hidden />
      break
    default:
      text = waitingText(workspace)
      if (workspace.waiting === "device_offline") {
        icon = <MonitorOffIcon className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
      } else if (workspace.waiting === "daemon_outdated") {
        icon = <CircleArrowUpIcon className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
      }
  }

  const failed = workspace.state === "failed"
  const detail = failed && showsDetail(workspace) ? workspace.errorDetail : null

  return (
    <Collapsible asChild>
      <div
        className="group/ws flex shrink-0 flex-col border-b bg-muted/60 px-4 py-2 text-sm"
        data-workspace-state={workspace.state}
      >
        <div className="flex flex-wrap items-start gap-x-3 gap-y-1.5">
          <div className="flex min-w-0 flex-1 basis-64 items-start gap-2">
            {icon}
            <p className={cn("min-w-0 text-pretty [overflow-wrap:anywhere]", preparing && "tabular-nums")}>
              {text}
            </p>
          </div>
          {failed && (
            <div className="ml-auto flex shrink-0 items-center gap-2">
              {detail !== null && (
                <CollapsibleTrigger className="flex h-7 items-center gap-1 rounded-md px-2 text-xs text-muted-foreground outline-none transition-colors hover:bg-foreground/5 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50">
                  {t("workspace.details")}
                  <ChevronDownIcon
                    className="size-3 transition-transform group-data-[state=open]/ws:rotate-180"
                    aria-hidden
                  />
                </CollapsibleTrigger>
              )}
              <DisabledReason reason={retryDisabledReason}>
                {/* aria-disabled while a retry runs, so focus stays on the button. */}
                <Button
                  variant="outline"
                  size="sm"
                  className="h-7 shrink-0 bg-background aria-disabled:opacity-50"
                  disabled={retryDisabledReason !== null}
                  aria-disabled={retrying || undefined}
                  onClick={() => {
                    if (!retrying) onRetry()
                  }}
                >
                  {retrying && <Spinner aria-hidden />}
                  {t("workspace.retry")}
                </Button>
              </DisabledReason>
            </div>
          )}
        </div>
        {detail !== null && (
          <CollapsibleContent className="data-[state=closed]:fade-out-0 data-[state=closed]:slide-out-to-top-2 data-[state=open]:slide-in-from-top-2 outline-none data-[state=closed]:animate-out data-[state=open]:animate-in">
            <NoteText text={detail} label={t("workspace.details")} className="mb-0.5 bg-background/80" />
          </CollapsibleContent>
        )}
      </div>
    </Collapsible>
  )
}

/** One line under the composer while the folder isn't ready. */
export function WorkspaceComposerNote() {
  const { t } = useTranslation()
  return <p className="px-1 text-xs text-pretty text-muted-foreground">{t("workspace.composerNote")}</p>
}
