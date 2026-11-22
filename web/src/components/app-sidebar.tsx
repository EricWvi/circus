import { useState } from "react"
import { Link } from "react-router"
import { BookOpenIcon, ChevronRightIcon, KeyRoundIcon, MonitorSmartphoneIcon, PlusIcon, SearchIcon } from "lucide-react"
import { useTranslation } from "react-i18next"

import { useCurrentUser, useDevices, useProjects, useSessionList } from "@/api/hooks"
import { Brand } from "@/components/brand"
import { useOpenNewProject } from "@/components/projects/new-project-context"
import { StatusDot, WorkspaceMark } from "@/components/status-dot"
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
  SidebarMenuAction,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarMenuSkeleton,
  SidebarMenuSub,
  SidebarMenuSubButton,
  SidebarMenuSubItem,
  useSidebar,
} from "@/components/ui/sidebar"
import { rollupState } from "@/lib/status"
import { formatRelative } from "@/lib/time"
import { sessionTitle } from "@/lib/title"
import { buildTree, ledgerPath, projectPath, sessionPath, type ProjectNode } from "@/lib/tree"
import { cn } from "@/lib/utils"

import { UserMenu } from "./user-menu"

// Hover is half the strength of the selected state.
const rowStates =
  "hover:bg-sidebar-foreground/5 active:bg-sidebar-foreground/5 data-active:bg-sidebar-foreground/10 data-active:hover:bg-sidebar-foreground/10"

