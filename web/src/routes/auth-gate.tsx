import { Navigate, Outlet, useLocation } from "react-router"

import { useAuth } from "@/api/hooks"
import { AuthCard } from "@/components/auth/auth-card"
import { Button } from "@/components/ui/button"
import { authRedirect } from "@/lib/auth-redirect"

/** Resolves who is signed in and routes to setup, sign-in or the app. */
export function AuthGate() {
  const auth = useAuth()
  const location = useLocation()

  // Render nothing until we know, so the app never flashes before a redirect.
  // A failed refetch keeps the last known state.
  if (auth.data === undefined) {
    if (!auth.isError) return null
    return (
      <AuthCard>
        <div className="flex flex-col items-center gap-4">
          <p className="text-sm text-muted-foreground">Can&apos;t reach the server.</p>
          <Button variant="outline" onClick={() => void auth.refetch()}>
            Try again
          </Button>
        </div>
      </AuthCard>
    )
  }

  const target = authRedirect(auth.data, location)
  if (target) return <Navigate to={target} replace />
  return <Outlet />
}
