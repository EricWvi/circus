import { ChevronDownIcon, EllipsisIcon, FolderIcon, InfoIcon, MonitorSmartphoneIcon, PlusIcon } from "lucide-react"
import { memo, useCallback, useEffect, useId, useMemo, useRef, useState, type RefObject } from "react"
import { useTranslation } from "react-i18next"
import { Link, useLocation } from "react-router"

import {
  useDevices,
  useModelProviders,
  useProjects,
  useSetWorkerGrant,
  useWorkerGrantStates,
  useWorkers,
  type MachineUpdateTarget,
} from "@/api/hooks"
import { isApiError } from "@/api/errors"
import type { Device, EnrollRole, Project, Worker } from "@/api/types"
import { AddDeviceDialog, type RejoinTarget } from "@/components/devices/add-device-dialog"
import { DeleteMachineDialog, type DeleteTarget } from "@/components/devices/delete-machine-dialog"
import { MachineUpdateAction } from "@/components/devices/machine-update"
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
import { groupMachines, type Machine } from "@/lib/machines"
import { formatDateTime, formatTimeAgo } from "@/lib/time"
import { projectPath } from "@/lib/tree"
import { cn } from "@/lib/utils"

/**
 * The signed-in user's machines, one card each with its device (status,
 * version, and the projects on it) and its worker (status, version, agents
 * and integrator grant), then their model providers (`#providers`).
 */
