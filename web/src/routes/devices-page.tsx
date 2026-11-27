import {
  ChevronDownIcon,
  EllipsisIcon,
  FolderIcon,
  InfoIcon,
  MonitorIcon,
  MonitorSmartphoneIcon,
  PlusIcon,
  ServerIcon,
} from "lucide-react"
import { memo, useCallback, useEffect, useId, useRef, useState, type RefObject } from "react"
import { useTranslation } from "react-i18next"
import { Link, useLocation } from "react-router"

import {
  useDevices,
  useModelProviders,
  useProjects,
  useSetWorkerGrant,
  useWorkerGrantStates,
  useWorkers,
} from "@/api/hooks"
import { isApiError } from "@/api/errors"
import type { Device, EnrollRole, Project, Worker } from "@/api/types"
import { AddDeviceDialog, type RejoinTarget } from "@/components/devices/add-device-dialog"
import { DeleteMachineDialog, type DeleteTarget } from "@/components/devices/delete-machine-dialog"
import { WorkerAgents } from "@/components/devices/worker-agents"
import { ProvidersSection, type ProvidersSectionHandle } from "@/components/model-providers/providers-section"
import { InfoTip } from "@/components/info-tip"
import { PageHeader } from "@/components/page-header"
import { DeviceStatusDot } from "@/components/status-dot"
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  Empty,
  EmptyContent,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import { Switch } from "@/components/ui/switch"
import { useMenuFocus } from "@/hooks/use-menu-focus"
import { useReturnFocus } from "@/hooks/use-return-focus"
import { i18n } from "@/i18n/i18n"
import { formatDateTime, formatTimeAgo } from "@/lib/time"
import { projectPath } from "@/lib/tree"
import { cn } from "@/lib/utils"

/**
 * The signed-in user's devices (status, platform, and the projects on each),
 * then their workers, then their model providers (`#providers`).
 */
export function DevicesPage() {
  const { t } = useTranslation()
  // Only the list itself (in DevicesSection) follows each status poll; the
  // page renders again only when the devices change.
  const devices = useDevices({ live: true })
  const [adding, setAdding] = useState(false)
  // The device or worker a Rejoin… or Delete… of a row is for. Each stays
  // set until its dialog has closed (its exit animation shows it), and is
  // replaced when one opens.
  const [rejoin, setRejoin] = useState<RejoinTarget | null>(null)
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [deleting, setDeleting] = useState<DeleteTarget | null>(null)
  // The role an Add opens the add dialog with: the workers' empty state
  // asks for a worker, the other Add buttons for a device.
  const [addRole, setAddRole] = useState<EnrollRole>("daemon")
  // The header's Add menu trigger, where focus goes once a dialog's opener is gone.
  const addRef = useRef<HTMLButtonElement>(null)
  const addTrigger = useCallback(() => addRef.current, [])
  const providersRef = useRef<ProvidersSectionHandle>(null)
  // Where focus goes when a dialog closes: the button that opened it (an
  // Add, or a row's menu); once that is gone, the menu of the row now under
  // its name (a rejoin's new identity), else (deleted, or the empty state
  // left) the Add button.
  const dialogFocus = useReturnFocus()
  const focusRow = useRef<RejoinTarget | null>(null)
  const onAdd = useCallback((role: EnrollRole, trigger: HTMLElement | null) => {
    dialogFocus.opened(trigger)
    focusRow.current = null
    setRejoin(null)
    setAddRole(role)
    setAdding(true)
  }, [dialogFocus])
  const onAddWorker = useCallback((trigger: HTMLElement | null) => onAdd("worker", trigger), [onAdd])
  const onAction = useCallback((action: RowAction) => {
    dialogFocus.opened(action.trigger)
    focusRow.current = action.target
    if (action.kind === "rejoin") {
      setRejoin(action.target)
      setAdding(true)
    } else {
      setDeleting(action.target)
      setDeleteOpen(true)
    }
  }, [dialogFocus])
  const closeAutoFocus = useCallback(
    (event: Event) => {
      const row = focusRow.current
      dialogFocus.restore(event, () => (row && rowTrigger(row)) || addRef.current)
      setRejoin(null)
      setDeleting(null)
    },
    [dialogFocus],
  )
  const onDeleted = useCallback(() => {
    dialogFocus.forget()
    focusRow.current = null
  }, [dialogFocus])

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        actions={
          // Once the devices are known (or can't be): the add dialog compares
          // the list from before its link with the one after.
          (devices.data || devices.isError) && (
            <AddMenu
              triggerRef={addRef}
              onAdd={onAdd}
              onAddProvider={(trigger) => providersRef.current?.addProvider(trigger)}
            />
          )
        }
      >
        <h1 className="min-w-0 truncate font-medium">{t("devices.title")}</h1>
      </PageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-[760px] flex-col gap-10 px-4 py-8">
          <DevicesSection onAdd={onAdd} onAction={onAction} />
          <WorkersSection onAdd={onAdd} onAction={onAction} />
          <ProvidersSection ref={providersRef} onAddWorker={onAddWorker} fallbackFocus={addTrigger} />
        </div>
      </div>
      <ScrollToHash />
      <AddDeviceDialog
        // A new dialog for each target: its state is the link's. The key
        // changes only as one opens (the target is cleared once closed).
        key={rejoin ? `${rejoin.kind}:${rejoin.id}` : `add:${addRole}`}
        open={adding}
        onOpenChange={setAdding}
        onCloseAutoFocus={closeAutoFocus}
        rejoin={rejoin ?? undefined}
        role={addRole}
      />
      <DeleteMachineDialog
        open={deleteOpen}
        target={deleting}
        onOpenChange={setDeleteOpen}
        onDeleted={onDeleted}
        onCloseAutoFocus={closeAutoFocus}
      />
    </div>
  )
}

