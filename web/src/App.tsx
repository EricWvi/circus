import type { CSSProperties } from "react"
import { createBrowserRouter, Navigate, RouterProvider, useParams } from "react-router"

import { AppSidebar } from "@/components/app-sidebar"
import { SessionView } from "@/components/session/session-view"
import { SidebarInset, SidebarProvider } from "@/components/ui/sidebar"
import { TooltipProvider } from "@/components/ui/tooltip"
import { buildTree, sessionPath } from "@/lib/tree"
import { useSessions } from "@/state/sessions-context"

function FirstSessionRedirect() {
  const { projects, sessions } = useSessions()
  const first = buildTree(projects, sessions).flatMap((n) => n.sessions)[0]
  if (!first) return <p className="p-6 text-sm text-muted-foreground">No sessions yet.</p>
  return <Navigate to={sessionPath(first)} replace />
}

function SessionRoute() {
  const { projectId, sessionId } = useParams()
  const { projects, sessions } = useSessions()
  const session = sessions.find((s) => s.id === sessionId && s.projectId === projectId)
  const project = projects.find((p) => p.id === projectId)

  if (!session || !project) return <Navigate to="/" replace />

  return (
    <SidebarProvider className="h-svh" style={{ "--sidebar-width": "18rem" } as CSSProperties}>
      <AppSidebar activeSessionId={session.id} />
      <SidebarInset className="min-h-0 overflow-hidden">
        <SessionView key={session.id} project={project} session={session} />
      </SidebarInset>
    </SidebarProvider>
  )
}

const router = createBrowserRouter([
  { path: "/", element: <FirstSessionRedirect /> },
  { path: "/p/:projectId/s/:sessionId", element: <SessionRoute /> },
  { path: "*", element: <Navigate to="/" replace /> },
])

export function App() {
  return (
    <TooltipProvider>
      <RouterProvider router={router} />
    </TooltipProvider>
  )
}
