import { CircleCheckIcon, TimerOffIcon } from "lucide-react"
import { memo, useCallback, useEffect, useId, useRef, useState, type ReactNode, type Ref } from "react"
import { useTranslation } from "react-i18next"

import {
  useCreateEnrollLink,
  useDevices,
  useFetchDevices,
  useFetchWorkers,
  useWorkers,
  type EnrollLinkRequest,
} from "@/api/hooks"
import type { EnrollLink, EnrollRole } from "@/api/types"
import { checkIdentityName } from "@/api/validation"
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
import { Field, FieldDescription, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import {
  detectArchitecture,
  detectPlatform,
  enrollCommand,
  enrollLinkErrorMessage,
  isPlatform,
  joinedWith,
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

/** The device or worker a rejoin link replaces. */
export interface RejoinTarget {
  kind: "device" | "worker"
  id: string
  name: string
}

/**
 * Adds a machine for the signed-in user: pick a role, optionally a name,
 * and a platform, generate a one-time link, and run the command it makes on
 * the machine. With `rejoin` it joins a machine again as one of the user's
 * devices or workers instead: the role and the name are the target's, and
 * the link's enrollment revokes the target. The link lives only in this
 * dialog's state; closing it discards the link.
 *
 * Memoized: the devices page renders again on every status poll, and the
 * dialog (which follows the devices itself) has nothing to take from that.
 */
export const AddDeviceDialog = memo(function AddDeviceDialog({
  open,
  onOpenChange,
  onCloseAutoFocus,
  rejoin,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  onCloseAutoFocus?: (event: Event) => void
  rejoin?: RejoinTarget
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogContent
        className="max-h-[calc(100dvh-2rem)] gap-5 overflow-y-auto *:min-w-0 sm:max-w-xl"
        onCloseAutoFocus={onCloseAutoFocus}
      >
        <AddDevice rejoin={rejoin} />
      </DialogContent>
    </Dialog>
  )
})

/** What the dialog watches for after issuing a link: who joined with it. */
interface Watch {
  /** The link's request: its role and, fixed with it, its name. */
  request: EnrollLinkRequest
  /** When the link was asked for: lists from before then don't count. */
  since: number
  /** The device and worker ids listed when the link was issued; null until a list arrives. */
  devices: ReadonlySet<string> | null
  workers: ReadonlySet<string> | null
}

const ids = <T,>(list: readonly T[] | undefined, id: (item: T) => string) =>
  list === undefined ? null : new Set(list.map(id))

/**
 * The dialog's contents. Mounted with the dialog, so every opening starts
 * over. It follows the devices and workers, so every status poll renders
 * it; its parts are memoized and render only when their own props change.
 */
function AddDevice({ rejoin }: { rejoin: RejoinTarget | undefined }) {
  const { t } = useTranslation()
  const create = useCreateEnrollLink()
  const fixedRole: EnrollRole | undefined = rejoin && (rejoin.kind === "device" ? "daemon" : "worker")
  const [chosenRole, setRole] = useState<EnrollRole>("daemon")
  const role = fixedRole ?? chosenRole
  // The name for `join --name`; empty for the machine's host name. A
  // rejoin joins under its target's name, which the server sets.
  const [name, setName] = useState("")
  const checkedName = rejoin || name === "" ? null : checkIdentityName(name)
  const nameProblem = checkedName && !checkedName.ok ? checkedName.problem : null
  const nameRef = useRef<HTMLInputElement>(null)
  const [platform, setPlatform] = usePlatform()
  const [expired, setExpired] = useState(false)
  // Fetching the lists before a link is issued.
  const [preparing, setPreparing] = useState(false)
  // A click arriving before the busy state renders must not issue another link.
  const generating = useRef(false)
  // Set once a link is issued and kept after it expires, so a join just
  // before the expiry still counts.
  const [watch, setWatch] = useState<Watch | null>(null)
  const watchDevices = watch !== null && watch.request.role !== "worker"
  const watchWorkers = watch !== null && watch.request.role !== "daemon"
  const devices = useDevices({ live: watchDevices })
  const workers = useWorkers({ live: watchWorkers })
  const fetchDevices = useFetchDevices()
  const fetchWorkers = useFetchWorkers()

  // Without a list fetched for the link, the first one fetched after it is
  // the baseline (the cache may hold one from before: a failed fetch keeps it).
  if (watch && watchDevices && watch.devices === null && devices.data && devices.dataUpdatedAt >= watch.since) {
    setWatch({ ...watch, devices: ids(devices.data, (d) => d.deviceId) })
  }
  if (watch && watchWorkers && watch.workers === null && workers.data && workers.dataUpdatedAt >= watch.since) {
    setWatch({ ...watch, workers: ids(workers.data, (w) => w.workerId) })
  }
  const wanted = rejoin?.name ?? watch?.request.name ?? ""
  const joinedDevice =
    watchDevices && watch.devices && devices.data
      ? joinedWith(watch.devices, devices.data, (d) => d.deviceId, wanted)
      : undefined
  const joinedWorker =
    watchWorkers && watch.workers && workers.data
      ? joinedWith(watch.workers, workers.data, (w) => w.workerId, wanted)
      : undefined
  const joined = watch?.request.role === "worker" ? joinedWorker : joinedDevice
  // The first one that joined is the answer; it stays even if a later list
  // misses it.
  const [joinedName, setJoinedName] = useState<string | null>(null)
  if (joined && joinedName === null) setJoinedName(joined.name)
  // Done with the link once what it enrolls joined: forget it. A link for
  // both roles stays until the worker joined too, for enrolling the worker
  // on its own should that part of the join fail.
  const bothPending = watch?.request.role === "both" && joinedWorker === undefined
  const { reset } = create
  useEffect(() => {
    if (joinedName !== null && !bothPending) reset()
  }, [joinedName, bothPending, reset])

  const busy = preparing || create.isPending
  const generate = async () => {
    if (generating.current) return
    if (nameProblem) {
      nameRef.current?.focus()
      return
    }
    generating.current = true
    setPreparing(true)
    setExpired(false)
    setWatch(null)
    const request: EnrollLinkRequest = { role, name: rejoin ? "" : name, ...(rejoin ? { replaceId: rejoin.id } : {}) }
    const since = Date.now()
    // Compare against the lists as they are now, not as last polled: one
    // that joined in the meantime (with another link) must not count as
    // this link's. Without a list, the first one that arrives later is
    // the baseline.
    const [listedDevices, listedWorkers] = await Promise.all([
      role === "worker" ? undefined : fetchDevices().catch(() => undefined),
      role === "daemon" ? undefined : fetchWorkers().catch(() => undefined),
    ])
    setPreparing(false)
    create.mutate(request, {
      onSuccess: () =>
        setWatch({
          request,
          since,
          devices: ids(listedDevices, (d) => d.deviceId),
          workers: ids(listedWorkers, (w) => w.workerId),
        }),
      onSettled: () => {
        generating.current = false
      },
    })
  }

  const expire = useCallback(() => {
    setExpired(true)
    reset()
  }, [reset])

  if (joinedName !== null && watch) {
    const roleJoined = watch.request.role === "worker" ? "worker" : "device"
    const online = (roleJoined === "worker" ? workers.data : devices.data)?.find((d) => d.name === joinedName)?.online ?? false
    // The worker command of a both-role link lasts as long as its link: once
    // it expires, `expire` forgets the link and the guidance asks for a new one.
    const expiring = create.data && bothPending ? create.data.deadlineUnixMs : null
    return (
      <>
        {expiring !== null && <ExpiryWatch deadlineUnixMs={expiring} onExpired={expire} />}
        <Joined
          name={joinedName}
          kind={roleJoined}
          online={online}
          rejoin={rejoin !== undefined}
          worker={
            watch.request.role === "both"
              ? {
                  joined: joinedWorker?.name,
                  // Enrolling the worker alone, with the same link's unused worker token.
                  command: create.data
                    ? enrollCommand({ ...create.data, role: "worker", platform, name: watch.request.name })
                    : undefined,
                }
              : undefined
          }
        />
      </>
    )
  }

  const link = create.data
  // The link's own request, fixed when it was issued.
  const issued = create.variables ?? watch?.request
  // A development build has nothing to install; the server says which it is
  // only with the link.
  const devBuild = link !== undefined && !isReleaseVersion(link.serverVersion)
  const waitingFor = watch && (watch.request.role === "worker" ? "worker" : "device")
  const listFailing = (watchDevices && devices.isError) || (watchWorkers && workers.isError)
  return (
    <>
      <DialogHeader>
        <DialogTitle>{rejoin ? t("devices.rejoin.title", { name: rejoin.name }) : t("devices.addDevice")}</DialogTitle>
        <DialogDescription>
          {rejoin
            ? t(rejoin.kind === "device" ? "devices.rejoin.descriptionDevice" : "devices.rejoin.descriptionWorker", {
                name: rejoin.name,
              })
            : t(devBuild ? "devices.add.descriptionDev" : "devices.add.description")}
        </DialogDescription>
      </DialogHeader>
      <RoleChoice
        value={role}
        onChange={setRole}
        only={fixedRole}
        disabled={fixedRole !== undefined || link !== undefined || busy}
      />
      {rejoin ? (
        <FixedName name={rejoin.name} />
      ) : (
        <NameField
          inputRef={nameRef}
          value={issued && link ? issued.name : name}
          problem={link ? null : nameProblem}
          onChange={setName}
          onSubmit={() => void generate()}
          disabled={link !== undefined || busy}
        />
      )}
      {!devBuild && <PlatformChoice value={platform} onChange={setPlatform} />}
      {create.isError && (
        <p role="alert" className="text-sm text-destructive">
          {enrollLinkErrorMessage(create.error)}
        </p>
      )}
      {link && issued && (
        <LinkPanel
          key={link.link}
          link={link}
          deadlineUnixMs={link.deadlineUnixMs}
          role={issued.role}
          name={issued.name}
          rejoinName={rejoin?.name}
          platform={platform}
          waiting={waitingFor}
          listFailing={listFailing}
          onExpired={expire}
        />
      )}
      {expired && !link && <Expired onGenerate={() => void generate()} busy={busy} blocked={nameProblem !== null} />}
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
            <GenerateButton busy={busy} blocked={nameProblem !== null} onClick={() => void generate()}>
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
 * assistive technology; the name stays the button's text. Blocked (by an
 * invalid name) it looks disabled but stays focusable.
 */
function GenerateButton({
  busy,
  blocked = false,
  onClick,
  className,
  children,
  ref,
}: {
  busy: boolean
  blocked?: boolean
  onClick: () => void
  className?: string
  children: ReactNode
  ref?: Ref<HTMLButtonElement>
}) {
  return (
    <Button
      ref={ref}
      type="button"
      aria-disabled={busy || blocked || undefined}
      aria-busy={busy || undefined}
      // Blocked, a click still reaches it and sends the user to the name.
      className={cn("aria-busy:pointer-events-none aria-disabled:opacity-50", className)}
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
  const choose = useCallback((p: Platform) => {
    chosen.current = true
    setPlatform(p)
  }, [])
  useEffect(() => {
    let live = true
    void detectArchitecture(navigator as Navigator & HighEntropyHints).then((p) => {
      if (live && p && !chosen.current) setPlatform(p)
    })
    return () => {
      live = false
    }
  }, [])
  return [platform, choose]
}

/** The role to enroll; `only` shows just that one (a rejoin's). */
const RoleChoice = memo(function RoleChoice({
  value,
  onChange,
  only,
  disabled,
}: {
  value: EnrollRole
  onChange: (role: EnrollRole) => void
  only?: EnrollRole
  disabled: boolean
}) {
  const { t } = useTranslation()
  return (
    <fieldset disabled={disabled} className="flex min-w-0 flex-col gap-2">
      <legend className="mb-2 text-sm font-medium">{t("devices.add.role")}</legend>
      {(only ? [only] : roles).map((r) => (
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
})

/**
 * The optional name the machine joins under, checked as typed with the
 * server's rules; empty means the machine's host name.
 */
const NameField = memo(function NameField({
  inputRef,
  value,
  problem,
  onChange,
  onSubmit,
  disabled,
}: {
  inputRef: Ref<HTMLInputElement>
  value: string
  problem: string | null
  onChange: (name: string) => void
  /** Enter in the field: generate the link. */
  onSubmit: () => void
  disabled: boolean
}) {
  const { t } = useTranslation()
  const id = useId()
  return (
    <Field className="gap-2" data-disabled={disabled || undefined}>
      <FieldLabel htmlFor={`${id}-name`}>{t("devices.add.name")}</FieldLabel>
      <Input
        ref={inputRef}
        id={`${id}-name`}
        value={value}
        disabled={disabled}
        autoComplete="off"
        autoCapitalize="off"
        autoCorrect="off"
        spellCheck={false}
        aria-invalid={problem ? true : undefined}
        aria-describedby={problem ? `${id}-error ${id}-hint` : `${id}-hint`}
        className="sm:w-64"
        onChange={(e) => onChange(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && !e.nativeEvent.isComposing) {
            e.preventDefault()
            onSubmit()
          }
        }}
      />
      {problem && <FieldError id={`${id}-error`}>{problem}</FieldError>}
      <FieldDescription id={`${id}-hint`} className="text-xs">
        {t("devices.add.nameHint")}
      </FieldDescription>
    </Field>
  )
})

/** A rejoin's name, as text: the target's, which the new identity keeps. */
function FixedName({ name }: { name: string }) {
  const { t } = useTranslation()
  return (
    <div className="flex flex-col gap-1">
      <dl className="flex flex-col gap-1">
        <dt className="text-sm font-medium">{t("devices.rejoin.name")}</dt>
        <dd className="text-sm wrap-anywhere">{name}</dd>
      </dl>
      <p className="text-xs text-muted-foreground">{t("devices.rejoin.nameHint")}</p>
    </div>
  )
}

const PlatformChoice = memo(function PlatformChoice({
  value,
  onChange,
}: {
  value: Platform
  onChange: (p: Platform) => void
}) {
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
})

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

/** Calls `onExpired` once the link's deadline passes (useExpiryClock), rendering nothing. */
function ExpiryWatch({ deadlineUnixMs, onExpired }: { deadlineUnixMs: number; onExpired: () => void }) {
  const now = useExpiryClock(deadlineUnixMs)
  const isExpired = now >= deadlineUnixMs
  useEffect(() => {
    if (isExpired) onExpired()
  }, [isExpired, onExpired])
  return null
}

/** Moves focus to the element on mount when the keyboard was last used: the control the user was on is gone. */
function useKeyboardFocusOnMount<T extends HTMLElement>() {
  const ref = useRef<T>(null)
  useEffect(() => {
    if (lastInputWasKeyboard()) ref.current?.focus()
  }, [])
  return ref
}

const LinkPanel = memo(function LinkPanel({
  link,
  deadlineUnixMs,
  role,
  name,
  rejoinName,
  platform,
  waiting,
  listFailing,
  onExpired,
}: {
  link: EnrollLink
  /** When the link expires by this browser's clock. */
  deadlineUnixMs: number
  role: EnrollRole
  /** The name to join under; empty for the host name (or a rejoin's target's). */
  name: string
  /** A rejoin link's target: the join passes --force. */
  rejoinName: string | undefined
  platform: Platform
  /** What the dialog waits for to join, if anything. */
  waiting: "device" | "worker" | null | undefined
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
      <CommandBlock
        command={enrollCommand({ ...link, role, platform, name, force: rejoinName !== undefined })}
        copyRef={copyRef}
      />
      {rejoinName !== undefined && (
        <p className="text-xs text-muted-foreground">{t("devices.rejoin.forceNote", { name: rejoinName })}</p>
      )}
      {role !== "daemon" && (
        <p className="text-xs text-muted-foreground">
          {release ? t("devices.add.workerNote") : t("devices.add.workerNoteDev")}
        </p>
      )}
      <p className="text-xs text-muted-foreground">
        {t("devices.add.expires", { in: formatTimeUntil(deadlineUnixMs, now) })}
      </p>
      {waiting && (
        <p role="status" className="mt-1 flex items-center gap-2 text-xs text-muted-foreground">
          <Spinner aria-hidden role={undefined} aria-label={undefined} className="size-3.5" />
          {listFailing
            ? t("devices.add.listFailing")
            : waiting === "worker"
              ? t("devices.add.waitingWorker")
              : t("devices.add.waiting")}
        </p>
      )}
    </section>
  )
})

function Expired({ onGenerate, busy, blocked }: { onGenerate: () => void; busy: boolean; blocked: boolean }) {
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
      <GenerateButton ref={generateRef} busy={busy} blocked={blocked} className="ml-7" onClick={onGenerate}>
        {t("devices.add.generateNew")}
      </GenerateButton>
    </div>
  )
}

/**
 * A device or a worker joined with the link (for a rejoin, replacing its
 * target). For a link of both roles the device is the one shown, and
 * `worker` says whether the worker joined too: a both-role join enrolls
 * the worker after the device, and that can fail on its own.
 */
function Joined({
  name,
  kind,
  online,
  rejoin,
  worker,
}: {
  name: string
  kind: "device" | "worker"
  online: boolean
  rejoin: boolean
  /** The worker's state for a both-role link: its name once joined, else the command that enrolls it alone. */
  worker: { joined: string | undefined; command: string | undefined } | undefined
}) {
  const { t } = useTranslation()
  const closeRef = useKeyboardFocusOnMount<HTMLButtonElement>()
  let title, hint
  if (rejoin) {
    title = online ? t("devices.rejoin.doneConnected", { name }) : t("devices.rejoin.done", { name })
    hint = t("devices.rejoin.doneHint")
  } else {
    title = online ? t("devices.add.connected", { name }) : t("devices.add.joined", { name })
    hint =
      kind === "worker"
        ? t(online ? "devices.add.connectedWorkerHint" : "devices.add.joinedWorkerHint")
        : t(online ? "devices.add.connectedHint" : "devices.add.joinedHint")
  }
  return (
    <>
      <DialogHeader role="status" className="items-center gap-3 py-4 text-center">
        <CircleCheckIcon className="size-8 text-foreground" strokeWidth={1.5} aria-hidden />
        <DialogTitle className="text-balance wrap-anywhere">{title}</DialogTitle>
        <DialogDescription>{hint}</DialogDescription>
      </DialogHeader>
      {worker &&
        (worker.joined !== undefined ? (
          <p role="status" className="text-center text-sm">
            {t("devices.add.workerJoinedToo", { name: worker.joined })}
          </p>
        ) : (
          <div className="flex flex-col gap-1.5">
            <p role="status" className="flex items-center gap-2 text-xs text-muted-foreground">
              <Spinner aria-hidden role={undefined} aria-label={undefined} className="size-3.5" />
              {t("devices.add.workerPending")}
            </p>
            <p className="text-xs text-muted-foreground">
              {t(worker.command ? "devices.add.workerFailedHint" : "devices.add.workerFailedHintNoLink")}
            </p>
            {worker.command && <CommandBlock command={worker.command} />}
          </div>
        ))}
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