/**
 * Scrolls to the section the location's hash names (`/devices#providers`)
 * and focuses it, once each list has loaded or failed to: until then their
 * placeholders are shorter than the lists (the projects add a line to each
 * device), and the section would move. Once per navigation, so the user's
 * own scrolling is left alone after; and focus stays where the user put it
 * meanwhile. A hash that isn't a valid escape names nothing.
 */
function ScrollToHash() {
  const { hash, key } = useLocation()
  const devices = useDevices()
  const workers = useWorkers()
  const providers = useModelProviders()
  const projects = useProjects()
  const settled = [devices, workers, providers, projects].every((q) => q.data !== undefined || q.isError)
  const done = useRef<string | null>(null)
  useEffect(() => {
    if (!settled || hash.length < 2 || done.current === key) return
    done.current = key
    let id
    try {
      id = decodeURIComponent(hash.slice(1))
    } catch {
      return
    }
    const target = document.getElementById(id)
    if (!target) return
    target.scrollIntoView({ block: "start" })
    const active = document.activeElement
    if (!active || active === document.body) target.focus({ preventScroll: true })
  }, [settled, hash, key])
  return null
}

/**
 * The devices, kept fresh. Memoized, like WorkersSection: a status poll
 * renders this section (its last seen times move) and not the page.
 * `onAdd` opens the add dialog for a device.
 */
const DevicesSection = memo(function DevicesSection({
  onAdd,
  onAction,
}: {
  onAdd: AddHandler
  onAction: (action: RowAction) => void
}) {
  const { t } = useTranslation()
  const devices = useDevices({ live: true })
  const projects = useProjects()

  let body
  // Only a failed first load shows the error; a failed refetch keeps the list.
  if (devices.isError && !devices.data) {
    body = (
      <div className="flex flex-col items-center gap-4 py-12">
        <p className="text-sm text-muted-foreground">{t("devices.loadFailed")}</p>
        <Button variant="outline" onClick={() => void devices.refetch()}>
          {t("common.tryAgain")}
        </Button>
      </div>
    )
  } else if (!devices.data) {
    body = <LoadingList />
  } else if (devices.data.length === 0) {
    body = <NoDevices onAdd={(trigger) => onAdd("daemon", trigger)} />
  } else {
    body = (
      <ul className="flex flex-col divide-y border">
        {devices.data.map((device) => (
          <DeviceRow
            key={device.deviceId}
            device={device}
            onAction={onAction}
            projects={projects.data}
            // Relative times count from the last fetch, so each refetch
            // (every few seconds) also moves "last seen" along.
            lastSeen={lastSeenAgo(device, devices.dataUpdatedAt)}
          />
        ))}
      </ul>
    )
  }

  return (
    <section className="flex flex-col gap-3" aria-labelledby="devices-heading">
      <h2 id="devices-heading" className="text-sm font-medium">
        {t("devices.devicesHeading")}
      </h2>
      {body}
    </section>
  )
})

