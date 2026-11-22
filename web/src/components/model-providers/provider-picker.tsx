import { KeyRoundIcon, LaptopIcon } from "lucide-react"
import type { ReactNode } from "react"
import { useTranslation } from "react-i18next"

import type { ModelProvider, Project } from "@/api/types"
import { DisabledReason } from "@/components/disabled-reason"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import {
  DEFAULT_CHOICE,
  defaultChoiceLabel,
  initialChoice,
  LOCAL_LOGIN,
  sessionProviderName,
} from "@/lib/provider-choice"
import { cn } from "@/lib/utils"

/** A select trigger that reads as a quiet line of text, like the location picker's. */
const quietTrigger =
  "max-w-full border-transparent bg-transparent px-2 text-xs text-muted-foreground shadow-none hover:bg-muted hover:text-foreground dark:bg-transparent dark:hover:bg-muted/50"

function Choice({ icon, children, className }: { icon: "provider" | "local"; children: ReactNode; className?: string }) {
  const Icon = icon === "local" ? LaptopIcon : KeyRoundIcon
  return (
    <span className={cn("flex min-w-0 items-center gap-2", className)}>
      <Icon className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
      <span className="truncate">{children}</span>
    </span>
  )
}

/**
 * The provider of a new session: the default (the project's, else the
 * user's) when there is one, any provider, or local login. `value` is null
 * until the user picks something.
 */
export function NewSessionProviderPicker({
  project,
  providers,
  value,
  disabled,
  onChange,
}: {
  project: Pick<Project, "defaultProviderId">
  providers: readonly ModelProvider[]
  value: string | null
  disabled: boolean
  onChange: (choice: string) => void
}) {
  const { t } = useTranslation()
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  const current = value ?? initialChoice
  const defaultLabel = defaultChoiceLabel(project, providers)
  const label = (choice: string): ReactNode => {
    if (choice === DEFAULT_CHOICE && defaultLabel) return <Choice icon="provider">{defaultLabel}</Choice>
    if (choice === LOCAL_LOGIN || choice === DEFAULT_CHOICE) {
      return <Choice icon="local">{t("providers.select.localLogin")}</Choice>
    }
    const provider = providers.find((p) => p.providerId === choice)
    return <Choice icon="provider">{provider?.name ?? t("session.provider.deleted")}</Choice>
  }
  return (
    <Select value={current} onValueChange={onChange} onOpenChange={onOpenChange} disabled={disabled}>
      <SelectTrigger ref={triggerRef} size="sm" aria-label={t("providers.select.label")} className={quietTrigger}>
        <SelectValue>{label(current)}</SelectValue>
      </SelectTrigger>
      <SelectContent position="popper" align="start" className="max-w-sm" onCloseAutoFocus={onCloseAutoFocus}>
        {defaultLabel && (
          <SelectItem value={DEFAULT_CHOICE} className="text-xs">
            {label(DEFAULT_CHOICE)}
          </SelectItem>
        )}
        {providers.map((p) => (
          <SelectItem key={p.providerId} value={p.providerId} className="text-xs">
            {label(p.providerId)}
          </SelectItem>
        ))}
        <LocalLoginItem />
      </SelectContent>
    </Select>
  )
}

/** Local login, with what it means. */
function LocalLoginItem() {
  const { t } = useTranslation()
  return (
    <>
      <SelectSeparator />
      <SelectItem value={LOCAL_LOGIN} className="text-xs">
        <Choice icon="local">{t("providers.select.localLogin")}</Choice>
      </SelectItem>
      <p className="px-2 pt-0.5 pb-1.5 text-xs text-muted-foreground">{t("providers.select.localLoginHint")}</p>
    </>
  )
}

/**
 * The session's provider in its header, which the user can change while no
 * turn runs (`disabledReason` says why not otherwise). A stopped session
 * only shows it.
 */
export function SessionProviderPicker({
  providerId,
  providers,
  stopped,
  disabledReason,
  open,
  onOpenChange,
  onChange,
  className,
}: {
  providerId: string
  providers: readonly ModelProvider[] | undefined
  stopped: boolean
  disabledReason: string | null
  open: boolean
  onOpenChange: (open: boolean) => void
  onChange: (providerId: string) => void
  className?: string
}) {
  const { t } = useTranslation()
  const { triggerRef, onOpenChange: overlayOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  const { label, deleted } = sessionProviderName(providerId, providers)
  const content = (
    <Choice icon={providerId === "" ? "local" : "provider"} className={cn(deleted && "text-destructive")}>
      {label}
    </Choice>
  )
  if (label === "") return null
  if (stopped) {
    return (
      <span className={cn("flex min-w-0 px-2 text-xs text-muted-foreground", className)} title={t("providers.select.label")}>
        {content}
      </span>
    )
  }
  const value = providerId === "" ? LOCAL_LOGIN : providerId
  return (
    <DisabledReason reason={disabledReason} className={cn("min-w-0", className)}>
      <Select
        value={value}
        // Never open while a change isn't allowed, even for a moment.
        open={open && disabledReason === null}
        onOpenChange={(next) => {
          overlayOpenChange(next)
          onOpenChange(next)
        }}
        onValueChange={(v) => onChange(v === LOCAL_LOGIN ? "" : v)}
        disabled={disabledReason !== null}
      >
        <SelectTrigger
          ref={triggerRef}
          size="sm"
          aria-label={t("session.provider.change")}
          title={t("session.provider.change")}
          className={quietTrigger}
        >
          <SelectValue>{content}</SelectValue>
        </SelectTrigger>
        <SelectContent position="popper" align="end" className="max-w-sm" onCloseAutoFocus={onCloseAutoFocus}>
          <p className="px-2 py-1.5 text-xs text-muted-foreground">{t("session.provider.changeDescription")}</p>
          {deleted && (
            <SelectItem value={providerId} disabled className="text-xs">
              {content}
            </SelectItem>
          )}
          {(providers ?? []).map((p) => (
            <SelectItem key={p.providerId} value={p.providerId} className="text-xs">
              <Choice icon="provider">{p.name}</Choice>
            </SelectItem>
          ))}
          <LocalLoginItem />
        </SelectContent>
      </Select>
    </DisabledReason>
  )
}
