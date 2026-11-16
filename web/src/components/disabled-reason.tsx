import type { ReactElement } from "react"

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { cn } from "@/lib/utils"

/**
 * Explains why a control is disabled. Disabled buttons get no pointer or
 * focus events, so a focusable wrapper carries the tooltip.
 */
export function DisabledReason({
  reason,
  children,
  className,
}: {
  /** Null when the control is enabled; the wrapper is then left out. */
  reason: string | null
  children: ReactElement
  className?: string
}) {
  if (!reason) return children
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          tabIndex={0}
          aria-label={reason}
          className={cn(
            "inline-flex w-fit rounded-lg outline-none focus-visible:ring-3 focus-visible:ring-ring/50",
            className,
          )}
        >
          {children}
        </span>
      </TooltipTrigger>
      <TooltipContent>{reason}</TooltipContent>
    </Tooltip>
  )
}
