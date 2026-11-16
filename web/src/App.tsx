import { RouterProvider } from "react-router"

import { TooltipProvider } from "@/components/ui/tooltip"
import type { createAppRouter } from "@/router"

export function App({ router }: { router: ReturnType<typeof createAppRouter> }) {
  return (
    <TooltipProvider>
      <RouterProvider router={router} />
    </TooltipProvider>
  )
}
