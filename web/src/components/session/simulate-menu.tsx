import { useQueryClient } from "@tanstack/react-query"
import { FlaskConicalIcon } from "lucide-react"
import { useSyncExternalStore, type ReactNode } from "react"
import { useTranslation } from "react-i18next"

import { queryKeys } from "@/api/query-client"
import type { DeviceSimulation } from "@/api/mock/backend"
import type { SessionSummary } from "@/api/types"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { useMenuFocus } from "@/hooks/use-menu-focus"
import {
  useConversations,
  useConversationSimulation,
  type ConversationSimulation,
} from "@/state/conversations-context"
import { useDeviceSimulation } from "@/state/device-simulation-context"

/**
 * Development-only switches for the simulated session states. Renders
 * nothing in production builds or where sessions aren't simulated.
 */
export function SimulateMenu({ session }: { session: SessionSummary }) {
  const conversations = useConversationSimulation()
  const devices = useDeviceSimulation()
  if (!import.meta.env.DEV || !conversations || !devices) return null
  return <Menu session={session} conversations={conversations} devices={devices} />
}

function Menu({
  session,
  conversations,
  devices,
}: {
  session: SessionSummary
  conversations: ConversationSimulation
  devices: DeviceSimulation
}) {
  const { t } = useTranslation()
  const focus = useMenuFocus()
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild {...focus.trigger}>
        <Button
          variant="outline"
          size="sm"
          className="h-7 shrink-0 gap-1.5 px-2 text-xs"
          aria-label={t("simulate.menu")}
        >
          <FlaskConicalIcon className="size-3.5" />
          {/* Icon only on narrow screens, where the title needs the room. */}
          <span className="hidden sm:inline">{t("simulate.menu")}</span>
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-auto min-w-52" {...focus.content}>
        <MenuItems session={session} conversations={conversations} devices={devices} />
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/** The menu's items, following the server's state of the session. */
function MenuItems({
  session,
  conversations,
  devices,
}: {
  session: SessionSummary
  conversations: ConversationSimulation
  devices: DeviceSimulation
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const { connection } = useConversations()
  const offline = useSyncExternalStore(devices.subscribe, devices.offlineDevices).has(session.device)

  // The server's state, not the view's: while disconnected the view is
  // frozen. Followed while the menu is open (a turn may end meanwhile).
  const state =
    useSyncExternalStore(
      (onChange) => queryClient.getQueryCache().subscribe(onChange),
      () => conversations.liveState(session.sessionId),
    ) ?? session.state
  const dropReason = connection.kind === "connected" ? null : t("simulate.alreadyDisconnected")
  const killReason = state === "running" ? null : t("simulate.noRunningTurn")
  const breakReason = state === "idle" || state === "interrupted" ? null : t("simulate.cannotBreak")

  return (
    <>
      <Item reason={dropReason} onSelect={conversations.dropConnection}>
        {t("simulate.dropConnection")}
      </Item>
      <Item reason={killReason} onSelect={() => conversations.killWorker(session.sessionId)}>
        {t("simulate.killWorker")}
      </Item>
      <Item reason={breakReason} onSelect={() => conversations.breakTranscript(session.sessionId)}>
        {t("simulate.breakTranscript")}
      </Item>
      <DropdownMenuSeparator />
      <DropdownMenuCheckboxItem
        checked={offline}
        onCheckedChange={(checked) => {
          devices.setDeviceOffline(session.device, checked)
          void queryClient.invalidateQueries({ queryKey: queryKeys.devices })
        }}
      >
        {t("simulate.deviceOffline")}
      </DropdownMenuCheckboxItem>
    </>
  )
}

/**
 * A menu item that says why it is disabled on hover. A disabled item takes
 * no pointer events, so the wrapper around it carries the tooltip.
 */
function Item({
  reason,
  onSelect,
  children,
}: {
  reason: string | null
  onSelect: () => void
  children: ReactNode
}) {
  const item = (
    <DropdownMenuItem disabled={reason !== null} onSelect={onSelect}>
      {children}
    </DropdownMenuItem>
  )
  if (!reason) return item
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <div>{item}</div>
      </TooltipTrigger>
      <TooltipContent side="left">{reason}</TooltipContent>
    </Tooltip>
  )
}
