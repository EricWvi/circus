import { useTranslation } from "react-i18next"

import type { SessionState } from "@/lib/types"
import { cn } from "@/lib/utils"

// The mono theme is greyscale, so states differ by shape, fill and motion.
// Only "broken" uses a hue (the theme's destructive color).
function Glyph({ state }: { state: SessionState }) {
  switch (state) {
    case "running":
      return (
        <>
          <span className="absolute inset-0 animate-ping rounded-full bg-foreground/40 motion-reduce:animate-none" />
          <span className="relative size-2 rounded-full bg-foreground" />
        </>
      )
    case "awaiting_approval":
      return (
        <span className="size-[7px] rotate-45 bg-foreground ring-[3px] ring-foreground/20" />
      )
    case "queued":
      return (
        <span className="flex size-2 items-center justify-center rounded-full border border-muted-foreground">
          <span className="size-[3px] rounded-full bg-muted-foreground" />
        </span>
      )
    case "idle":
      return <span className="size-2 rounded-full border border-muted-foreground/70" />
    case "interrupted":
      return (
        <span className="size-2 rounded-full border border-muted-foreground bg-[linear-gradient(90deg,var(--color-muted-foreground)_50%,transparent_50%)]" />
      )
    case "broken":
      return <span className="size-2 rounded-full bg-destructive" />
    case "stopped":
      return <span className="size-[7px] bg-muted-foreground/60" />
  }
}

export function StatusDot({
  state,
  className,
  label = true,
}: {
  state: SessionState
  className?: string
  /** Expose the state name to assistive technology. */
  label?: boolean
}) {
  const { t } = useTranslation()
  return (
    <span
      className={cn("relative inline-flex size-2.5 shrink-0 items-center justify-center", className)}
      role={label ? "img" : undefined}
      aria-label={label ? t(`status.${state}`) : undefined}
      aria-hidden={label ? undefined : true}
      title={label ? t(`status.${state}`) : undefined}
      data-state={state}
    >
      <Glyph state={state} />
    </span>
  )
}

export function StatusBadge({ state }: { state: SessionState }) {
  const { t } = useTranslation()
  return (
    <span
      className={cn(
        "inline-flex h-6 shrink-0 items-center gap-1.5 border px-2 text-xs whitespace-nowrap",
        state === "broken"
          ? "border-destructive/40 text-destructive"
          : "text-muted-foreground",
        state === "awaiting_approval" && "border-foreground text-foreground",
      )}
    >
      <StatusDot state={state} label={false} />
      {t(`status.${state}`)}
    </span>
  )
}
