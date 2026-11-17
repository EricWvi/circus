import { InfoIcon, PlusIcon, XIcon } from "lucide-react"
import { useRef, useState } from "react"
import { useTranslation } from "react-i18next"

import type { Device } from "@/api/types"
import { MAX_LOCATIONS } from "@/api/validation"
import { DisabledReason } from "@/components/disabled-reason"
import { InfoTip } from "@/components/info-tip"
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
import { useIsMobile } from "@/hooks/use-mobile"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { lastInputWasKeyboard } from "@/lib/input-modality"
import {
  addedRow,
  hasEarlierDefault,
  isReplaced,
  osLabel,
  pathPlaceholder,
  shortDefaultPath,
  sortDevices,
  type LocationField,
  type LocationRow,
  type RowErrors,
} from "@/lib/location-form"

/**
 * Classes for a control that is unavailable while the form is busy. It
 * stays enabled (and keeps focus) but ignores pointers and looks disabled;
 * handlers check `busy` themselves.
 */
export const busyClass = "aria-disabled:pointer-events-none aria-disabled:opacity-50"

/**
 * Edits a project's locations: one row per device and optional path. An
 * empty path is the device's default directory, shown as the placeholder
 * and marked as the default. A project may have no locations.
 *
 * While `busy` (saving), the controls are read-only rather than disabled,
 * so keyboard focus stays where it was.
 */
export function LocationEditor({
  rows,
  devices,
  errors,
  busy = false,
  defaultPath,
  defaultPathHint,
  onChange,
  onTouch,
}: {
  rows: readonly LocationRow[]
  /** Undefined while loading. */
  devices: Device[] | undefined
  /** Errors to show now, by row key. */
  errors: Record<string, RowErrors>
  busy?: boolean
  /** The default directory, as the placeholder of an empty path. */
  defaultPath: string
  /** Explains the default directory, next to an empty path. */
  defaultPathHint: string
  onChange: (rows: LocationRow[]) => void
  onTouch: (key: string, field: LocationField) => void
}) {
  const { t } = useTranslation()
  const mobile = useIsMobile()
  const sorted = sortDevices(devices ?? [])
  const update = (key: string, patch: Partial<LocationRow>) =>
    onChange(rows.map((r) => (r.key === key ? { ...r, ...patch } : r)))
  const addBlocked =
    rows.length >= MAX_LOCATIONS
      ? t("projects.maxLocations", { max: MAX_LOCATIONS })
      : devices !== undefined && sorted.length === 0
        ? t("projects.noDevicesToAdd")
        : null
  const defaultPlaceholder = shortDefaultPath(defaultPath, mobile ? { head: 4, tail: 4 } : undefined)

  // A row added on a device that already has its default location needs a
  // path: its field takes focus as it mounts.
  const focusPath = useRef<string | null>(null)

  const add = () => {
    if (busy) return
    const { row, needsPath } = addedRow(rows, sorted)
    if (needsPath) focusPath.current = row.key
    onChange([...rows, row])
  }

  return (
    <div className="flex flex-col gap-3">
      {rows.length === 0 ? (
        <p className="text-sm text-muted-foreground">{t("projects.noLocations")}</p>
      ) : (
        <ul className="flex flex-col gap-4 sm:gap-2">
          {rows.map((row, i) => {
            const rowErrors = errors[row.key] ?? {}
            const n = i + 1
            const device = sorted.find((d) => d.name === row.device)
            // An empty path is the default directory, unless an earlier row
            // already is this device's default: then a path is expected.
            const explicit = hasEarlierDefault(rows, i)
            const isDefault = row.path === "" && !explicit
            const replaced = isReplaced(row, sorted)
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
                    busy={busy}
                    replaced={replaced}
                    invalid={!!rowErrors.device}
                    label={t("projects.deviceForLocation", { n })}
                    onChange={(name) => update(row.key, { device: name, revoked: undefined })}
                    onClose={() => onTouch(row.key, "device")}
                  />
                  {replaced && !rowErrors.device && (
                    // Runs on under the (empty) space below the path field.
                    <p className="text-xs text-muted-foreground sm:w-max sm:max-w-[34rem]">{t("projects.replacedHint")}</p>
                  )}
                  {rowErrors.device && <FieldError>{rowErrors.device}</FieldError>}
                </div>
                <div className="col-span-2 flex min-w-0 flex-col gap-1 sm:col-span-1 sm:col-start-2 sm:row-start-1">
                  <div className="relative">
                    <Input
                      ref={(el) => {
                        if (el && focusPath.current === row.key) {
                          focusPath.current = null
                          el.focus({ focusVisible: lastInputWasKeyboard() } as FocusOptions)
                        }
                      }}
                      value={row.path}
                      placeholder={explicit ? pathPlaceholder(device?.os) : defaultPlaceholder}
                      aria-label={t("projects.pathForLocation", { n })}
                      aria-invalid={rowErrors.path ? true : undefined}
                      aria-busy={busy || undefined}
                      readOnly={busy}
                      autoCapitalize="none"
                      autoComplete="off"
                      spellCheck={false}
                      className={isDefault ? "pr-20" : undefined}
                      onChange={(e) => update(row.key, { path: e.target.value })}
                      onBlur={() => onTouch(row.key, "path")}
                    />
                    {isDefault && <DefaultMark hint={defaultPathHint} />}
                  </div>
                  {rowErrors.path && <FieldError>{rowErrors.path}</FieldError>}
                </div>
                <div className="col-start-2 row-start-1 sm:col-start-3">
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    aria-label={t("projects.removeLocation", { n })}
                    aria-disabled={busy || undefined}
                    className={busyClass}
                    onClick={() => {
                      if (!busy) onChange(rows.filter((r) => r.key !== row.key))
                    }}
                  >
                    <XIcon />
                  </Button>
                </div>
              </li>
            )
          })}
        </ul>
      )}
      <DisabledReason reason={addBlocked}>
        <Button
          type="button"
          variant="outline"
          size="sm"
          className={`w-fit ${busyClass}`}
          aria-disabled={busy || undefined}
          disabled={devices === undefined || addBlocked !== null}
          onClick={add}
        >
          <PlusIcon />
          {t("projects.addLocation")}
        </Button>
      </DisabledReason>
    </div>
  )
}

