import { useRef, useState, type ReactElement, type ReactNode } from "react"

import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"

/**
 * A tooltip that touch screens can read too. With a mouse it behaves like
 * any tooltip (hover or focus opens it, leaving closes it), except that a
 * click keeps it open instead of closing it. A tap toggles it. The trigger
 * must be focusable.
 */
export function InfoTip({
  children,
  content,
  contentClassName,
  side,
  align,
}: {
  children: ReactElement
  content: ReactNode
  contentClassName?: string
  side?: "top" | "bottom" | "left" | "right"
  align?: "start" | "center" | "end"
}) {
  const [open, setOpen] = useState(false)
  // How the last press on the trigger was made ("" for the keyboard), and
  // whether the tooltip was open then: Radix closes it on pointer down.
  const pointer = useRef("")
  const openAtPress = useRef(false)
  return (
    <Tooltip open={open} onOpenChange={setOpen}>
      <TooltipTrigger
        asChild
        onPointerDown={(event) => {
          pointer.current = event.pointerType
          openAtPress.current = open
        }}
        onPointerLeave={(event) => {
          if (event.pointerType === "mouse") setOpen(false)
        }}
        onClick={(event) => {
          // Keeps Radix from closing the tooltip on the trigger's click.
          event.preventDefault()
          const touch = event.detail > 0 && pointer.current !== "" && pointer.current !== "mouse"
          pointer.current = ""
          setOpen(touch ? !openAtPress.current : true)
        }}
      >
        {children}
      </TooltipTrigger>
      <TooltipContent side={side} align={align} className={contentClassName}>
        {content}
      </TooltipContent>
    </Tooltip>
  )
}
