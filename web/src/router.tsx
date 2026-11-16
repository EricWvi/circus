import { createBrowserRouter, Navigate } from "react-router"

import { AppShell } from "@/routes/app-shell"
import { AuthGate } from "@/routes/auth-gate"
import { HomePage } from "@/routes/home-page"
import { LoginPage } from "@/routes/login-page"
import { RouteError } from "@/routes/route-error"
import { SessionPage } from "@/routes/session-page"
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
            { path: "/p/:projectId/s/:sessionId", element: <SessionPage /> },
          ],
        },
        { path: "*", element: <Navigate to="/" replace /> },
      ],
    },
  ])
}
