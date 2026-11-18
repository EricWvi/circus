import { CheckIcon, CopyIcon, FolderIcon, MonitorSmartphoneIcon } from "lucide-react"
import { useEffect, useRef, useState, type ReactNode } from "react"
import { Trans, useTranslation } from "react-i18next"
import { Link } from "react-router"

import { useDevices, useProjects } from "@/api/hooks"
import type { Device, Project } from "@/api/types"
import { InfoTip } from "@/components/info-tip"
import { PageHeader } from "@/components/page-header"
import { DeviceStatusDot } from "@/components/status-dot"
import { Button } from "@/components/ui/button"
import {
  Empty,
  EmptyContent,
  EmptyDescription,
  EmptyHeader,
  EmptyMedia,
  EmptyTitle,
} from "@/components/ui/empty"
import { Skeleton } from "@/components/ui/skeleton"
import { formatDateTime, formatTimeAgo } from "@/lib/time"
import { projectPath } from "@/lib/tree"
import { cn } from "@/lib/utils"

/** How a device is added (see `circus server --help` and `circus join --help`). */
export const ENROLL_COMMAND = "circus server enroll-token create --role daemon"
export const JOIN_COMMAND = "circus join"
export const DAEMON_COMMAND = "circus daemon"

/** The signed-in user's devices: status, platform, and the projects on each. */
export function DevicesPage() {
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
    body = <NoDevices />
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
      <PageHeader>
        <h1 className="min-w-0 truncate font-medium">{t("devices.title")}</h1>
      </PageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-[760px] flex-col px-4 py-8">{body}</div>
      </div>
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

/** No devices: what one is, and the commands that add one. */
function NoDevices() {
  const { t } = useTranslation()
  // Inline in a small note: the note's size, not the code font size (the
  // unlayered rule in index.css would win over a utility class).
  const code = <code className="text-foreground" style={{ fontSize: "1em", lineHeight: "inherit" }} />
  return (
    <Empty className="border border-dashed px-4 py-8 sm:px-6 sm:py-10">
      <EmptyHeader>
        <EmptyMedia variant="icon">
          <MonitorSmartphoneIcon />
        </EmptyMedia>
        <EmptyTitle>{t("devices.emptyTitle")}</EmptyTitle>
        <EmptyDescription className="text-pretty">{t("devices.emptyDescription")}</EmptyDescription>
      </EmptyHeader>
      <EmptyContent className="max-w-lg items-stretch text-left">
        <ol className="flex flex-col gap-4">
          <Step n={1} text={t("devices.stepCreateLink")} command={ENROLL_COMMAND}>
            <Trans i18nKey="devices.stepCreateLinkNote" components={{ code }} />
          </Step>
          <Step n={2} text={t("devices.stepJoin")} command={JOIN_COMMAND} />
          <Step n={3} text={t("devices.stepDaemon")} command={DAEMON_COMMAND} />
        </ol>
      </EmptyContent>
    </Empty>
  )
}

function Step({
  n,
  text,
  command,
  children,
}: {
  n: number
  text: string
  command: string
  children?: ReactNode
}) {
  return (
    <li className="grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1.5 text-sm">
      <span aria-hidden className="w-4 text-right text-muted-foreground tabular-nums">
        {n}
      </span>
      <span>{text}</span>
      <div className="col-start-2">
        <CommandLine command={command} />
      </div>
      {children && <p className="col-start-2 text-xs text-muted-foreground">{children}</p>}
    </li>
  )
}

/** A shell command to copy: scrolls sideways on its own if it doesn't fit. */
function CommandLine({ command }: { command: string }) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)
  const timer = useRef(0)
  useEffect(() => () => window.clearTimeout(timer.current), [])

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(command)
    } catch {
      return
    }
    setCopied(true)
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(() => setCopied(false), 2000)
  }

  return (
    <div className="flex min-w-0 items-center border bg-muted/50">
      <pre className="min-w-0 flex-1 overflow-x-auto px-3 py-2 font-mono">
        <code>{command}</code>
      </pre>
      <Button
        type="button"
        variant="ghost"
        size="icon-sm"
        className="mr-1"
        aria-label={copied ? t("devices.copied") : t("devices.copyCommand")}
        title={copied ? t("devices.copied") : t("devices.copyCommand")}
        onClick={() => void copy()}
      >
        {copied ? <CheckIcon /> : <CopyIcon />}
      </Button>
      <span className="sr-only" aria-live="polite">
        {copied ? t("devices.copied") : ""}
      </span>
    </div>
  )
}
