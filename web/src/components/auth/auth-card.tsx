import type { ReactNode } from "react"

import { Brand } from "@/components/brand"
import { Card, CardContent, CardHeader } from "@/components/ui/card"

/** The centered card used by the sign-in and setup pages. */
export function AuthCard({ children }: { children: ReactNode }) {
  return (
    <main className="flex min-h-svh items-center justify-center px-4 py-10">
      <Card className="w-full max-w-sm gap-6 py-6">
        <CardHeader className="justify-items-center px-6">
          <Brand className="text-card-foreground" />
        </CardHeader>
        <CardContent className="px-6">{children}</CardContent>
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
