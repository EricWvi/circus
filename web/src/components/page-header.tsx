import type { ReactNode } from "react"

import { Separator } from "@/components/ui/separator"
import { SidebarTrigger } from "@/components/ui/sidebar"

/** The bar at the top of every main-area page. */
export function PageHeader({ children, actions }: { children: ReactNode; actions?: ReactNode }) {
  return (
    <header className="flex h-12 shrink-0 items-center gap-2 border-b px-3">
      <SidebarTrigger />
      <Separator orientation="vertical" className="mr-1 data-vertical:h-4 data-vertical:self-center" />
      <div className="flex min-w-0 flex-1 items-center gap-2 text-sm">{children}</div>
      {actions}
    </header>
  )
}
