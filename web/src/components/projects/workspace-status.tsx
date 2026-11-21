import { FolderCheckIcon, FolderClockIcon, FolderSyncIcon, FolderXIcon } from "lucide-react"
import { useTranslation } from "react-i18next"

import type { WorkspaceStatus } from "@/api/types"
import { workspaceReason } from "@/lib/workspace"

/**
 * A default location's folder on its device, in a few words: where it is
 * once ready, otherwise how far its preparation is.
 */
export function WorkspaceLocationStatus({ workspace }: { workspace: WorkspaceStatus }) {
  const { t } = useTranslation()
  const { device } = workspace
  let Icon = FolderClockIcon
  let text: string
  switch (workspace.state) {
    case "ready":
      Icon = FolderCheckIcon
      text = t("workspace.location.ready", { path: workspace.path })
      break
    case "preparing":
      Icon = FolderSyncIcon
      text = t(workspace.repoUrl === "" ? "workspace.location.creating" : "workspace.location.cloning")
      break
    case "failed":
      Icon = FolderXIcon
      text = t("workspace.location.failed", {
        reason: workspaceReason(workspace.error, { device, detail: workspace.errorDetail }),
      })
      break
    default:
      text =
        workspace.waiting === "device_offline"
          ? t("workspace.location.deviceOffline", { device })
          : workspace.waiting === "daemon_outdated"
            ? t("workspace.location.daemonOutdated", { device })
            : t("workspace.location.retry")
  }
  return (
    <span className="flex items-start gap-1.5" data-workspace={workspace.state}>
      <Icon
        className={workspace.state === "failed" ? "mt-px size-3 shrink-0 text-destructive" : "mt-px size-3 shrink-0"}
        aria-hidden
      />
      <span className="min-w-0 [overflow-wrap:anywhere]">{text}</span>
    </span>
  )
}
