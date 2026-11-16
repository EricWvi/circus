import { KeyRoundIcon } from "lucide-react"
import type { ReactNode } from "react"

import { Brand } from "@/components/brand"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader } from "@/components/ui/card"
import { Spinner } from "@/components/ui/spinner"

/** The centered card used by the sign-in and setup pages. */
export function AuthCard({ children }: { children: ReactNode }) {
  return (
    <main className="flex min-h-svh items-center justify-center px-4 py-10">
      <Card className="w-full max-w-sm gap-6 py-6">
        <CardHeader className="justify-items-center px-6">
          <Brand className="text-card-foreground" />
        </CardHeader>
        <CardContent className="flex flex-col gap-5 px-6">{children}</CardContent>
      </Card>
    </main>
  )
}

/** A one-line form error, announced to screen readers. */
export function FormError({ children }: { children?: ReactNode }) {
  if (!children) return null
  return (
    <p role="alert" className="text-sm text-destructive">
      {children}
    </p>
  )
}

/** A short centered message in place of a form. */
export function AuthMessage({ children }: { children: ReactNode }) {
  return <div className="flex flex-col gap-1 text-center text-sm text-muted-foreground">{children}</div>
}

/** Separates the password form from the other ways to sign in. */
export function OrDivider() {
  return (
    <div className="flex items-center gap-3 text-xs text-muted-foreground">
      <span className="h-px flex-1 bg-border" aria-hidden />
      or
      <span className="h-px flex-1 bg-border" aria-hidden />
    </div>
  )
}

/** "Continue with <provider>" for the configured OIDC provider. */
export function OidcButton({
  displayName,
  pending,
  disabled,
  onClick,
}: {
  displayName: string
  /** This flow is starting. */
  pending: boolean
  /** Another way of signing in is in progress. */
  disabled: boolean
  onClick: () => void
}) {
  return (
    <Button
      type="button"
      variant="outline"
      className="w-full"
      disabled={pending || disabled}
      onClick={onClick}
    >
      {pending ? <Spinner aria-label={`Continuing with ${displayName}`} /> : <KeyRoundIcon />}
      Continue with {displayName}
    </Button>
  )
}
