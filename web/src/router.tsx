import { createBrowserRouter, Navigate } from "react-router"

import { AppShell } from "@/routes/app-shell"
import { AuthGate } from "@/routes/auth-gate"
import { DevicesPage } from "@/routes/devices-page"
import { HomePage } from "@/routes/home-page"
import { LedgerPage } from "@/routes/ledger-page"
import { LoginPage } from "@/routes/login-page"
import { ProjectPage } from "@/routes/project-page"
import { ProjectSettingsPage } from "@/routes/project-settings-page"
import { RouteError } from "@/routes/route-error"
import { SessionPage } from "@/routes/session-page"
import { SettingsPage } from "@/routes/settings-page"
import { SetupPage } from "@/routes/setup-page"

export function createAppRouter() {
  return createBrowserRouter([
    {
      element: <AuthGate />,
      errorElement: <RouteError />,
      children: [
        { path: "/login", element: <LoginPage /> },
        { path: "/setup", element: <SetupPage /> },
        {
          element: <AppShell />,
          children: [
            { path: "/", element: <HomePage /> },
            { path: "/p/:projectId", element: <ProjectPage /> },
            { path: "/p/:projectId/settings", element: <ProjectSettingsPage /> },
            { path: "/p/:projectId/ledger", element: <LedgerPage /> },
            { path: "/p/:projectId/ledger/:decision", element: <LedgerPage /> },
            { path: "/p/:projectId/s/:sessionId", element: <SessionPage /> },
            { path: "/devices", element: <DevicesPage /> },
            { path: "/settings", element: <SettingsPage /> },
          ],
        },
        { path: "*", element: <Navigate to="/" replace /> },
      ],
    },
  ])
}
