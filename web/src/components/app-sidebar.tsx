import { useState } from "react"
import { Link } from "react-router"
import { ChevronRightIcon, MonitorSmartphoneIcon, PlusIcon, SearchIcon } from "lucide-react"

import { useAuth, useDevices, useProjects, useSessionList } from "@/api/hooks"
import { Brand } from "@/components/brand"
import { StatusDot } from "@/components/status-dot"
import { Button } from "@/components/ui/button"
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { Kbd } from "@/components/ui/kbd"
import {
  Sidebar,
  SidebarContent,
  SidebarFooter,
  SidebarGroup,
  SidebarGroupLabel,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarMenuSkeleton,
  SidebarMenuSub,
  SidebarMenuSubButton,
  SidebarMenuSubItem,
  useSidebar,
} from "@/components/ui/sidebar"
import { rollupState, sessionStateLabels } from "@/lib/status"
import { formatRelative } from "@/lib/time"
import { buildTree, sessionPath, type ProjectNode } from "@/lib/tree"

import { UserMenu } from "./user-menu"

// Hover is half the strength of the selected state.
const rowStates =
  "hover:bg-sidebar-foreground/5 active:bg-sidebar-foreground/5 data-active:bg-sidebar-foreground/10 data-active:hover:bg-sidebar-foreground/10"

export function AppSidebar({ activeSessionId }: { activeSessionId?: string }) {
  const auth = useAuth()
  const projects = useProjects()
  const sessions = useSessionList()
  const devices = useDevices()
  const tree = projects.data && sessions.data ? buildTree(projects.data, sessions.data) : null
  // Only a failed first load shows an error; a failed refetch keeps the list.
  const loadFailed = (projects.isError && !projects.data) || (sessions.isError && !sessions.data)
  const user = auth.data?.kind === "signed_in" ? auth.data.user : null

  return (
    <Sidebar collapsible="offcanvas">
      <SidebarHeader className="gap-3 px-3 pt-3">
        <Brand className="h-7 px-1" />
        <Button
          variant="outline"
          className="h-8 w-full justify-start gap-2 bg-background px-2 font-normal text-muted-foreground"
        >
          <SearchIcon />
          <span className="flex-1 text-left">Search</span>
          <Kbd>⌘K</Kbd>
        </Button>
      </SidebarHeader>

      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>Projects</SidebarGroupLabel>
          <SidebarMenu>
            {tree?.map((node) => (
              <ProjectGroup
                key={node.project.projectId}
                node={node}
                activeSessionId={activeSessionId}
              />
            ))}
            {!tree && !loadFailed && (
              <>
                <SidebarMenuSkeleton />
                <SidebarMenuSkeleton />
                <SidebarMenuSkeleton />
              </>
            )}
            {loadFailed && (
              <li className="px-2 py-1 text-xs text-muted-foreground">Couldn&apos;t load projects</li>
            )}
            <SidebarMenuItem>
              <SidebarMenuButton className={`text-muted-foreground ${rowStates}`}>
                <PlusIcon />
                <span>New project</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter className="border-t">
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton className={rowStates}>
              <MonitorSmartphoneIcon />
              <span className="flex-1">Devices</span>
              {devices.data && (
                <span className="text-xs text-muted-foreground tabular-nums">
                  {devices.data.filter((d) => d.online).length}/{devices.data.length} online
                </span>
              )}
            </SidebarMenuButton>
          </SidebarMenuItem>
          {user && (
            <SidebarMenuItem>
              <UserMenu
                username={user.username}
                className={`h-10 data-[state=open]:bg-sidebar-foreground/10 ${rowStates}`}
              />
            </SidebarMenuItem>
          )}
        </SidebarMenu>
      </SidebarFooter>
    </Sidebar>
  )
}

function ProjectGroup({
  node: { project, sessions },
  activeSessionId,
}: {
  node: ProjectNode
  activeSessionId?: string
}) {
  const [open, setOpen] = useState(true)
  const { isMobile, setOpenMobile } = useSidebar()
  const rollup = rollupState(sessions)

  return (
    <Collapsible asChild open={open} onOpenChange={setOpen} className="group/collapsible">
      <SidebarMenuItem>
        <CollapsibleTrigger asChild>
          <SidebarMenuButton className={rowStates}>
            <ChevronRightIcon className="text-muted-foreground transition-transform group-data-[state=open]/collapsible:rotate-90" />
            <span className="flex-1 truncate">{project.name}</span>
            {!open && rollup && (
              <span className="flex items-center" title={sessionStateLabels[rollup]}>
                <StatusDot state={rollup} />
              </span>
            )}
          </SidebarMenuButton>
        </CollapsibleTrigger>
        <CollapsibleContent>
          <SidebarMenuSub className="mr-0 pr-0">
            {sessions.length === 0 && (
              <li className="px-2 py-1 text-xs text-muted-foreground">No active sessions</li>
            )}
            {sessions.map((session) => (
              <SidebarMenuSubItem key={session.sessionId}>
                <SidebarMenuSubButton
                  asChild
                  isActive={session.sessionId === activeSessionId}
                  className={`h-8 ${rowStates}`}
                >
                  <Link
                    to={sessionPath(session)}
                    onClick={() => isMobile && setOpenMobile(false)}
                    aria-current={session.sessionId === activeSessionId ? "page" : undefined}
                  >
                    <StatusDot state={session.state} />
                    <span className="min-w-0 flex-1 truncate">{session.title}</span>
                    <time
                      dateTime={new Date(session.lastActivityAtUnixMs).toISOString()}
                      className="shrink-0 text-xs text-muted-foreground tabular-nums"
                    >
                      {formatRelative(session.lastActivityAtUnixMs)}
                    </time>
                  </Link>
                </SidebarMenuSubButton>
              </SidebarMenuSubItem>
            ))}
          </SidebarMenuSub>
        </CollapsibleContent>
      </SidebarMenuItem>
    </Collapsible>
  )
}
