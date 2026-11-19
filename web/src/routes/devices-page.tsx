import { FolderIcon, MonitorSmartphoneIcon, PlusIcon } from "lucide-react"
import { useState } from "react"
import { useTranslation } from "react-i18next"
import { Link } from "react-router"

import { useDevices, useProjects } from "@/api/hooks"
import type { Device, Project } from "@/api/types"
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

/** The signed-in user's devices: status, platform, and the projects on each. */
export function DevicesPage() {
  const { t } = useTranslation()
  const devices = useDevices({ live: true })
  const projects = useProjects()
  const [adding, setAdding] = useState(false)
  const focus = useOverlayFocus()
  const setAddOpen = (open: boolean) => {
    focus.onOpenChange(open)
    setAdding(open)
  }

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
    body = <NoDevices onAdd={() => setAddOpen(true)} />
  } else {
    body = (
      <ul className="flex flex-col divide-y border">
        {devices.data.map((device) => (
          <DeviceRow
            key={device.name}
            device={device}
            projects={projectsOn(device, projects.data)}
            // Relative times count from the last fetch, so each refetch
            // (every few seconds) also moves "last seen" along.
            now={devices.dataUpdatedAt}
          />
        ))}
      </ul>
    )
  }

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
        <div className="mx-auto flex w-full max-w-[760px] flex-col px-4 py-8">{body}</div>
      </div>
      <AddDeviceDialog open={adding} onOpenChange={setAddOpen} onCloseAutoFocus={focus.onCloseAutoFocus} />
    </div>
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

/** "linux/amd64", or whichever half is known. */
const platformOf = (device: Device) => [device.os, device.arch].filter(Boolean).join("/")

function DeviceRow({ device, projects, now }: { device: Device; projects: Project[]; now: number }) {
  const { t } = useTranslation()
  const platform = platformOf(device)
  return (
    <li className="grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-x-3 gap-y-1.5 px-4 py-3">
      <DeviceStatusDot online={device.online} />
      <span className="truncate text-sm font-medium">{device.name}</span>
      <LinkState device={device} now={now} />
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
}

/** "Online", or when an offline device was last seen (the date on hover). */
function LinkState({ device, now }: { device: Device; now: number }) {
  const { t } = useTranslation()
  const className = "shrink-0 text-xs whitespace-nowrap text-muted-foreground"
  if (device.online) return <span className={cn(className, "text-foreground")}>{t("devices.online")}</span>
  if (device.lastSeenUnixMs === 0) return <span className={className}>{t("devices.neverConnected")}</span>
  return (
    <InfoTip content={formatDateTime(device.lastSeenUnixMs)} side="top" align="end">
      <time
        tabIndex={0}
        dateTime={new Date(device.lastSeenUnixMs).toISOString()}
        className={cn(className, "cursor-default rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring/50")}
      >
        {t("devices.lastSeen", { ago: formatTimeAgo(device.lastSeenUnixMs, now) })}
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
