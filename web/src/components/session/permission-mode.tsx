import { ShieldAlertIcon, ShieldIcon } from "lucide-react"
import { useTranslation } from "react-i18next"

import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { isPermissionMode, PERMISSION_MODES, type PermissionMode } from "@/lib/permission-mode"
import { cn } from "@/lib/utils"

/** Bypassing every check is the one mode worth a warning color. */
function ModeIcon({ mode, className }: { mode: string; className?: string }) {
  const Icon = mode === "bypassPermissions" ? ShieldAlertIcon : ShieldIcon
  return (
    <Icon
      className={cn("size-3.5 shrink-0", mode === "bypassPermissions" ? "text-destructive" : "text-muted-foreground", className)}
      aria-hidden
    />
  )
}

/** A mode's name; one this client doesn't know reads as the server names it. */
function useModeName() {
  const { t } = useTranslation()
  return (mode: string) => (isPermissionMode(mode) ? t(`permissionMode.modes.${mode}.name`) : mode)
}

/**
 * The permission mode of a new session, beside its location and provider:
 * each mode with what it means.
 */
export function NewSessionPermissionPicker({
  value,
  disabled,
  onChange,
}: {
  value: PermissionMode
  disabled: boolean
  onChange: (mode: PermissionMode) => void
}) {
  const { t } = useTranslation()
  const name = useModeName()
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  return (
    <Select
      value={value}
      onValueChange={(v) => {
        if (isPermissionMode(v)) onChange(v)
      }}
      onOpenChange={onOpenChange}
      disabled={disabled}
    >
      <SelectTrigger
        ref={triggerRef}
        size="sm"
        aria-label={t("permissionMode.choose")}
        title={t("permissionMode.label")}
        data-permission-mode={value}
        className="max-w-full border-transparent bg-transparent px-2 text-xs text-muted-foreground shadow-none hover:bg-muted hover:text-foreground dark:bg-transparent dark:hover:bg-muted/50"
      >
        <SelectValue>
          <span className="flex min-w-0 items-center gap-2">
            <ModeIcon mode={value} />
            <span className="truncate">{name(value)}</span>
          </span>
        </SelectValue>
      </SelectTrigger>
      <SelectContent position="popper" align="start" className="max-w-sm" onCloseAutoFocus={onCloseAutoFocus}>
        <p className="px-2 py-1.5 text-xs text-muted-foreground">{t("permissionMode.label")}</p>
        {PERMISSION_MODES.map((mode) => (
          <SelectItem key={mode} value={mode} className="items-start text-xs" data-permission-mode={mode}>
            <ModeIcon mode={mode} className="mt-0.5 self-start" />
            <span className="flex min-w-0 flex-col gap-0.5">
              <span className="font-medium">{name(mode)}</span>
              <span className="text-muted-foreground">{t(`permissionMode.modes.${mode}.description`)}</span>
            </span>
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

/**
 * A session's permission mode where its configuration shows. It is the
 * mode the session was created with, which the worker starts Claude Code
 * in; Claude can leave it during a session (ExitPlanMode, say), and no
 * event tells the view, so it is labelled as the start mode. What the mode
 * means, and that, show on hover and to assistive technology.
 */
export function SessionPermissionMode({ mode, className }: { mode: string; className?: string }) {
  const { t } = useTranslation()
  const name = useModeName()
  const label = t("permissionMode.startModeLabel", { mode: name(mode) })
  const details = [
    label,
    ...(isPermissionMode(mode) ? [t(`permissionMode.modes.${mode}.description`)] : []),
    t("permissionMode.startModeNote"),
  ]
  return (
    <span
      className={cn("flex shrink-0 items-center gap-1.5 text-xs text-muted-foreground", className)}
      title={details.join("\n")}
      data-session-permission-mode={mode}
    >
      <ModeIcon mode={mode} className="size-3" />
      <span aria-hidden>{t("permissionMode.startMode", { mode: name(mode) })}</span>
      <span className="sr-only">{details.join(" ")}</span>
    </span>
  )
}
