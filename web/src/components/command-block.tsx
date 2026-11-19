import { CheckIcon, CopyIcon } from "lucide-react"
import { useEffect, useRef, useState, type Ref } from "react"
import { useTranslation } from "react-i18next"

import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

/**
 * Shell commands to copy, one per line. Lines too long for the block scroll
 * sideways inside it, never the page.
 */
export function CommandBlock({
  command,
  className,
  copyRef,
}: {
  command: string
  className?: string
  /** The copy button, e.g. to move keyboard focus to it. */
  copyRef?: Ref<HTMLButtonElement>
}) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)
  const timer = useRef(0)
  useEffect(() => () => window.clearTimeout(timer.current), [])

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command)
    } catch {
      return
    }
    setCopied(true)
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => setCopied(false), 2000)
  }

  return (
    <div className={cn("flex min-w-0 items-start border bg-muted/50", className)}>
      <pre className="min-w-0 flex-1 overflow-x-auto px-3 py-2 font-mono">
        <code>{command}</code>
      </pre>
      <Button
        ref={copyRef}
        type="button"
        variant="ghost"
        size="icon-sm"
        className="m-1"
        aria-label={copied ? t("devices.copied") : t("devices.copyCommand")}
        title={copied ? t("devices.copied") : t("devices.copyCommand")}
        onClick={() => void copy()}
      >
        {copied ? <CheckIcon /> : <CopyIcon />}
      </Button>
      <span className="sr-only" aria-live="polite">
        {copied ? t("devices.copied") : ""}
      </span>
    </div>
  )
}
