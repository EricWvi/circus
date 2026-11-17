import { PlusIcon, XIcon } from "lucide-react"
import { useTranslation } from "react-i18next"

import type { Device } from "@/api/types"
import { DisabledReason } from "@/components/disabled-reason"
import { Button } from "@/components/ui/button"
import { FieldError } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import {
  newRow,
  osLabel,
  pathPlaceholder,
  sortDevices,
  type LocationField,
  type LocationRow,
  type RowErrors,
} from "@/lib/location-form"

/**
 * Edits a project's locations: one row per device and absolute path. There
 * is always at least one row.
 */
export function LocationEditor({
  rows,
  devices,
  errors,
  disabled,
  onChange,
  onTouch,
}: {
  rows: LocationRow[]
  /** Undefined while loading. */
  devices: Device[] | undefined
  /** Errors to show now, by row key. */
  errors: Record<string, RowErrors>
  disabled?: boolean
  onChange: (rows: LocationRow[]) => void
  onTouch: (key: string, field: LocationField) => void
}) {
  const { t } = useTranslation()
  const sorted = sortDevices(devices ?? [])
  const update = (key: string, patch: Partial<LocationRow>) =>
    onChange(rows.map((r) => (r.key === key ? { ...r, ...patch } : r)))

  return (
    <div className="flex flex-col gap-3">
      <ul className="flex flex-col gap-4 sm:gap-2">
        {rows.map((row, i) => {
          const device = sorted.find((d) => d.name === row.device)
          const rowErrors = errors[row.key] ?? {}
          const n = i + 1
          return (
            <li
              key={row.key}
              className="grid grid-cols-[minmax(0,1fr)_auto] gap-x-2 gap-y-1.5 sm:grid-cols-[11rem_minmax(0,1fr)_auto]"
            >
              <div className="flex min-w-0 flex-col gap-1">
                <DeviceSelect
                  value={row.device}
                  devices={sorted}
                  loading={devices === undefined}
                  disabled={disabled}
                  invalid={!!rowErrors.device}
                  label={t("projects.deviceForLocation", { n })}
                  onChange={(name) => update(row.key, { device: name })}
                  onClose={() => onTouch(row.key, "device")}
                />
                {rowErrors.device && <FieldError>{rowErrors.device}</FieldError>}
              </div>
              <div className="col-span-2 flex min-w-0 flex-col gap-1 sm:col-span-1 sm:col-start-2 sm:row-start-1">
                <Input
                  value={row.path}
                  placeholder={pathPlaceholder(device?.os)}
                  aria-label={t("projects.pathForLocation", { n })}
                  aria-invalid={rowErrors.path ? true : undefined}
                  autoCapitalize="none"
                  autoComplete="off"
                  spellCheck={false}
                  disabled={disabled}
                  onChange={(e) => update(row.key, { path: e.target.value })}
                  onBlur={() => onTouch(row.key, "path")}
                />
                {rowErrors.path && <FieldError>{rowErrors.path}</FieldError>}
              </div>
              <div className="col-start-2 row-start-1 sm:col-start-3">
                <DisabledReason
                  reason={rows.length === 1 ? t("projects.needsOneLocation") : null}
                >
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    aria-label={t("projects.removeLocation", { n })}
                    disabled={disabled || rows.length === 1}
                    onClick={() => onChange(rows.filter((r) => r.key !== row.key))}
                  >
                    <XIcon />
                  </Button>
                </DisabledReason>
              </div>
            </li>
          )
        })}
      </ul>
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="w-fit"
        disabled={disabled || sorted.length === 0}
        onClick={() => onChange([...rows, newRow(sorted)])}
      >
        <PlusIcon />
        {t("projects.addLocation")}
      </Button>
    </div>
  )
}

function DeviceSelect({
  value,
  devices,
  loading,
  disabled,
  invalid,
  label,
  onChange,
  onClose,
}: {
  value: string
  devices: Device[]
  loading: boolean
  disabled?: boolean
  invalid: boolean
  label: string
  onChange: (name: string) => void
  onClose: () => void
}) {
  const { t } = useTranslation()
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  const selected = devices.find((d) => d.name === value)
  const empty = !loading && devices.length === 0

  return (
    <Select
      value={value}
      onValueChange={onChange}
      disabled={disabled || loading || empty}
      onOpenChange={(open) => {
        onOpenChange(open)
        if (!open) onClose()
      }}
    >
      <SelectTrigger
        ref={triggerRef}
        aria-label={label}
        aria-invalid={invalid ? true : undefined}
        className="w-full"
      >
        <SelectValue placeholder={t(empty ? "projects.noDevices" : "projects.device")}>
          {selected && <DeviceName device={selected} />}
        </SelectValue>
      </SelectTrigger>
      <SelectContent position="popper" onCloseAutoFocus={onCloseAutoFocus}>
        {devices.map((d) => (
          <SelectItem key={d.name} value={d.name}>
            <DeviceName device={d} showOs />
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

function DeviceName({ device, showOs }: { device: Device; showOs?: boolean }) {
  const { t } = useTranslation()
  return (
    <span className="flex min-w-0 items-baseline gap-2">
      <span className="truncate">{device.name}</span>
      {showOs && <span className="shrink-0 text-xs text-muted-foreground">{osLabel(device.os)}</span>}
      {!device.online && <span className="shrink-0 text-xs text-muted-foreground">{t("common.offline")}</span>}
    </span>
  )
}
