import { FolderOpenIcon } from "lucide-react"
import { useContext, useMemo, useRef, useState, type ReactNode } from "react"
import { useTranslation } from "react-i18next"

import type { Device } from "@/api/types"
import { DisabledReason } from "@/components/disabled-reason"
import { Button } from "@/components/ui/button"
import { Popover, PopoverAnchor, PopoverContent, PopoverTrigger } from "@/components/ui/popover"
import { Sheet, SheetContent, SheetDescription, SheetHeader, SheetTitle, SheetTrigger } from "@/components/ui/sheet"
import { useIsMobile } from "@/hooks/use-mobile"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { lastInputWasTouch } from "@/lib/input-modality"
import { cn } from "@/lib/utils"

import { FolderBrowser } from "./folder-browser"
import { busyClass } from "./busy"
import { framedAnchor, PickerFrameContext } from "./picker-frame"

/**
 * A path field with a button that browses the device's folders: a popover
 * under (or above) the field, or a sheet from the bottom on phones. Inside
 * a `PickerFrameContext` the popover is centered on that frame instead of
 * lining up with the field. `children` is the field; the button sits inside
 * its right end.
 */
export function FolderPickerField({
  device,
  deviceName,
  path,
  busy,
  onSelect,
  children,
}: {
  /** The row's device, when it is listed. */
  device: Device | undefined
  deviceName: string
  /** The row's path: where browsing starts. */
  path: string
  /** Read-only: doesn't open, but keeps focus. */
  busy: boolean
  onSelect: (path: string) => void
  children: ReactNode
}) {
  const { t } = useTranslation()
  const mobile = useIsMobile()
  const [open, setOpen] = useState(false)
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  const inputRef = useRef<HTMLInputElement>(null)
  const frame = useContext(PickerFrameContext)
  const [fieldElement, setFieldElement] = useState<HTMLDivElement | null>(null)
  const virtualRef = useMemo(
    () => (frame && fieldElement ? { current: framedAnchor(frame, fieldElement) } : null),
    [frame, fieldElement],
  )

  const reason = !device
    ? t("folders.chooseDevice")
    : !device.online
      ? t("folders.deviceOffline", { device: device.name })
      : null

  const change = (next: boolean) => {
    if (next && (busy || reason !== null)) return
    onOpenChange(next)
    setOpen(next)
  }

  // A touch opening leaves the input unfocused, so no keyboard covers the list.
  const onOpenAutoFocus = (event: Event) => {
    event.preventDefault()
    if (!lastInputWasTouch()) inputRef.current?.focus()
  }

  const button = (
    <Button
      ref={triggerRef}
      type="button"
      variant="ghost"
      size="icon-xs"
      aria-label={t("folders.browse", { device: deviceName })}
      aria-disabled={busy || undefined}
      disabled={reason !== null}
      className={busyClass}
    >
      <FolderOpenIcon />
    </Button>
  )
  const trigger = (
    <DisabledReason reason={reason} className="absolute top-1/2 right-1 -translate-y-1/2">
      {mobile ? <SheetTrigger asChild>{button}</SheetTrigger> : <PopoverTrigger asChild>{button}</PopoverTrigger>}
    </DisabledReason>
  )

  const browser = device && (
    <FolderBrowser
      device={device.name}
      os={device.os}
      startPath={path}
      layout={mobile ? "sheet" : "popover"}
      inputRef={inputRef}
      onSelect={(selected) => {
        onSelect(selected)
        change(false)
      }}
      onCancel={() => change(false)}
    />
  )

  if (mobile) {
    return (
      <Sheet open={open} onOpenChange={change}>
        <div className="relative">
          {children}
          {trigger}
        </div>
        <SheetContent
          side="bottom"
          className="gap-0 data-[side=bottom]:h-[85dvh]"
          onOpenAutoFocus={onOpenAutoFocus}
          onCloseAutoFocus={onCloseAutoFocus}
        >
          <SheetHeader className="pr-12">
            <SheetTitle className="truncate">{t("folders.title", { device: deviceName })}</SheetTitle>
            <SheetDescription className="sr-only">{t("folders.browse", { device: deviceName })}</SheetDescription>
          </SheetHeader>
          {browser}
        </SheetContent>
      </Sheet>
    )
  }

  const field = (
    <div ref={setFieldElement} className="relative">
      {children}
      {trigger}
    </div>
  )

  return (
    <Popover open={open} onOpenChange={change}>
      {frame ? (
        <>
          {virtualRef && <PopoverAnchor virtualRef={virtualRef} />}
          {field}
        </>
      ) : (
        <PopoverAnchor asChild>{field}</PopoverAnchor>
      )}
      <PopoverContent
        align={frame ? "center" : "start"}
        collisionPadding={16}
        aria-label={t("folders.title", { device: deviceName })}
        className={cn(
          "max-h-(--radix-popover-content-available-height) max-w-[calc(100vw-2rem)] gap-0 overflow-hidden p-0",
          // Framed, the anchor is the frame: never wider than it.
          frame
            ? "w-[min(28rem,var(--radix-popover-trigger-width))]"
            : "w-[max(28rem,var(--radix-popover-trigger-width))]",
        )}
        onOpenAutoFocus={onOpenAutoFocus}
        onCloseAutoFocus={onCloseAutoFocus}
      >
        {browser}
      </PopoverContent>
    </Popover>
  )
}
