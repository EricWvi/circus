import type { ReactNode } from "react"

import { Button } from "@/components/ui/button"
import { SidebarTrigger } from "@/components/ui/sidebar"

/** A main-area page with only the sidebar trigger in its header. */
export function BarePage({ children }: { children?: ReactNode }) {
  return (
    <div className="flex h-full min-h-0 flex-col">
      <header className="flex h-12 shrink-0 items-center border-b px-3">
        <SidebarTrigger />
      </header>
      {children}
    </div>
  )
}

export function LoadError({ onRetry }: { onRetry: () => void }) {
  return (
    <BarePage>
      <div className="flex flex-1 flex-col items-center justify-center gap-4 p-6">
        <p className="text-sm text-muted-foreground">Couldn&apos;t load this page.</p>
        <Button variant="outline" onClick={onRetry}>
          Try again
        </Button>
      </div>
    </BarePage>
  )
}
