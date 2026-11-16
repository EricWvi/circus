import { AuthCard } from "@/components/auth/auth-card"
import { Button } from "@/components/ui/button"

/** Shown instead of react-router's raw error screen when a route throws. */
export function RouteError() {
  return (
    <AuthCard>
      <div className="flex flex-col items-center gap-4">
        <p className="text-sm text-muted-foreground">Something went wrong.</p>
        <Button variant="outline" asChild>
          {/* A full load, so a broken router state can't stick around. */}
          <a href="/">Go home</a>
        </Button>
      </div>
    </AuthCard>
  )
}
