import type { CSSProperties } from "react"
import { Outlet, useMatch } from "react-router"

import { useAuth } from "@/api/hooks"
import { AppSidebar } from "@/components/app-sidebar"
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar"
import { ConversationsProvider } from "@/state/conversations-provider"

/** Sidebar layout around every signed-in page. */
export function AppShell() {
  const sessionId = useMatch("/p/:projectId/s/:sessionId")?.params.sessionId
  const auth = useAuth()
  const username = auth.data?.kind === "signed_in" ? auth.data.user.username : ""

  // Keyed by user, so nothing of the previous user's conversations survives.
  return (
    <ConversationsProvider key={username}>
      <SidebarProvider className="h-svh" style={{ "--sidebar-width": "18rem" } as CSSProperties}>
        <AppSidebar activeSessionId={sessionId} />
        <SidebarInset className="min-h-0 overflow-hidden">
          <Outlet />
        </SidebarInset>
      </SidebarProvider>
    </ConversationsProvider>
  )
}
