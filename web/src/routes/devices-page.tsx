import { FolderIcon, MonitorSmartphoneIcon, MonitorIcon, PlusIcon } from "lucide-react"
import { memo, useCallback, useState } from "react"
import { useTranslation } from "react-i18next"
import { Link } from "react-router"

import { useDevices, useProjects, useWorkers } from "@/api/hooks"
import type { Device, Project, Worker } from "@/api/types"
import { AddDeviceDialog } from "@/components/devices/add-device-dialog"
import { InfoTip } from "@/components/info-tip"
import { PageHeader } from "@/components/page-header"
import { DeviceStatusDot } from "@/components/status-dot"
import { Button } from "@/components/ui/button"
import {
  Empty,
  EmptyContent,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { formatDateTime, formatTimeAgo } from "@/lib/time"
import { projectPath } from "@/lib/tree"
import { cn } from "@/lib/utils"

/** The signed-in user's devices (status, platform, and the projects on each), then their workers. */
export function DevicesPage() {
  const { t } = useTranslation()
  // Only the list itself (in DevicesSection) follows each status poll; the
  // page renders again only when the devices change.
  const devices = useDevices({ live: true })
  const [adding, setAdding] = useState(false)
  const focus = useOverlayFocus()
  // Stable, like `focus`, for the memoized sections and dialog.
  const { onOpenChange } = focus
  const setAddOpen = useCallback(
    (open: boolean) => {
      onOpenChange(open)
      setAdding(open)
    },
    [onOpenChange],
  )

  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader
        actions={
          // With no devices, the empty state's own button is the only one.
          devices.data && devices.data.length > 0 ? (
            <Button variant="outline" size="sm" onClick={() => setAddOpen(true)}>
              <PlusIcon />
              {t("devices.addDevice")}
            </Button>
          ) : undefined
        }
      >
        <h1 className="min-w-0 truncate font-medium">{t("devices.title")}</h1>
      </PageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-[760px] flex-col gap-10 px-4 py-8">
          <DevicesSection onAdd={setAddOpen} />
          <WorkersSection />
        </div>
      </div>
      <AddDeviceDialog open={adding} onOpenChange={setAddOpen} onCloseAutoFocus={focus.onCloseAutoFocus} />
    </div>
  )
}

/**
 * The devices, kept fresh. Memoized, like WorkersSection: a status poll
 * renders this section (its last seen times move) and not the page.
 * `onAdd(true)` opens the add-device dialog.
 */
const DevicesSection = memo(function DevicesSection({ onAdd }: { onAdd: (open: boolean) => void }) {
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
    body = <NoDevices onAdd={() => onAdd(true)} />
  } else {
    body = (
      <ul className="flex flex-col divide-y border">
        {devices.data.map((device) => (
          <DeviceRow
            key={device.name}
            device={device}
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
}: {
  device: Device
  /** All the user's projects; the row shows those on its device. */
  projects: Project[] | undefined
  lastSeen: string | undefined
}) {
  const { t } = useTranslation()
  const platform = platformOf(device)
  const projects = projectsOn(device, allProjects)
  return (
    <li className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-3 gap-y-1.5 px-4 py-3">
      <DeviceStatusDot online={device.online} />
      <span className="truncate text-sm font-medium">{device.name}</span>
      <LinkState device={device} lastSeen={lastSeen} />
      {(platform || device.version) && (
        <p className="col-span-2 col-start-2 flex min-w-0 flex-wrap gap-x-3 text-xs text-muted-foreground">
          {platform && <span>{platform}</span>}
          {device.version && <span>{t("devices.daemonVersion", { version: device.version })}</span>}
        </p>
      )}
      {projects.length > 0 && (
        <ul
          aria-label={t("devices.projectsOn", { device: device.name })}
          className="col-span-2 col-start-2 flex min-w-0 flex-wrap gap-1.5 pt-0.5"
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

/** The user's workers, read only: status, version, and the devices on the same machine. */
const WorkersSection = memo(function WorkersSection() {
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
    body = <p className="text-sm text-muted-foreground">{t("devices.noWorkers")}</p>
  } else {
    body = (
      <ul className="flex flex-col divide-y border">
        {workers.data.map((w) => (
          <WorkerRow key={w.workerId} worker={w} lastSeen={lastSeenAgo(w, workers.dataUpdatedAt)} />
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
        <p className="text-xs text-muted-foreground">{t("devices.workersHint")}</p>
      </div>
      {body}
    </section>
  )
})

/** Memoized, like DeviceRow. */
const WorkerRow = memo(function WorkerRow({ worker, lastSeen }: { worker: Worker; lastSeen: string | undefined }) {
  const { t } = useTranslation()
  const platform = [worker.os, worker.arch].filter(Boolean).join("/")
  return (
    <li className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-3 gap-y-1.5 px-4 py-3">
      <DeviceStatusDot online={worker.online} />
      <span className="truncate text-sm font-medium">{worker.name}</span>
      <LinkState device={worker} lastSeen={lastSeen} />
      {(platform || worker.version) && (
        <p className="col-span-2 col-start-2 flex min-w-0 flex-wrap gap-x-3 text-xs text-muted-foreground">
          {platform && <span>{platform}</span>}
          {worker.version && <span>{t("devices.workerVersion", { version: worker.version })}</span>}
        </p>
      )}
      {worker.localDevices.length > 0 && (
        <div className="col-span-2 col-start-2 flex min-w-0 flex-wrap items-center gap-1.5 pt-0.5">
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
    </li>
  )
})

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
function NoDevices({ onAdd }: { onAdd: () => void }) {
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
        <Button onClick={onAdd}>
          <PlusIcon />
          {t("devices.addDevice")}
        </Button>
      </EmptyContent>
    </Empty>
  )
}
