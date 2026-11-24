import { ChevronDownIcon, MessageSquareShareIcon } from "lucide-react"
import { useLayoutEffect, useRef, useState } from "react"

import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible"
import { cn } from "@/lib/utils"

/**
 * The text of an opened note. Where it is too long for its box and
 * scrolls, the box takes focus, so the keyboard can scroll it too.
 */
export function NoteText({ text, label, className }: { text: string; label: string; className?: string }) {
  const ref = useRef<HTMLDivElement>(null)
  const [scrolls, setScrolls] = useState(false)
  useLayoutEffect(() => {
    const box = ref.current
    if (!box) return
    const measure = () => setScrolls(box.scrollHeight > box.clientHeight + 1)
    measure()
    if (typeof ResizeObserver === "undefined") return
    const observer = new ResizeObserver(measure)
    observer.observe(box)
    return () => observer.disconnect()
  }, [])
  return (
    <div
      ref={ref}
      tabIndex={scrolls ? 0 : undefined}
      role={scrolls ? "region" : undefined}
      aria-label={scrolls ? label : undefined}
      className={cn(
        "mt-2 max-h-64 overflow-y-auto rounded-md bg-muted/50 px-3 py-2 font-mono text-xs whitespace-pre-wrap text-muted-foreground outline-none [overflow-wrap:anywhere] focus-visible:ring-2 focus-visible:ring-ring/50",
        className,
      )}
    >
      {text}
    </div>
  )
}

/**
 * Something Claude was told: one quiet row, collapsed, that opens to the
 * exact text Claude received. Without text it is just the row.
 */
export function NoteDisclosure({ label, text }: { label: string; text: string }) {
  const row = "flex max-w-full items-center gap-1.5 text-left text-xs text-muted-foreground"
  const icon = <MessageSquareShareIcon className="size-3 shrink-0" aria-hidden />
  if (text === "") {
    return (
      <p className={row}>
        {icon}
        <span className="min-w-0">{label}</span>
      </p>
    )
  }
  return (
    <Collapsible className="group/note">
      <CollapsibleTrigger
        className={`${row} rounded-sm outline-none transition-colors hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50`}
      >
        {icon}
        <span className="min-w-0">{label}</span>
        <ChevronDownIcon
          className="size-3 shrink-0 transition-transform group-data-[state=open]/note:rotate-180"
          aria-hidden
        />
      </CollapsibleTrigger>
      <CollapsibleContent className="data-[state=closed]:fade-out-0 data-[state=closed]:slide-out-to-top-2 data-[state=open]:slide-in-from-top-2 outline-none data-[state=closed]:animate-out data-[state=open]:animate-in">
        <NoteText text={text} label={label} />
      </CollapsibleContent>
    </Collapsible>
  )
}
