import { useLayoutEffect, useRef, useState, type ReactElement } from "react"

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { lastInputWasKeyboard } from "@/lib/input-modality"
import { cn } from "@/lib/utils"

/**
 * Explains why a control is disabled. Disabled buttons get no pointer or
 * focus events, so a focusable wrapper carries the tooltip. The wrapper
 * stays in place when the reason clears, so the control isn't remounted.
 * Focus follows the state: when a reason appears while the control has
 * focus, focus moves to the wrapper (a disabled control would drop it);
 * when the reason clears while the wrapper has focus, it moves back to the
 * control.
 */
export function DisabledReason({
  reason,
  children,
  className,
}: {
  /** Null when the control is enabled; the wrapper then neither focuses nor explains. */
  reason: string | null
  children: ReactElement
  className?: string
}) {
  const [open, setOpen] = useState(false)
  const wrapper = useRef<HTMLSpanElement>(null)

  // Before the browser drops focus from a control that just became disabled.
  useLayoutEffect(() => {
    const element = wrapper.current
    const active = document.activeElement
    if (!element || !(active instanceof HTMLElement)) return
    if (reason && active !== element && element.contains(active)) {
      element.focus({ focusVisible: lastInputWasKeyboard() } as FocusOptions)
    } else if (!reason && active === element) {
      element.querySelector<HTMLElement>("button, a, input, textarea, select, [tabindex]")?.focus()
    }
  }, [reason])

  return (
    <Tooltip open={reason !== null && open} onOpenChange={setOpen}>
      <TooltipTrigger asChild>
        <span
          ref={wrapper}
          // -1 without a reason: still focusable, so a browser doesn't blur the
          // focused wrapper before the effect below hands focus to the control.
          tabIndex={reason ? 0 : -1}
          aria-label={reason ?? undefined}
          className={cn(
            "inline-flex w-fit rounded-lg outline-none focus-visible:ring-3 focus-visible:ring-ring/50",
            className,
          )}
        >
          {children}
        </span>
      </TooltipTrigger>
      {reason && <TooltipContent>{reason}</TooltipContent>}
    </Tooltip>
  )
}
