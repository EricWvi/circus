import { CircleCheckIcon, TimerOffIcon } from "lucide-react"
import { useCallback, useEffect, useRef, useState, type ReactNode, type Ref } from "react"
import { useTranslation } from "react-i18next"

import { useCreateEnrollLink, useDevices, useFetchDevices } from "@/api/hooks"
import type { EnrollLink, EnrollRole } from "@/api/types"
import { CommandBlock } from "@/components/command-block"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogClose,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog"
import { Field, FieldLabel } from "@/components/ui/field"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import {
  detectArchitecture,
  detectPlatform,
  enrollCommand,
  enrollLinkErrorMessage,
  isPlatform,
  type HighEntropyHints,
  type NavigatorHints,
  isReleaseVersion,
  platforms,
  type Platform,
} from "@/lib/enroll"
import { lastInputWasKeyboard } from "@/lib/input-modality"
import { formatTimeUntil } from "@/lib/time"
import { cn } from "@/lib/utils"

const roles: readonly EnrollRole[] = ["daemon", "worker", "both"]

/**
 * Adds a machine for the signed-in user: pick a role and a platform,
 * generate a one-time link, and run the command it makes on the machine.
 * The link lives only in this dialog's state; closing it discards the link.
 */
export function AddDeviceDialog({
  open,
  onOpenChange,
  onCloseAutoFocus,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onCloseAutoFocus?: (event: Event) => void
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="max-h-[calc(100dvh-2rem)] gap-5 overflow-y-auto *:min-w-0 sm:max-w-xl"
        onCloseAutoFocus={onCloseAutoFocus}
      >
        <AddDevice />
      </DialogContent>
    </Dialog>
  )
}

/** The dialog's contents. Mounted with the dialog, so every opening starts over. */
function AddDevice() {
  const { t } = useTranslation()
  const devices = useDevices({ live: true })
  const fetchDevices = useFetchDevices()
  const create = useCreateEnrollLink()
  const [role, setRole] = useState<EnrollRole>("daemon")
  const [platform, setPlatform] = usePlatform()
  const [expired, setExpired] = useState(false)
  // Fetching the device list before a link is issued.
  const [preparing, setPreparing] = useState(false)
  // A click arriving before the busy state renders must not issue another link.
  const generating = useRef(false)
  // The devices listed when the link was generated and still listed since;
  // a name missing from them joined with the link. A device revoked and
  // enrolled again under the same name leaves the list in between, so it
  // counts as new. Null until the first list fetched after generating.
  const [known, setKnown] = useState<ReadonlySet<string> | null>(null)
  const [generatedAt, setGeneratedAt] = useState(0)
  const [watching, setWatching] = useState(false)
  const [joinedName, setJoinedName] = useState<string | null>(null)

  const listed = devices.data
  if (watching && listed) {
    const names = new Set(listed.map((d) => d.name))
    // The cache may hold a list from before the link (a failed fetch keeps it).
    if (known === null) {
      if (devices.dataUpdatedAt >= generatedAt) setKnown(names)
    } else if ([...known].some((n) => !names.has(n))) setKnown(new Set([...known].filter((n) => names.has(n))))
  }
  const newcomer = known && listed?.find((d) => !known.has(d.name))
  if (watching && joinedName === null && newcomer) setJoinedName(newcomer.name)
  // Done with the link once a device joined with it: forget it.
  const { reset } = create
  useEffect(() => {
    if (joinedName !== null) reset()
  }, [joinedName, reset])

  const busy = preparing || create.isPending
  const generate = async () => {
    if (generating.current) return
    generating.current = true
    setPreparing(true)
    setGeneratedAt(Date.now())
    setExpired(false)
    setWatching(false)
    // Compare against the list as it is now, not as last polled: a device
    // that joined in the meantime (with another link) must not count as
    // this link's. Telling the two apart can't be exact; this keeps the
    // window to the time the link is out. Without a list, the first one
    // that arrives later is the baseline.
    let baseline: ReadonlySet<string> | null = null
    try {
      baseline = new Set((await fetchDevices()).map((d) => d.name))
    } catch {
      // The live query keeps trying; the hint below says so.
    }
    setKnown(baseline)
    setPreparing(false)
    create.mutate(role, {
      onSuccess: () => setWatching(role !== "worker"),
      onSettled: () => {
        generating.current = false
      },
    })
  }

  const expire = useCallback(() => {
    setExpired(true)
    reset()
  }, [reset])

  if (joinedName !== null) {
    const device = listed?.find((d) => d.name === joinedName)
    return <Joined name={joinedName} online={device?.online ?? false} />
  }

  const link = create.data
  // A development build has nothing to install; the server says which it is
  // only with the link.
  const devBuild = link !== undefined && !isReleaseVersion(link.serverVersion)
  return (
    <>
      <DialogHeader>
        <DialogTitle>{t("devices.addDevice")}</DialogTitle>
        <DialogDescription>{t(devBuild ? "devices.add.descriptionDev" : "devices.add.description")}</DialogDescription>
      </DialogHeader>
      <RoleChoice value={role} onChange={setRole} disabled={link !== undefined || busy} />
      {!devBuild && (
        <PlatformChoice value={platform} onChange={setPlatform} />
      )}
      {create.isError && (
        <p role="alert" className="text-sm text-destructive">
          {enrollLinkErrorMessage(create.error)}
        </p>
      )}
      {link && (
        <LinkPanel
          key={link.link}
          link={link}
          deadlineUnixMs={link.deadlineUnixMs}
          role={create.variables ?? role}
          platform={platform}
          waiting={watching}
          listFailing={watching && devices.isError}
          onExpired={expire}
        />
      )}
      {expired && !link && <Expired onGenerate={() => void generate()} busy={busy} />}
      <DialogFooter>
        {link || expired ? (
          <DialogClose asChild>
            <Button type="button" variant="outline">
              {t("common.close")}
            </Button>
          </DialogClose>
        ) : (
          <>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                {t("common.cancel")}
              </Button>
            </DialogClose>
            <GenerateButton busy={busy} onClick={() => void generate()}>
              {t("devices.add.generate")}
            </GenerateButton>
          </>
        )}
      </DialogFooter>
    </>
  )
}