/** "default" inside an empty path field, explaining the default directory on hover or focus. */
function DefaultMark({ hint }: { hint: string }) {
  const { t } = useTranslation()
  return (
    <InfoTip content={hint} contentClassName="max-w-xs break-all">
      <span
        tabIndex={0}
        className="absolute top-1/2 right-1.5 flex -translate-y-1/2 cursor-default items-center gap-1 rounded-sm px-1 py-0.5 text-xs text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
      >
        {t("projects.defaultPathMark")}
        <InfoIcon className="size-3" aria-hidden />
        <span className="sr-only">{hint}</span>
      </span>
    </InfoTip>
  )
}

function DeviceSelect({
  value,
  devices,
  loading,
  busy,
  replaced,
  invalid,
  label,
  onChange,
  onClose,
}: {
  value: string
  devices: Device[]
  loading: boolean
  /** Read-only: doesn't open or change, but keeps focus. */
  busy: boolean
  /** The saved device was revoked and a new device has its name. */
  replaced: boolean
  invalid: boolean
  label: string
  onChange: (name: string) => void
  onClose: () => void
}) {
  const { t } = useTranslation()
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  const [open, setOpen] = useState(false)
  const selected = devices.find((d) => d.name === value)
  // A saved location's device that is no longer listed was removed; it
  // stays visible, marked, until another device is chosen.
  const missing = !loading && !selected && value !== "" ? value : null
  const empty = !loading && devices.length === 0 && missing === null

  return (
    <Select
      value={value}
      open={open}
      onValueChange={(name) => {
        if (!busy) onChange(name)
      }}
      disabled={loading || empty}
      onOpenChange={(next) => {
        if (next && busy) return
        setOpen(next)
        onOpenChange(next)
        if (!next) onClose()
      }}
    >
      <SelectTrigger
        ref={triggerRef}
        aria-label={label}
        aria-invalid={invalid ? true : undefined}
        aria-disabled={busy || undefined}
        className={`w-full ${busyClass}`}
      >
        <SelectValue placeholder={t(empty ? "projects.noDevices" : "projects.device")}>
          {selected && <DeviceName device={selected} replaced={replaced} />}
          {missing !== null && <RemovedDeviceName name={missing} />}
        </SelectValue>
      </SelectTrigger>
      <SelectContent position="popper" onCloseAutoFocus={onCloseAutoFocus}>
        {missing !== null && (
          <SelectItem value={missing} disabled>
            <RemovedDeviceName name={missing} />
          </SelectItem>
        )}
        {devices.map((d) => (
          <SelectItem key={d.name} value={d.name}>
            <DeviceName device={d} showOs />
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

function DeviceName({ device, showOs, replaced }: { device: Device; showOs?: boolean; replaced?: boolean }) {
  const { t } = useTranslation()
  return (
    <span className="flex min-w-0 items-baseline gap-2">
      <span className="truncate">{device.name}</span>
      {showOs && <span className="shrink-0 text-xs text-muted-foreground">{osLabel(device.os)}</span>}
      {replaced && <span className="shrink-0 text-xs text-amber-700 dark:text-amber-400">{t("projects.replacedMark")}</span>}
      {!replaced && !device.online && (
        <span className="shrink-0 text-xs text-muted-foreground">{t("common.offline")}</span>
      )}
    </span>
  )
}

function RemovedDeviceName({ name }: { name: string }) {
  const { t } = useTranslation()
  return (
    <span className="flex min-w-0 items-baseline gap-2">
      <span className="truncate text-muted-foreground line-through decoration-muted-foreground/60">{name}</span>
      <span className="shrink-0 text-xs text-destructive">
        {t("projects.removedMark")}
      </span>
    </span>
  )
}