export function AppSidebar({
  activeSessionId,
  activeProjectId,
  devicesActive = false,
  providersActive = false,
  ledgerProjectId,
}: {
  activeSessionId?: string
  /** The project whose own page (home, settings or ledger) is open. */
  activeProjectId?: string
  /** The project whose ledger is open. */
  ledgerProjectId?: string
  /** The devices page is open. */
  devicesActive?: boolean
  /** The providers page is open. */
  providersActive?: boolean
}) {
  const { t } = useTranslation()
  const { user } = useCurrentUser()
  const openNewProject = useOpenNewProject()
  const { isMobile, setOpenMobile } = useSidebar()
  const projects = useProjects()
  const sessions = useSessionList()
  const devices = useDevices()
  const tree = projects.data && sessions.data ? buildTree(projects.data, sessions.data) : null
  // Only a failed first load shows an error; a failed refetch keeps the list.
  const loadFailed = (projects.isError && !projects.data) || (sessions.isError && !sessions.data)

  return (
    <Sidebar collapsible="offcanvas">
      <SidebarHeader className="gap-3 px-3 pt-3">
        <Link
          to="/"
          className="w-fit rounded-md outline-hidden ring-sidebar-ring focus-visible:ring-2"
          onClick={() => isMobile && setOpenMobile(false)}
        >
          <Brand className="h-7 px-1" />
        </Link>
        <Button
          variant="outline"
          className="h-8 w-full justify-start gap-2 bg-background px-2 font-normal text-muted-foreground"
        >
          <SearchIcon />
          <span className="flex-1 text-left">{t("sidebar.search")}</span>
          {/* oxlint-disable-next-line i18next/no-literal-string -- a key glyph, the same in every language */}
          <Kbd>⌘K</Kbd>
        </Button>
      </SidebarHeader>

      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupLabel>{t("sidebar.projects")}</SidebarGroupLabel>
          <SidebarMenu>
            {tree?.map((node) => (
              <ProjectGroup
                key={node.project.projectId}
                node={node}
                activeSessionId={activeSessionId}
                active={node.project.projectId === activeProjectId}
                ledgerActive={node.project.projectId === ledgerProjectId}
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
              <li className="px-2 py-1 text-xs text-muted-foreground">{t("sidebar.projectsLoadFailed")}</li>
            )}
            <SidebarMenuItem>
              <SidebarMenuButton
                className={`text-muted-foreground ${rowStates}`}
                onClick={() => {
                  if (isMobile) setOpenMobile(false)
                  openNewProject()
                }}
              >
                <PlusIcon />
                <span>{t("projects.newProject")}</span>
              </SidebarMenuButton>
            </SidebarMenuItem>
          </SidebarMenu>
        </SidebarGroup>
      </SidebarContent>

      <SidebarFooter className="border-t">
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton asChild isActive={devicesActive} className={rowStates}>
              <Link
                to="/devices"
                aria-current={devicesActive ? "page" : undefined}
                onClick={() => isMobile && setOpenMobile(false)}
              >
                <MonitorSmartphoneIcon />
                <span className="flex-1">{t("sidebar.devices")}</span>
                {devices.data && (
                  <span className="text-xs text-muted-foreground tabular-nums">
                    {t("sidebar.devicesOnline", {
                      online: devices.data.filter((d) => d.online).length,
                      total: devices.data.length,
                    })}
                  </span>
                )}
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
          <SidebarMenuItem>
            <SidebarMenuButton asChild isActive={providersActive} className={rowStates}>
              <Link
                to="/providers"
                aria-current={providersActive ? "page" : undefined}
                onClick={() => isMobile && setOpenMobile(false)}
              >
                <KeyRoundIcon />
                <span className="flex-1">{t("sidebar.providers")}</span>
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
          {user && (
            <SidebarMenuItem>
              <UserMenu
                user={user}
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
  active,
  ledgerActive,
}: {
  node: ProjectNode
  activeSessionId?: string
  active: boolean
  ledgerActive: boolean
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(true)
  const { isMobile, setOpenMobile } = useSidebar()
  const rollup = rollupState(sessions)

  // The name opens the project's page (and expands it); the chevron only
  // toggles, so it sits on top of the row as its own button.
  return (
    <Collapsible asChild open={open} onOpenChange={setOpen} className="group/collapsible">
      <SidebarMenuItem>
        <SidebarMenuButton asChild isActive={active} className={`pl-8 ${rowStates}`}>
          <Link
            to={projectPath(project.projectId)}
            aria-current={active ? "page" : undefined}
            onClick={() => {
              setOpen(true)
              if (isMobile) setOpenMobile(false)
            }}
          >
            <span className="flex-1 truncate">{project.name}</span>
            {!open && rollup && (
              <span className="flex items-center" title={t(`status.${rollup}`)}>
                <StatusDot state={rollup} />
              </span>
            )}
          </Link>
        </SidebarMenuButton>
        {/* The ledger, quietly: shown while the row is hovered or focused (always on touch screens). */}
        <SidebarMenuAction
          asChild
          className={cn(
            `top-1.5 right-1.5 text-muted-foreground hover:text-sidebar-foreground ${rowStates}`,
            !ledgerActive &&
              "md:opacity-0 md:peer-hover/menu-button:opacity-100 md:peer-focus-visible/menu-button:opacity-100 md:hover:opacity-100 md:focus-visible:opacity-100",
            ledgerActive && "bg-sidebar-foreground/10 text-sidebar-foreground",
          )}
        >
          <Link
            to={ledgerPath(project.projectId)}
            aria-label={t("ledger.ledgerOf", { name: project.name })}
            title={t("ledger.title")}
            aria-current={ledgerActive ? "page" : undefined}
            onClick={() => isMobile && setOpenMobile(false)}
          >
            <BookOpenIcon />
          </Link>
        </SidebarMenuAction>
        <CollapsibleTrigger asChild>
          <button
            type="button"
            aria-label={t(open ? "sidebar.collapseProject" : "sidebar.expandProject", {
              name: project.name,
            })}
            className="absolute top-1.5 left-1.5 flex size-5 items-center justify-center rounded-md text-muted-foreground outline-hidden ring-sidebar-ring hover:bg-sidebar-foreground/10 hover:text-sidebar-foreground focus-visible:ring-2"
          >
            <ChevronRightIcon className="size-4 transition-transform group-data-[state=open]/collapsible:rotate-90" />
          </button>
        </CollapsibleTrigger>
        <CollapsibleContent>
          <SidebarMenuSub className="mr-0 pr-0">
            {sessions.length === 0 && (
              <li className="px-2 py-1 text-xs text-muted-foreground">{t("sidebar.noActiveSessions")}</li>
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
                    <span className="min-w-0 flex-1 truncate">{sessionTitle(session.title)}</span>
                    <WorkspaceMark workspace={session.workspace} state={session.state} />
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