/**
 * Issues a link. While busy the spinner shows it and `aria-busy` tells
 * assistive technology; the name stays the button's text.
 */
function GenerateButton({
  busy,
  onClick,
  className,
  children,
  ref,
}: {
  busy: boolean
  onClick: () => void
  className?: string
  children: ReactNode
  ref?: Ref<HTMLButtonElement>
}) {
  return (
    <Button
      ref={ref}
      type="button"
      aria-disabled={busy || undefined}
      aria-busy={busy || undefined}
      className={cn("aria-disabled:pointer-events-none aria-disabled:opacity-50", className)}
      onClick={onClick}
    >
      {busy && <Spinner aria-hidden role={undefined} aria-label={undefined} />}
      {children}
    </Button>
  )
}

/** The platform this browser runs on, refined by client hints once they arrive. */
function usePlatform(): [Platform, (p: Platform) => void] {
  const [platform, setPlatform] = useState<Platform>(() =>
    detectPlatform(typeof navigator === "undefined" ? undefined : (navigator as Navigator & NavigatorHints)),
  )
  const chosen = useRef(false)
  useEffect(() => {
    let live = true
    void detectArchitecture(navigator as Navigator & HighEntropyHints).then((p) => {
      if (live && p && !chosen.current) setPlatform(p)
    })
    return () => {
      live = false
    }
  }, [])
  return [
    platform,
    (p) => {
      chosen.current = true
      setPlatform(p)
    },
  ]
}

function RoleChoice({
  value,
  onChange,
  disabled,
}: {
  value: EnrollRole
  onChange: (role: EnrollRole) => void
  disabled: boolean
}) {
  const { t } = useTranslation()
  return (
    <fieldset disabled={disabled} className="flex min-w-0 flex-col gap-2">
      <legend className="mb-2 text-sm font-medium">{t("devices.add.role")}</legend>
      {roles.map((r) => (
        <label
          key={r}
          className={cn(
            "flex cursor-pointer items-start gap-3 border px-3 py-2.5 transition-colors",
            "hover:bg-muted has-checked:border-primary has-checked:bg-primary/5 has-checked:hover:bg-primary/5",
            // Muted is the dialog's own color in the dark theme.
            "dark:hover:bg-accent/50 dark:has-checked:bg-primary/10 dark:has-checked:hover:bg-primary/10",
            "has-focus-visible:ring-3 has-focus-visible:ring-ring/50",
            "has-disabled:cursor-default has-disabled:hover:bg-transparent has-disabled:not-has-checked:opacity-50",
          )}
        >
          <input
            type="radio"
            name="enroll-role"
            value={r}
            checked={value === r}
            onChange={() => onChange(r)}
            className="mt-0.5 size-3.5 shrink-0 accent-primary outline-none"
          />
          <span className="flex min-w-0 flex-col gap-0.5">
            <span className="text-sm leading-none font-medium">{t(`devices.add.roles.${r}`)}</span>
            <span className="text-xs text-muted-foreground">{t(`devices.add.roleHints.${r}`)}</span>
          </span>
        </label>
      ))}
    </fieldset>
  )
}

