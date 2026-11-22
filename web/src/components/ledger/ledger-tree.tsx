import { ChevronRightIcon, FolderIcon, SearchIcon } from "lucide-react"
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react"
import { useTranslation } from "react-i18next"
import { Link, useNavigate } from "react-router"

import type { Ledger } from "@/api/types"
import { Input } from "@/components/ui/input"
import { ancestorDirs, buildLedgerTree, rowKey, visibleRows, type LedgerTreeRow } from "@/lib/ledger-tree"
import { decisionPath } from "@/lib/tree"
import { cn } from "@/lib/utils"

const INDENT_PX = 14
const nearest: ScrollIntoViewOptions = { block: "nearest" }

/**
 * The ledger's directories and decisions, with a filter. One tree (ARIA
 * tree pattern): Tab reaches it once, the arrow keys move within it, Right
 * and Left open and close directories, Enter or Space opens a decision.
 */
export function LedgerTree({
  projectId,
  ledger,
  selected,
  focusSelected = false,
  className,
}: {
  projectId: string
  ledger: Ledger
  /** The decision on the page (or just left), if any. */
  selected: number | null
  /** Focus the selected decision once it shows: the user came back to the list from it. */
  focusSelected?: boolean
  className?: string
}) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const root = useMemo(() => buildLedgerTree(ledger.entries), [ledger.entries])
  const [filter, setFilter] = useState("")
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(new Set())
  const rows = useMemo(() => visibleRows(root, { collapsed, filter }), [root, collapsed, filter])
  const filtering = filter.trim() !== ""

  // A newly selected decision's directories open, so it shows.
  const selectedDir = ledger.entries.find((e) => e.number === selected)?.dir
  const [openedFor, setOpenedFor] = useState<string | undefined>(undefined)
  if (selectedDir !== openedFor) {
    setOpenedFor(selectedDir)
    const above = ancestorDirs(selectedDir ?? "")
    if (above.some((d) => collapsed.has(d))) setCollapsed(new Set([...collapsed].filter((d) => !above.includes(d))))
  }

  // Roving focus: one row is in the tab order.
  const [focusKey, setFocusKey] = useState<string | null>(null)
  const selectedKey = selected === null ? null : `D${selected}`
  const tabKey =
    [focusKey, selectedKey].find((k) => k !== null && rows.some((r) => rowKey(r) === k)) ??
    (rows[0] ? rowKey(rows[0]) : null)
  const rowRefs = useRef(new Map<string, HTMLElement>())
  const focusRow = (key: string) => {
    setFocusKey(key)
    rowRefs.current.get(key)?.focus()
  }

  // The selected decision is scrolled into view when the page opens on it.
  const scrolled = useRef(false)

  // Back on the list from a decision: it is in view and has focus.
  useEffect(() => {
    if (!focusSelected || selected === null) return
    const el = rowRefs.current.get(`D${selected}`)
    el?.scrollIntoView?.(nearest)
    el?.focus({ preventScroll: true })
  }, [focusSelected, selected, rowRefs])

  const toggle = (path: string, open?: boolean) =>
    setCollapsed((set) => {
      const next = new Set(set)
      const shouldOpen = open ?? next.has(path)
      if (shouldOpen) next.delete(path)
      else next.add(path)
      return next
    })

  const onKeyDown = (event: KeyboardEvent, row: LedgerTreeRow, index: number) => {
    const key = rowKey(row)
    const move = (to: number) => {
      const target = rows[Math.max(0, Math.min(rows.length - 1, to))]
      if (target) focusRow(rowKey(target))
    }
    switch (event.key) {
      case "ArrowDown":
        move(index + 1)
        break
      case "ArrowUp":
        move(index - 1)
        break
      case "Home":
        move(0)
        break
      case "End":
        move(rows.length - 1)
        break
      case "ArrowRight":
        if (row.kind !== "dir") return
        if (!row.expanded) toggle(row.dir.path, true)
        else if (rows[index + 1]?.parent === key) move(index + 1)
        break
      case "ArrowLeft":
        if (row.kind === "dir" && row.expanded && !filtering) toggle(row.dir.path, false)
        else if (row.parent !== null) focusRow(row.parent)
        break
      case "Enter":
      case " ":
        if (row.kind === "dir") toggle(row.dir.path)
        else void navigate(decisionPath(projectId, row.entry.number))
        break
      default:
        return
    }
    event.preventDefault()
  }

  const rowClass = (active: boolean) =>
    cn(
      "flex h-8 min-w-0 shrink-0 items-center gap-1.5 rounded-md pr-2 text-sm outline-none select-none hover:bg-foreground/5 focus-visible:ring-2 focus-visible:ring-ring/50 focus-visible:ring-inset",
      active && "bg-foreground/10 hover:bg-foreground/10",
    )

  return (
    <div className={cn("flex min-h-0 flex-col", className)}>
      <div className="relative shrink-0 px-3 pt-3 pb-2">
        <SearchIcon className="pointer-events-none absolute top-1/2 left-5.5 mt-0.5 size-3.5 -translate-y-1/2 text-muted-foreground" aria-hidden />
        <Input
          type="search"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown" && rows[0]) {
              e.preventDefault()
              focusRow(rowKey(rows[0]))
            }
          }}
          placeholder={t("ledger.filterPlaceholder")}
          aria-label={t("ledger.filterLabel")}
          className="h-8 pl-7 text-sm"
        />
      </div>
      {rows.length === 0 && <p className="px-4 py-1.5 text-sm text-muted-foreground">{t("ledger.noMatches")}</p>}
      <div role="tree" aria-label={t("ledger.tree")} className="flex min-h-0 flex-1 flex-col overflow-y-auto px-2 pb-2">
        {rows.map((row, index) => {
          const key = rowKey(row)
          const common = {
            ref: (el: HTMLElement | null) => {
              if (!el) {
                rowRefs.current.delete(key)
                return
              }
              rowRefs.current.set(key, el)
              if (key === selectedKey && !scrolled.current) {
                scrolled.current = true
                el.scrollIntoView?.(nearest)
              }
            },
            "aria-level": row.level,
            "aria-setsize": row.setSize,
            "aria-posinset": row.posInSet,
            tabIndex: key === tabKey ? 0 : -1,
            onFocus: () => setFocusKey(key),
            onKeyDown: (e: KeyboardEvent) => onKeyDown(e, row, index),
            style: { paddingLeft: 8 + (row.level - 1) * INDENT_PX },
          } as const
          if (row.kind === "dir") {
            return (
              <div
                key={key}
                role="treeitem"
                {...common}
                aria-expanded={row.expanded}
                className={cn(rowClass(false), "cursor-default text-muted-foreground")}
                onClick={() => {
                  focusRow(key)
                  if (!filtering) toggle(row.dir.path)
                }}
              >
                <ChevronRightIcon className={cn("size-3.5 shrink-0 transition-transform", row.expanded && "rotate-90")} aria-hidden />
                <FolderIcon className="size-3.5 shrink-0" aria-hidden />
                <span className="min-w-0 flex-1 truncate text-foreground">{row.dir.name}</span>
                {!filtering && <span className="shrink-0 text-xs tabular-nums">{row.dir.count}</span>}
              </div>
            )
          }
          const active = row.entry.number === selected
          return (
            <Link
              key={key}
              role="treeitem"
              {...common}
              to={decisionPath(projectId, row.entry.number)}
              aria-selected={active}
              // A decision remembered on the list is selected, not the page.
              aria-current={active && !focusSelected ? "page" : undefined}
              className={rowClass(active)}
            >
              {/* Lines up with the directory names above. */}
              <span className="w-3.5 shrink-0" aria-hidden />
              <span className="min-w-9 shrink-0 font-mono text-xs text-muted-foreground tabular-nums">{`D${row.entry.number}`}</span>
              <span className="min-w-0 flex-1 truncate">{row.entry.title}</span>
            </Link>
          )
        })}
      </div>
      <p className="shrink-0 border-t px-4 py-2 text-xs text-muted-foreground tabular-nums">
        {t("ledger.summary", { count: ledger.entries.length, next: ledger.nextNumber })}
      </p>
    </div>
  )
}
