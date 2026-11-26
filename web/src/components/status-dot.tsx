import { CircleArrowUpIcon, FolderSyncIcon, FolderXIcon, MonitorOffIcon } from "lucide-react"
import { useTranslation } from "react-i18next"

import type { WorkspaceStatus } from "@/api/types"
import type { SessionState } from "@/lib/types"
import { cn } from "@/lib/utils"

// The mono theme is greyscale, so states differ by shape, fill and motion.
// Only "broken" and "failed" use a hue (the theme's destructive color):
// broken fills it, a failed turn rings it.
function Glyph({ state }: { state: SessionState }) {
  switch (state) {
    case "running":
      return (
        <>
          <span className="absolute inset-0 animate-ping rounded-full bg-foreground/40 motion-reduce:animate-none" />
          <span className="relative size-2 rounded-full bg-foreground" />
        </>
      )
    case "awaiting_approval":
    case "awaiting_answer":
      return (
        <span className="size-[7px] rotate-45 bg-foreground ring-[3px] ring-foreground/20" />
      )
    case "queued":
      return (
        <span className="flex size-2 items-center justify-center rounded-full border border-muted-foreground">
          <span className="size-[3px] rounded-full bg-muted-foreground" />
        </span>
      )
    case "idle":
      return <span className="size-2 rounded-full border border-muted-foreground/70" />
    case "interrupted":
      return (
        <span className="size-2 rounded-full border border-muted-foreground bg-[linear-gradient(90deg,var(--color-muted-foreground)_50%,transparent_50%)]" />
      )
    case "failed":
      return <span className="size-2 rounded-full border-[1.5px] border-destructive" />
    case "broken":
      return <span className="size-2 rounded-full bg-destructive" />
    case "stopped":
      return <span className="size-[7px] bg-muted-foreground/60" />
  }
}

export function StatusDot({
  state,
  className,
  label = true,
}: {
  state: SessionState
  className?: string
  /** Expose the state name to assistive technology. */
  label?: boolean
}) {
  const { t } = useTranslation()
  return (
    <span
      className={cn("relative inline-flex size-2.5 shrink-0 items-center justify-center", className)}
      role={label ? "img" : undefined}
      aria-label={label ? t(`status.${state}`) : undefined}
      aria-hidden={label ? undefined : true}
      title={label ? t(`status.${state}`) : undefined}
      data-state={state}
    >
      <Glyph state={state} />
    </span>
  )
}

/**
 * A session's state in its header. On narrow screens only its mark shows,
 * as the folder's badge does; its words stay for assistive technology and
 * on hover.
 */
export function StatusBadge({ state }: { state: SessionState }) {
  const { t } = useTranslation()
  return (
    <span
      title={t(`status.${state}`)}
      className={cn(
        "inline-flex h-6 shrink-0 items-center gap-1.5 border px-1.5 text-xs whitespace-nowrap sm:px-2",
        state === "broken" || state === "failed"
          ? "border-destructive/40 text-destructive"
          : "text-muted-foreground",
        (state === "awaiting_approval" || state === "awaiting_answer") && "border-foreground text-foreground",
      )}
    >
      <StatusDot state={state} label={false} />
      <span className="max-sm:sr-only">{t(`status.${state}`)}</span>
    </span>
  )
}

/**
 * Next to a session in a list while its folder isn't ready: a folder being
 * prepared (pending or preparing), or one that failed (the theme's
 * destructive color, like a broken session). Nothing otherwise, and
 * nothing for a stopped session, which waits for nothing.
 */
export function WorkspaceMark({
  workspace,
  state,
  className,
}: {
  workspace: WorkspaceStatus | undefined
  state: SessionState
  className?: string
}) {
  const { t } = useTranslation()
  if (!workspace || state === "stopped") return null
  const failed = workspace.state === "failed"
  if (!failed && workspace.state !== "pending" && workspace.state !== "preparing") return null
  const label = t(failed ? "workspace.mark.failed" : "workspace.mark.preparing")
  const Icon = failed ? FolderXIcon : FolderSyncIcon
  return (
    <span
      role="img"
      aria-label={label}
      title={label}
      data-workspace={workspace.state}
      className={cn(
        "inline-flex shrink-0 items-center",
        failed ? "text-destructive" : "text-muted-foreground",
        className,
      )}
    >
      <Icon className="size-3.5" aria-hidden />
    </span>
  )
}

const workspaceBadges = {
  waitingDevice: { Icon: MonitorOffIcon, key: "workspace.badge.waitingDevice" },
  waitingUpdate: { Icon: CircleArrowUpIcon, key: "workspace.badge.waitingUpdate" },
  preparing: { Icon: FolderSyncIcon, key: "workspace.badge.preparing" },
  failed: { Icon: FolderXIcon, key: "workspace.badge.failed" },
} as const

/** Which badge a folder that isn't ready gets: as its banner says it. */
function workspaceBadgeKind(ws: Pick<WorkspaceStatus, "state" | "waiting">): keyof typeof workspaceBadges {
  if (ws.state === "failed") return "failed"
  if (ws.state === "pending" && ws.waiting === "device_offline") return "waitingDevice"
  if (ws.state === "pending" && ws.waiting === "daemon_outdated") return "waitingUpdate"
  return "preparing"
}

/**
 * In a session's header in place of its state while it waits for its
 * folder, in the banner's terms: waiting for the device or an update,
 * being prepared (or about to be), or failed (like a broken session's
 * badge). On narrow screens only its icon shows; its words stay for
 * assistive technology and on hover.
 */
export function WorkspaceBadge({ workspace }: { workspace: Pick<WorkspaceStatus, "state" | "waiting"> }) {
  const { t } = useTranslation()
  const kind = workspaceBadgeKind(workspace)
  const { Icon, key } = workspaceBadges[kind]
  const label = t(key)
  return (
    <span
      data-workspace-badge={kind}
      title={label}
      className={cn(
        "inline-flex h-6 shrink-0 items-center gap-1.5 border px-1.5 text-xs whitespace-nowrap sm:px-2",
        kind === "failed" ? "border-destructive/40 text-destructive" : "text-muted-foreground",
      )}
    >
      <Icon className="size-3 shrink-0" aria-hidden />
      <span className="max-sm:sr-only">{label}</span>
    </span>
  )
}

/**
 * A device's link state in the same vocabulary: online is a solid dot (like
 * a running session, without the pulse), offline a hollow ring (like idle).
 */
export function DeviceStatusDot({ online, className }: { online: boolean; className?: string }) {
  const { t } = useTranslation()
  const label = t(online ? "devices.online" : "devices.offline")
  return (
    <span
      className={cn("relative inline-flex size-2.5 shrink-0 items-center justify-center", className)}
      role="img"
      aria-label={label}
      title={label}
      data-online={online}
    >
      {online ? (
        <span className="size-2 rounded-full bg-foreground" />
      ) : (
        <span className="size-2 rounded-full border border-muted-foreground/70" />
      )}
    </span>
  )
}