/**
 * The projects with a location on the device, in the list's order (by
 * name). Locations name their device; one on a revoked device of the same
 * name belongs to that old device, not this one.
 */
function projectsOn(device: Device, projects: Project[] | undefined): Project[] {
  return (projects ?? []).filter((p) =>
    p.locations.some((l) => l.device === device.name && !l.deviceRevoked),
  )
}

/** "linux/amd64", or whichever half is known. */
const platformOf = (device: Device) => [device.os, device.arch].filter(Boolean).join("/")

/**
 * How long ago an offline device or worker was last seen, as of `now`; none
 * when it is online or never connected.
 */
function lastSeenAgo(device: Pick<Device, "online" | "lastSeenUnixMs">, now: number): string | undefined {
  return device.online || device.lastSeenUnixMs === 0 ? undefined : formatTimeAgo(device.lastSeenUnixMs, now)
}

/**
 * Memoized, like WorkerRow: the list refetches every few seconds, mostly
 * unchanged. The rows take the time since last seen as its text and the
 * query's data as it is (both stay the same between refetches that change
 * nothing), so such a refetch renders none of them.
 */
const DeviceRow = memo(function DeviceRow({
  device,
  projects: allProjects,
  lastSeen,
  onAction,
}: {
  device: Device
  onAction: (action: RowAction) => void
  /** All the user's projects; the row shows those on its device. */
  projects: Project[] | undefined
  lastSeen: string | undefined
}) {
  const { t } = useTranslation()
  const platform = platformOf(device)
  const projects = projectsOn(device, allProjects)
  return (
    <li className="grid grid-cols-[auto_minmax(0,1fr)_auto_auto] items-center gap-x-3 gap-y-1.5 px-4 py-3">
      <DeviceStatusDot online={device.online} />
      <span className="truncate text-sm font-medium">{device.name}</span>
      <LinkState device={device} lastSeen={lastSeen} />
      <RowMenu kind="device" id={device.deviceId} name={device.name} online={device.online} onAction={onAction} />
      {(platform || device.version) && (
        <p className="col-span-3 col-start-2 flex min-w-0 flex-wrap gap-x-3 text-xs text-muted-foreground">
          {platform && <span>{platform}</span>}
          {device.version && <span>{t("devices.daemonVersion", { version: device.version })}</span>}
        </p>
      )}
      {projects.length > 0 && (
        <ul
          aria-label={t("devices.projectsOn", { device: device.name })}
          className="col-span-3 col-start-2 flex min-w-0 flex-wrap gap-1.5 pt-0.5"
        >
          {projects.map((p) => (
            <li key={p.projectId} className="min-w-0">
              <Link
                to={projectPath(p.projectId)}
                className="flex h-6 min-w-0 items-center gap-1.5 border px-2 text-xs text-muted-foreground outline-none hover:bg-muted hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
              >
                <FolderIcon className="size-3 shrink-0" aria-hidden />
                <span className="truncate">{p.name}</span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </li>
  )
})

/** The user's workers: status, version, the devices on the same machine, their agents, and the integrator grant. */
const WorkersSection = memo(function WorkersSection({
  onAdd,
  onAction,
}: {
  onAdd: AddHandler
  onAction: (action: RowAction) => void
}) {
  const { t } = useTranslation()
  // Here rather than in the page, so a refetch renders only this section.
  const workers = useWorkers({ live: true })
  let body
  if (workers.isError && !workers.data) {
    body = (
      <div className="flex items-center gap-3">
        <p className="text-sm text-muted-foreground">{t("devices.workersLoadFailed")}</p>
        <Button variant="outline" size="sm" onClick={() => void workers.refetch()}>
          {t("common.tryAgain")}
        </Button>
      </div>
    )
  } else if (!workers.data) {
    body = (
      <ul aria-busy="true" aria-label={t("devices.workersLoading")} className="flex flex-col divide-y border">
        <li className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-3 px-4 py-3.5">
          <Skeleton className="size-2.5 rounded-full" />
          <Skeleton className="h-3.5 w-32" />
          <Skeleton className="h-3 w-14" />
        </li>
      </ul>
    )
  } else if (workers.data.length === 0) {
    body = <NoWorkers onAdd={(trigger) => onAdd("worker", trigger)} />
  } else {
    body = (
      <ul className="flex flex-col divide-y border">
        {workers.data.map((w) => (
          <WorkerRow
            key={w.workerId}
            worker={w}
            onAction={onAction}
            lastSeen={lastSeenAgo(w, workers.dataUpdatedAt)}
            reportedAgo={agentsReportedAgo(w, workers.dataUpdatedAt)}
          />
        ))}
      </ul>
    )
  }
  return (
    <section className="flex flex-col gap-3" aria-labelledby="workers-heading">
      <div className="flex flex-col gap-1">
        <h2 id="workers-heading" className="text-sm font-medium">
          {t("devices.workersHeading")}
        </h2>
      </div>
      {body}
    </section>
  )
})

/**
 * How long ago an offline worker last reported its agents, as of `now`;
 * none when it is online (its report is current) or never reported. A
 * worker reports all its agents at once.
 */
function agentsReportedAgo(worker: Worker, now: number): string | undefined {
  const at = Math.max(0, ...worker.agents.map((a) => a.reportedAtUnixMs))
  return worker.online || at === 0 ? undefined : formatTimeAgo(at, now)
}

/** Memoized, like DeviceRow; `reportedAgo` is from agentsReportedAgo. */
const WorkerRow = memo(function WorkerRow({
  worker,
  lastSeen,
  reportedAgo,
  onAction,
}: {
  worker: Worker
  onAction: (action: RowAction) => void
  lastSeen: string | undefined
  reportedAgo: string | undefined
}) {
  const { t } = useTranslation()
  const platform = [worker.os, worker.arch].filter(Boolean).join("/")
  return (
    <li className="grid grid-cols-[auto_minmax(0,1fr)_auto_auto] items-center gap-x-3 gap-y-1.5 px-4 py-3">
      <DeviceStatusDot online={worker.online} />
      <span className="truncate text-sm font-medium">{worker.name}</span>
      <LinkState device={worker} lastSeen={lastSeen} />
      <RowMenu kind="worker" id={worker.workerId} name={worker.name} online={worker.online} onAction={onAction} />
      {(platform || worker.version) && (
        <p className="col-span-3 col-start-2 flex min-w-0 flex-wrap gap-x-3 text-xs text-muted-foreground">
          {platform && <span>{platform}</span>}
          {worker.version && <span>{t("devices.workerVersion", { version: worker.version })}</span>}
        </p>
      )}
      {worker.localDevices.length > 0 && (
        <div className="col-span-3 col-start-2 flex min-w-0 flex-wrap items-center gap-1.5 pt-0.5">
          <span className="text-xs text-muted-foreground" aria-hidden>
            {t("devices.sameMachine")}
          </span>
          <ul aria-label={t("devices.sameMachineAs", { worker: worker.name })} className="flex min-w-0 flex-wrap gap-1.5">
            {worker.localDevices.map((d) => (
              <li key={d} className="flex h-6 min-w-0 items-center gap-1.5 border px-2 text-xs text-muted-foreground">
                <MonitorIcon className="size-3 shrink-0" aria-hidden />
                <span className="truncate">{d}</span>
              </li>
            ))}
          </ul>
        </div>
      )}
      <WorkerAgents worker={worker} reportedAgo={reportedAgo} />
      <IntegratorGrant worker={worker} />
    </li>
  )
})

/**
 * The worker's integrator switch. While a change is in flight the switch
 * shows the requested state, whatever list refetches say, and ignores
 * further presses; then it shows the worker the server returned. Changes
 * live in the mutation cache, so this holds when the page is left and
 * opened again meanwhile. It stays enabled (only marked busy), so focus
 * stays on it. A failure is shown until a list shows the requested state
 * after all (the change was applied but its answer lost) or the next try.
 */
function IntegratorGrant({ worker }: { worker: Worker }) {
  const { t } = useTranslation()
  const id = useId()
  const grant = useSetWorkerGrant(worker)
  const changes = useWorkerGrantStates(worker.workerId)
  const pending = changes.findLast((c) => c.status === "pending")
  const latest = changes.at(-1)
  const checked = pending ? pending.variables!.integrator : worker.integrator
  // A worker that is gone is reported by a notice, as its row goes.
  const failed =
    latest?.status === "error" &&
    !isApiError(latest.error, "not_found") &&
    latest.variables!.integrator !== worker.integrator
  return (
    <div className="col-span-3 col-start-2 flex min-w-0 flex-col gap-1 pt-0.5">
      <div className="flex min-w-0 items-center gap-2">
        <Switch
          id={`${id}-switch`}
          size="sm"
          checked={checked}
          aria-busy={pending !== undefined}
          aria-describedby={`${id}-hint`}
          className="aria-busy:cursor-progress aria-busy:opacity-70"
          onCheckedChange={(integrator) => {
            if (!pending) grant.mutate({ integrator })
          }}
        />
        <label htmlFor={`${id}-switch`} className="cursor-default text-xs text-muted-foreground">
          {t("devices.integrator")}
        </label>
        <InfoTip content={t("devices.integratorHint")} contentClassName="max-w-xs" side="top">
          <span
            tabIndex={0}
            className="flex cursor-default items-center rounded-sm p-0.5 text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            <InfoIcon className="size-3" aria-hidden />
            <span id={`${id}-hint`} className="sr-only">
              {t("devices.integratorHint")}
            </span>
          </span>
        </InfoTip>
        {pending && <Spinner className="size-3 text-muted-foreground" aria-label={t("common.saving")} />}
      </div>
      {failed && (
        <p role="alert" className="text-xs text-destructive">
          {t("devices.integratorFailed")}
        </p>
      )}
    </div>
  )
}

/** The menu trigger of the device's or worker's row now listed under `row`'s name, if any. */
function rowTrigger(row: RejoinTarget): HTMLElement | null {
  const section = document.querySelector(`section[aria-labelledby="${row.kind === "device" ? "devices" : "workers"}-heading"]`)
  const label = i18n.t("devices.actions", { name: row.name })
  return [...(section?.querySelectorAll<HTMLElement>("button[aria-label]") ?? [])].find((b) => b.getAttribute("aria-label") === label) ?? null
}

/** Opens the add dialog with `role` chosen; `trigger` is the button, where focus returns. */
type AddHandler = (role: EnrollRole, trigger: HTMLElement | null) => void

/**
 * The header's Add menu: a device, a worker (the add dialog with that role
 * chosen) or a model provider (the providers section's form). Each dialog
 * returns focus to the trigger.
 */
function AddMenu({
  triggerRef,
  onAdd,
  onAddProvider,
}: {
  triggerRef: RefObject<HTMLButtonElement | null>
  onAdd: AddHandler
  onAddProvider: (trigger: HTMLElement | null) => void
}) {
  const { t } = useTranslation()
  const menuFocus = useMenuFocus()
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild {...menuFocus.trigger}>
        <Button ref={triggerRef} variant="outline" size="sm">
          <PlusIcon />
          {t("devices.addAction")}
          <ChevronDownIcon />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-auto min-w-40" {...menuFocus.content}>
        <DropdownMenuItem onSelect={() => onAdd("daemon", triggerRef.current)}>{t("devices.addDevice")}</DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onAdd("worker", triggerRef.current)}>{t("devices.addWorker")}</DropdownMenuItem>
        <DropdownMenuItem onSelect={() => onAddProvider(triggerRef.current)}>{t("devices.addProvider")}</DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/** What a row's menu asks the page to do; `trigger` is the menu's button, where focus returns. */
type RowAction = ({ kind: "rejoin"; target: RejoinTarget } | { kind: "delete"; target: DeleteTarget }) & {
  trigger: HTMLElement | null
}

/**
 * A device's or worker's actions: Rejoin… (only while it is offline: a
 * rejoin is refused while the old one is online) and Delete….
 */
function RowMenu({
  kind,
  id,
  name,
  online,
  onAction,
}: {
  kind: "device" | "worker"
  id: string
  name: string
  online: boolean
  onAction: (action: RowAction) => void
}) {
  const { t } = useTranslation()
  const menuFocus = useMenuFocus()
  const triggerRef = useRef<HTMLButtonElement>(null)
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild {...menuFocus.trigger}>
        <Button
          ref={triggerRef}
          variant="ghost"
          size="icon-sm"
          className="-my-1 size-7"
          aria-label={t("devices.actions", { name })}
        >
          <EllipsisIcon />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-auto min-w-36" {...menuFocus.content}>
        {!online && (
          <DropdownMenuItem
            onSelect={() => onAction({ kind: "rejoin", target: { kind, id, name }, trigger: triggerRef.current })}
          >
            {t("devices.rejoinAction")}
          </DropdownMenuItem>
        )}
        <DropdownMenuItem
          variant="destructive"
          onSelect={() => onAction({ kind: "delete", target: { kind, id, name, online }, trigger: triggerRef.current })}
        >
          {t("devices.deleteAction")}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

/**
 * "Online", or when an offline device or worker was last seen (the date on
 * hover). `lastSeen` is how long ago, from lastSeenAgo: none when online or
 * never connected.
 */
function LinkState({
  device,
  lastSeen,
}: {
  device: Pick<Device, "online" | "lastSeenUnixMs">
  lastSeen: string | undefined
}) {
  const { t } = useTranslation()
  const className = "shrink-0 text-xs whitespace-nowrap text-muted-foreground"
  if (device.online) return <span className={cn(className, "text-foreground")}>{t("devices.online")}</span>
  if (lastSeen === undefined) return <span className={className}>{t("devices.neverConnected")}</span>
  return (
    <InfoTip content={formatDateTime(device.lastSeenUnixMs)} side="top" align="end">
      <time
        tabIndex={0}
        dateTime={new Date(device.lastSeenUnixMs).toISOString()}
        className={cn(className, "cursor-default rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring/50")}
      >
        {t("devices.lastSeen", { ago: lastSeen })}
      </time>
    </InfoTip>
  )
}

function LoadingList() {
  const { t } = useTranslation()
  return (
    <ul aria-busy="true" aria-label={t("devices.loading")} className="flex flex-col divide-y border">
      {[0, 1, 2].map((i) => (
        <li key={i} className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-3 gap-y-2.5 px-4 py-3.5">
          <Skeleton className="size-2.5 rounded-full" />
          <Skeleton className="h-3.5 w-32" />
          <Skeleton className="h-3 w-14" />
          <Skeleton className="col-start-2 h-3 w-40" />
        </li>
      ))}
    </ul>
  )
}

/** No devices: what one is, and the way to add one. */
function NoDevices({ onAdd }: { onAdd: (trigger: HTMLElement) => void }) {
  const { t } = useTranslation()
  return (
    <Empty className="border border-dashed px-4 py-8 sm:px-6 sm:py-10">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <MonitorSmartphoneIcon />
        </EmptyMedia>
        <EmptyTitle>{t("devices.emptyTitle")}</EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button onClick={(e) => onAdd(e.currentTarget)}>
          <PlusIcon />
          {t("devices.addDevice")}
        </Button>
      </EmptyContent>
    </Empty>
  )
}

/** No workers: the way to add one, with the worker role chosen. */
function NoWorkers({ onAdd }: { onAdd: (trigger: HTMLElement) => void }) {
  const { t } = useTranslation()
  return (
    <Empty className="border border-dashed px-4 py-8 sm:px-6 sm:py-10">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <ServerIcon />
        </EmptyMedia>
        <EmptyTitle>{t("devices.workersEmptyTitle")}</EmptyTitle>
      </EmptyHeader>
      <EmptyContent>
        <Button onClick={(e) => onAdd(e.currentTarget)}>
          <PlusIcon />
          {t("devices.addWorker")}
        </Button>
      </EmptyContent>
    </Empty>
  )
}
