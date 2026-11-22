import type { CSSProperties } from "react"
import { Outlet, useMatch } from "react-router"

import { useCurrentUser } from "@/api/hooks"
import { AppSidebar } from "@/components/app-sidebar"
import { NoticeProvider, NoticeStrip } from "@/components/notices"
import { NewProjectDialogProvider } from "@/components/projects/new-project-dialog"
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar"
import { OutboxProvider } from "@/state/outbox-provider"

/** Sidebar layout around every signed-in page. */
export function AppShell() {
  const sessionId = useMatch("/p/:projectId/s/:sessionId")?.params.sessionId
  const projectHome = useMatch("/p/:projectId")?.params.projectId
  const projectSettings = useMatch("/p/:projectId/settings")?.params.projectId
  const projectLedger = useMatch("/p/:projectId/ledger/*")?.params.projectId
  const devicesPage = useMatch("/devices") !== null
  const providersPage = useMatch("/providers") !== null
  const { user } = useCurrentUser()

  // Keyed by user, so nothing of the previous user's messages survives.
  return (
    <OutboxProvider key={user?.userId ?? ""}>
      <NoticeProvider>
        <SidebarProvider className="h-svh" style={{ "--sidebar-width": "18rem" } as CSSProperties}>
          <NewProjectDialogProvider>
            <AppSidebar
              activeSessionId={sessionId}
              activeProjectId={projectHome ?? projectSettings ?? projectLedger}
              ledgerProjectId={projectLedger}
              devicesActive={devicesPage}
              providersActive={providersPage}
            />
            <SidebarInset className="min-h-0 overflow-hidden outline-none">
              <NoticeStrip />
              <div className="min-h-0 flex-1">
                <Outlet />
              </div>
            </SidebarInset>
          </NewProjectDialogProvider>
        </SidebarProvider>
      </NoticeProvider>
    </OutboxProvider>
  )
}