function PlatformChoice({ value, onChange }: { value: Platform; onChange: (p: Platform) => void }) {
  const { t } = useTranslation()
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  return (
    <Field className="gap-2">
      <FieldLabel htmlFor="add-device-platform">{t("devices.add.platform")}</FieldLabel>
      <Select value={value} onValueChange={(v) => isPlatform(v) && onChange(v)} onOpenChange={onOpenChange}>
        <SelectTrigger id="add-device-platform" ref={triggerRef} className="w-full sm:w-64">
          <SelectValue />
        </SelectTrigger>
        <SelectContent position="popper" onCloseAutoFocus={onCloseAutoFocus}>
          {platforms.map((p) => (
            <SelectItem key={p} value={p}>
              {t(`devices.add.platforms.${p}`)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </Field>
  )
}

const MINUTE = 60_000

/**
 * Now, as of the last time the remaining time crossed a whole minute: the
 * expiry phrase changes exactly then, and once more at the deadline. Timers
 * stop while a computer sleeps, so the clock also catches up whenever the
 * page becomes visible or focused again.
 */
function useExpiryClock(deadlineUnixMs: number): number {
  const [now, setNow] = useState(Date.now)
  useEffect(() => {
    let id = 0
    const schedule = () => {
      const remaining = deadlineUnixMs - Date.now()
      if (remaining > 0) id = window.setTimeout(tick, remaining % MINUTE || MINUTE)
    }
    const tick = () => {
      window.clearTimeout(id)
      setNow(Date.now())
      schedule()
    }
    schedule()
    document.addEventListener("visibilitychange", tick)
    window.addEventListener("focus", tick)
    return () => {
      window.clearTimeout(id)
      document.removeEventListener("visibilitychange", tick)
      window.removeEventListener("focus", tick)
    }
  }, [deadlineUnixMs])
  return now
}

/** Moves focus to the element on mount when the keyboard was last used: the control the user was on is gone. */
function useKeyboardFocusOnMount<T extends HTMLElement>() {
  const ref = useRef<T>(null)
  useEffect(() => {
    if (lastInputWasKeyboard()) ref.current?.focus()
  }, [])
  return ref
}

function LinkPanel({
  link,
  deadlineUnixMs,
  role,
  platform,
  waiting,
  listFailing,
  onExpired,
}: {
  link: EnrollLink
  /** When the link expires by this browser's clock. */
  deadlineUnixMs: number
  role: EnrollRole
  platform: Platform
  waiting: boolean
  listFailing: boolean
  onExpired: () => void
}) {
  const { t } = useTranslation()
  const now = useExpiryClock(deadlineUnixMs)
  const isExpired = now >= deadlineUnixMs
  useEffect(() => {
    if (isExpired) onExpired()
  }, [isExpired, onExpired])
  const copyRef = useKeyboardFocusOnMount<HTMLButtonElement>()

  const release = isReleaseVersion(link.serverVersion)
  return (
    <section aria-label={t("devices.add.runOnMachine")} className="flex min-w-0 flex-col gap-2">
      <p className="text-sm">
        {release ? t("devices.add.runOnMachine") : t("devices.add.devBuild", { version: link.serverVersion })}
      </p>
      <CommandBlock command={enrollCommand({ ...link, role, platform })} copyRef={copyRef} />
      {role !== "daemon" && <p className="text-xs text-muted-foreground">{t("devices.add.workerNote")}</p>}
      <p className="text-xs text-muted-foreground">
        {t("devices.add.expires", { in: formatTimeUntil(deadlineUnixMs, now) })}
      </p>
      {waiting && (
        <p role="status" className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
          <Spinner aria-hidden role={undefined} aria-label={undefined} className="size-3.5" />
          {listFailing ? t("devices.add.listFailing") : t("devices.add.waiting")}
        </p>
      )}
    </section>
  )
}

function Expired({ onGenerate, busy }: { onGenerate: () => void; busy: boolean }) {
  const { t } = useTranslation()
  const generateRef = useKeyboardFocusOnMount<HTMLButtonElement>()
  return (
    <div className="flex flex-col items-start gap-3 border border-dashed px-4 py-4">
      <div className="flex items-start gap-3">
        <TimerOffIcon className="mt-0.5 size-4 shrink-0 text-muted-foreground" aria-hidden />
        <div className="flex flex-col gap-1">
          <p role="status" className="text-sm font-medium">
            {t("devices.add.expired")}
          </p>
          <p className="text-xs text-muted-foreground">{t("devices.add.expiredHint")}</p>
        </div>
      </div>
      <GenerateButton ref={generateRef} busy={busy} className="ml-7" onClick={onGenerate}>
        {t("devices.add.generateNew")}
      </GenerateButton>
    </div>
  )
}

/** A device joined with the link. */
function Joined({ name, online }: { name: string; online: boolean }) {
  const { t } = useTranslation()
  const closeRef = useKeyboardFocusOnMount<HTMLButtonElement>()
  return (
    <>
      <DialogHeader role="status" className="items-center gap-3 py-4 text-center">
        <CircleCheckIcon className="size-8 text-foreground" strokeWidth={1.5} aria-hidden />
        <DialogTitle className="text-balance wrap-anywhere">
          {online ? t("devices.add.connected", { name }) : t("devices.add.joined", { name })}
        </DialogTitle>
        <DialogDescription>
          {online ? t("devices.add.connectedHint") : t("devices.add.joinedHint")}
        </DialogDescription>
      </DialogHeader>
      <DialogFooter>
        <DialogClose asChild>
          <Button ref={closeRef} type="button">
            {t("common.close")}
          </Button>
        </DialogClose>
      </DialogFooter>
    </>
  )
}