export function DevicesPage() {
  const { t } = useTranslation()
  // Only the list itself (in MachinesSection) follows each status poll; the
  // page renders again only when the devices change.
  const devices = useDevices({ live: true })
  const [adding, setAdding] = useState(false)
  // The device or worker a Rejoin… or Delete… of a row is for. Each stays
  // set until its dialog has closed (its exit animation shows it), and is
  // replaced when one opens.
  const [rejoin, setRejoin] = useState<RejoinTarget | null>(null)
  const [deleteOpen, setDeleteOpen] = useState(false)
  const [deleting, setDeleting] = useState<DeleteTarget | null>(null)
  // The role an Add opens the add dialog with.
  const [addRole, setAddRole] = useState<EnrollRole>("daemon")
  // The header's Add menu trigger, where focus goes once a dialog's opener is gone.
  const addRef = useRef<HTMLButtonElement>(null)
  const addTrigger = useCallback(() => addRef.current, [])
  const providersRef = useRef<ProvidersSectionHandle>(null)
  // Where focus goes when a dialog closes: the button that opened it (an
  // Add, or a row's menu); once that is gone, the menu of the row now under
  // its name (a rejoin's new identity), else the menu of another row of the
  // card a deleted row was on, else (the card went with it, or the empty
  // state left) the Add button.
  const dialogFocus = useReturnFocus()
  const focusRow = useRef<RejoinTarget | null>(null)
  // The card and the menu of the row a row's action was opened from, and,
  // once that row is deleted, the same for the deleted row.
  const actionFrom = useRef<RowOrigin | null>(null)
  const deletedFrom = useRef<RowOrigin | null>(null)
  const onAdd = useCallback((role: EnrollRole, trigger: HTMLElement | null) => {
    dialogFocus.opened(trigger)
    focusRow.current = null
    deletedFrom.current = null
    setRejoin(null)
    setAddRole(role)
    setAdding(true)
  }, [dialogFocus])
  const onAddWorker = useCallback((trigger: HTMLElement | null) => onAdd("worker", trigger), [onAdd])
  const onAction = useCallback((action: RowAction) => {
    dialogFocus.opened(action.trigger)
    focusRow.current = action.target
    actionFrom.current = { card: action.trigger?.closest("[data-machine-card]") ?? null, trigger: action.trigger }
    deletedFrom.current = null
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
      const from = deletedFrom.current
      deletedFrom.current = null
      dialogFocus.restore(event, () => (row && rowTrigger(row)) || (from && otherRowTrigger(from)) || addRef.current)
      setRejoin(null)
      setDeleting(null)
    },
    [dialogFocus],
  )
  const onDeleted = useCallback(() => {
    deletedFrom.current = actionFrom.current
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
          <MachinesSection onAdd={onAdd} onAction={onAction} />
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
 * The user's machines, kept fresh: one card for each, with a row for its
 * device and one for its worker. Memoized: a status poll renders this
 * section (its last seen times move) and not the page. `onAdd` opens the
 * add dialog.
 */
const MachinesSection = memo(function MachinesSection({
  onAdd,
  onAction,
}: {
  onAdd: AddHandler
  onAction: (action: RowAction) => void
}) {
  const { t } = useTranslation()
  const devices = useDevices({ live: true })
  const workers = useWorkers({ live: true })
  const projects = useProjects()
  // The lists keep their objects across refetches that change nothing, and so does this.
  const machines = useMemo(
    () => (devices.data && workers.data ? groupMachines(devices.data, workers.data) : undefined),
    [devices.data, workers.data],
  )

  let body
  // Only a failed first load shows the error; a failed refetch keeps the list.
  if ((devices.isError && !devices.data) || (workers.isError && !workers.data)) {
    body = (
      <div className="flex flex-col items-center gap-4 py-12">
        <p className="text-sm text-muted-foreground">{t("devices.loadFailed")}</p>
        <Button
          variant="outline"
          onClick={() => {
            if (!devices.data) void devices.refetch()
            if (!workers.data) void workers.refetch()
          }}
        >
          {t("common.tryAgain")}
        </Button>
      </div>
    )
  } else if (!machines) {
    body = <LoadingList />
  } else if (machines.length === 0) {
    body = <NoMachines onAdd={onAdd} />
  } else {
    body = (
      <ul className="flex flex-col gap-3">
        {machines.map((m) => (
          <MachineCard
            key={m.key}
            machine={m}
            projects={projects.data}
            onAction={onAction}
            // Relative times count from the last fetch, so each refetch
            // (every few seconds) also moves "last seen" along.
            devicesAt={devices.dataUpdatedAt}
            workersAt={workers.dataUpdatedAt}
          />
        ))}
      </ul>
    )
  }

  return (
    <section className="flex flex-col gap-3" aria-labelledby="machines-heading">
      <h2 id="machines-heading" className="text-sm font-medium">
        {t("devices.machinesHeading")}
      </h2>
      {body}
    </section>
  )
})

/**
 * A machine's card: its name, platform and update, then a row for each of
 * its roles. `devicesAt` and `workersAt` are when each list was fetched,
 * which relative times count from.
 */
function MachineCard({
  machine,
  projects,
  onAction,
  devicesAt,
  workersAt,
}: {
  machine: Machine
  projects: Project[] | undefined
  onAction: (action: RowAction) => void
  devicesAt: number
  workersAt: number
}) {
  const nameId = useId()
  const roles = [...machine.devices, ...machine.workers]
  const { platform } = machine
  const aligned = machine.devices.length > 0 && machine.workers.length > 0
  // The update is offered on the machine when a role offers it (the
  // device first); otherwise the card shows the machine's update, if any.
  const offering = machine.devices.find((d) => d.updateVersion) ?? machine.workers.find((w) => w.updateVersion)
  const pressed = offering ?? roles[0]
  const target: MachineUpdateTarget =
    "deviceId" in pressed
      ? { kind: "device", deviceId: pressed.deviceId, name: pressed.name, machine: machine.name }
      : { kind: "worker", workerId: pressed.workerId, name: pressed.name, machine: machine.name }
  return (
    <li data-machine-card aria-labelledby={nameId} className="flex min-w-0 flex-col border">
      <div className="flex min-w-0 flex-col gap-1.5 px-4 pt-3 pb-2 sm:flex-row sm:items-center sm:gap-3">
        <p className="flex min-w-0 flex-1 items-baseline gap-x-3">
          <span id={nameId} className="truncate text-sm font-medium">
            {machine.name}
          </span>
          {platform && <span className="shrink-0 text-xs text-muted-foreground">{platform}</span>}
        </p>
        {/* Mounted while it shows nothing too: it keeps its live region and focus (see MachineUpdateAction). */}
        <MachineUpdateAction
          target={target}
          updateVersion={offering?.updateVersion ?? ""}
          update={roles.find((r) => r.update)?.update}
        />
      </div>
      <ul className="flex min-w-0 flex-col">
        {machine.devices.map((d) => (
          <DeviceRow
            key={d.deviceId}
            device={d}
            onAction={onAction}
            machine={machine.name}
            aligned={aligned}
            projects={projects}
            lastSeen={lastSeenAgo(d, devicesAt)}
          />
        ))}
        {machine.workers.map((w) => (
          <WorkerRow
            key={w.workerId}
            worker={w}
            machine={machine.name}
            aligned={aligned}
            onAction={onAction}
            lastSeen={lastSeenAgo(w, workersAt)}
            reportedAgo={agentsReportedAgo(w, workersAt)}
          />
        ))}
      </ul>
    </li>
  )
}

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

/**
 * How long ago an offline device or worker was last seen, as of `now`; none
 * when it is online or never connected.
 */
function lastSeenAgo(device: Pick<Device, "online" | "lastSeenUnixMs">, now: number): string | undefined {
  return device.online || device.lastSeenUnixMs === 0 ? undefined : formatTimeAgo(device.lastSeenUnixMs, now)
}

/** Whether a role's name is the machine's, ignoring case: the row then leaves it out. */
const sameName = (name: string, machine: string) => name.toLowerCase() === machine.toLowerCase()

/**
 * The first line of a role's row: its status, role, name (left out when
 * it is the machine's, which the card's header shows) and version, its
 * link state and its menu, in the row's grid. The row's own label and the
 * menu's always name it. On a narrow screen the version wraps under the
 * name; every line is 20px high, and the dot, the link state and the menu
 * stay on the first one.
 */
function RoleHeader({
  kind,
  id,
  name,
  machine,
  aligned,
  version,
  role,
  lastSeen,
  onAction,
}: {
  kind: "device" | "worker"
  id: string
  name: string
  /** The card's name. */
  machine: string
  /** The card has rows of both kinds, whose labels line up. */
  aligned: boolean
  version: string
  role: Pick<Device, "online" | "lastSeenUnixMs">
  lastSeen: string | undefined
  onAction: (action: RowAction) => void
}) {
  const { t } = useTranslation()
  const deviceLabel = t("devices.roles.device")
  const workerLabel = t("devices.roles.worker")
  const [label, otherLabel] = kind === "device" ? [deviceLabel, workerLabel] : [workerLabel, deviceLabel]
  return (
    <>
      <span className={firstLine}>
        <DeviceStatusDot online={role.online} />
      </span>
      <span className="flex min-w-0 flex-wrap items-baseline gap-x-2">
        {/* The role and the name stay on one line; the version wraps first. */}
        <span className="flex max-w-full min-w-0 items-baseline gap-x-2">
          {/*
           * On a card with both kinds of rows, both labels in one cell, the
           * other one invisible: the cell is as wide as the longer one in
           * this language, so the rows line up.
           */}
          <span className="grid shrink-0 text-xs leading-5 text-muted-foreground">
            <span data-role-label className="col-start-1 row-start-1">
              {label}
            </span>
            {aligned && (
              <span aria-hidden className="invisible col-start-1 row-start-1">
                {otherLabel}
              </span>
            )}
          </span>
          {!sameName(name, machine) && (
            <span data-role-name className="min-w-0 truncate text-sm">
              {name}
            </span>
          )}
        </span>
        {/* The version alone: the label says whose (daemon or worker). */}
        {version && <span className="text-xs leading-5 whitespace-nowrap text-muted-foreground">{version}</span>}
      </span>
      <span className={cn(firstLine, "justify-end")}>
        <LinkState device={role} lastSeen={lastSeen} />
      </span>
      <span className={firstLine}>
        <RowMenu kind={kind} id={id} name={name} online={role.online} onAction={onAction} />
      </span>
    </>
  )
}

/** A role row's classes: a grid of the status dot, the name, the link state and the menu, then what spans the last three. */
const roleRow = "grid grid-cols-[auto_minmax(0,1fr)_auto_auto] items-start gap-x-3 gap-y-1.5 border-t px-4 py-2.5"

/** A cell that keeps to the first line of a role's row (its lines are 20px high). */
const firstLine = "flex h-5 items-center"

/**
 * Memoized, like WorkerRow: the lists refetch every few seconds, mostly
 * unchanged. The rows take the time since last seen as its text and the
 * query's data as it is (both stay the same between refetches that change
 * nothing), so such a refetch renders none of them.
 */
const DeviceRow = memo(function DeviceRow({
  device,
  machine,
  aligned,
  projects: allProjects,
  lastSeen,
  onAction,
}: {
  device: Device
  /** The card's name; the row leaves out the device's name when it is the same. */
  machine: string
  /** As RoleHeader's. */
  aligned: boolean
  onAction: (action: RowAction) => void
  /** All the user's projects; the row shows those on its device. */
  projects: Project[] | undefined
  lastSeen: string | undefined
}) {
  const { t } = useTranslation()
  const projects = projectsOn(device, allProjects)
  return (
    <li data-role-row="device" aria-label={t("devices.roleRow.device", { name: device.name })} className={roleRow}>
      <RoleHeader
        kind="device"
        id={device.deviceId}
        name={device.name}
        machine={machine}
        aligned={aligned}
        version={device.version}
        role={device}
        lastSeen={lastSeen}
        onAction={onAction}
      />
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
  machine,
  aligned,
  lastSeen,
  reportedAgo,
  onAction,
}: {
  worker: Worker
  /** As DeviceRow's. */
  machine: string
  aligned: boolean
  onAction: (action: RowAction) => void
  lastSeen: string | undefined
  reportedAgo: string | undefined
}) {
  const { t } = useTranslation()
  return (
    <li data-role-row="worker" aria-label={t("devices.roleRow.worker", { name: worker.name })} className={roleRow}>
      <RoleHeader
        kind="worker"
        id={worker.workerId}
        name={worker.name}
        machine={machine}
        aligned={aligned}
        version={worker.version}
        role={worker}
        lastSeen={lastSeen}
        onAction={onAction}
      />
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

/**
 * The menu of another row of the card a deleted row was on, if the card
 * is still listed: the deleted row's own menu (`trigger`) is skipped, in
 * case the list has not caught up yet.
 */
function otherRowTrigger({ card, trigger }: RowOrigin): HTMLElement | null {
  if (!card?.isConnected) return null
  return [...card.querySelectorAll<HTMLElement>("[data-role-row] [data-card-focus]")].find((b) => b !== trigger) ?? null
}

/** The menu trigger of the device's or worker's row now listed under `row`'s name, if any. */
function rowTrigger(row: RejoinTarget): HTMLElement | null {
  const label = i18n.t(row.kind === "device" ? "devices.actionsDevice" : "devices.actionsWorker", { name: row.name })
  const section = document.querySelector('section[aria-labelledby="machines-heading"]')
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

/** The card a row is on and the row's menu button. */
type RowOrigin = { card: Element | null; trigger: HTMLElement | null }

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
          aria-label={t(kind === "device" ? "devices.actionsDevice" : "devices.actionsWorker", { name })}
          // Where a card's machine update moves focus when nothing of it remains.
          data-card-focus
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
    <ul aria-busy="true" aria-label={t("devices.loading")} className="flex flex-col gap-3">
      {[0, 1].map((i) => (
        <li key={i} className="flex flex-col border">
          <div className="flex items-center gap-3 px-4 pt-3.5 pb-2.5">
            <Skeleton className="h-3.5 w-32" />
            <Skeleton className="h-3 w-20" />
          </div>
          <div className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-3 border-t px-4 py-3">
            <Skeleton className="size-2.5 rounded-full" />
            <Skeleton className="h-3.5 w-40" />
            <Skeleton className="h-3 w-14" />
          </div>
        </li>
      ))}
    </ul>
  )
}

/** No machines: what to add, a device or a worker, each with its role chosen. */
function NoMachines({ onAdd }: { onAdd: AddHandler }) {
  const { t } = useTranslation()
  return (
    <Empty className="border border-dashed px-4 py-8 sm:px-6 sm:py-10">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <MonitorSmartphoneIcon />
        </EmptyMedia>
        <EmptyTitle>{t("devices.emptyTitle")}</EmptyTitle>
      </EmptyHeader>
      <EmptyContent className="flex-row flex-wrap justify-center">
        <Button onClick={(e) => onAdd("daemon", e.currentTarget)}>
          <PlusIcon />
          {t("devices.addDevice")}
        </Button>
        <Button variant="outline" onClick={(e) => onAdd("worker", e.currentTarget)}>
          <PlusIcon />
          {t("devices.addWorker")}
        </Button>
      </EmptyContent>
    </Empty>
  )
}
