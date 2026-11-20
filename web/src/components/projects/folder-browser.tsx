import {
  CircleAlertIcon,
  CornerLeftUpIcon,
  FolderGit2Icon,
  FolderIcon,
  FolderSymlinkIcon,
  InfoIcon,
  Link2Icon,
} from "lucide-react"
import { useEffect, useId, useRef, useState, type KeyboardEvent, type ReactNode, type Ref } from "react"
import { useTranslation } from "react-i18next"

import { useDirectory } from "@/api/hooks"
import type { DirectoryEntry, DirectoryError, DirectoryListing } from "@/api/types"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { Skeleton } from "@/components/ui/skeleton"
import { Spinner } from "@/components/ui/spinner"
import {
  directoryKey,
  filterEntries,
  HOME,
  joinPath,
  pathErrorMessage,
  requestErrorMessage,
  splitInput,
  withSeparator,
} from "@/lib/folder-path"
import { cn } from "@/lib/utils"

/** Whether hidden folders are listed; per browser. */
export const SHOW_HIDDEN_KEY = "circus-folders-show-hidden"

function readShowHidden(): boolean {
  try {
    return localStorage.getItem(SHOW_HIDDEN_KEY) === "true"
  } catch {
    return false
  }
}

function writeShowHidden(value: boolean) {
  try {
    localStorage.setItem(SHOW_HIDDEN_KEY, String(value))
  } catch {
    // Not remembered; the choice lasts while the picker is open.
  }
}

type Row = { kind: "up"; path: string } | { kind: "dir"; path: string; entry: DirectoryEntry }

// Pointer presses on the picker's controls leave focus in the path input,
// so the arrow keys keep working and no focus ring appears.
const keepFocus = (event: { preventDefault: () => void }) => event.preventDefault()

/**
 * Browses the directories of one device and picks one, like a file
 * dialog's location bar: the input holds a directory and a filter for its
 * subfolders. Starts at `startPath`, or at the home directory when that is
 * empty or can't be listed (saying why).
 */
export function FolderBrowser({
  device,
  os,
  startPath,
  layout,
  inputRef,
  onSelect,
  onCancel,
}: {
  device: string
  /** The device's OS, for its path separators. */
  os: string
  startPath: string
  layout: "popover" | "sheet"
  inputRef?: Ref<HTMLInputElement>
  /** Called with the chosen directory's absolute path. */
  onSelect: (path: string) => void
  onCancel: () => void
}) {
  const { t } = useTranslation()
  const id = useId()
  const listId = `${id}-list`
  const optionId = (i: number) => `${id}-option-${i}`

  const start = startPath.trim()
  const startKey = start === "" ? HOME : directoryKey(start, os)
  const [input, setInput] = useState(start === "" ? "" : withSeparator(start))
  // The directory listed: a request path, as typed or resolved.
  const [dirKey, setDirKey] = useState(startKey)
  // Nothing was opened yet: a start directory that fails gives way to home.
  const [atStart, setAtStart] = useState(start !== "")
  const [startProblem, setStartProblem] = useState<{ path: string; error: DirectoryError } | null>(null)
  const [showHidden, setShowHidden] = useState(readShowHidden)
  const [selectWhenListed, setSelectWhenListed] = useState(false)
  const [quietFocus, setQuietFocus] = useState(true)

  const { query, fetch } = useDirectory({ device, path: dirKey, includeHidden: showHidden })
  // Bumped whenever the directory changes, so a "Select" waiting for the
  // previous one gives up.
  const selectToken = useRef(0)
  useEffect(
    () => () => {
      selectToken.current++
    },
    [],
  )
  // What the list shows: while another directory loads, the previous one.
  const shown = query.data
  const loadingNext = query.isPlaceholderData
  // The listing of the directory asked for, once it arrived.
  const current = loadingNext ? undefined : query.data
  const { filter } = splitInput(input, os)

  // A listing arrived: fall back from a failing start directory, or show
  // the resolved path ("~/" becomes "/home/you/"), keeping the filter.
  const [seen, setSeen] = useState<DirectoryListing | undefined>(undefined)
  if (current !== seen) {
    setSeen(current)
    if (current?.error && atStart) {
      setAtStart(false)
      setStartProblem({ path: start, error: current.error })
      setDirKey(HOME)
      setInput("")
    } else if (current && current.error === undefined && current.path !== "") {
      setAtStart(false)
      const typed = splitInput(input, os)
      if (input === "" || typed.dir === dirKey) {
        setInput(withSeparator(current.path) + typed.filter)
        // The listing is cached under its resolved path too.
        setDirKey(current.path)
      }
    }
  }

  const rows: Row[] = []
  if (shown && filter === "" && shown.parent !== "") rows.push({ kind: "up", path: shown.parent })
  for (const entry of filterEntries(shown?.entries ?? [], filter)) {
    rows.push({ kind: "dir", path: joinPath(shown?.path ?? "", entry.name), entry })
  }

  // The highlight starts on the first folder (not "..") of each new list.
  const listKey = `${shown?.path}\n${filter}\n${showHidden}`
  const firstIndex = rows[0]?.kind === "up" && rows.length > 1 ? 1 : 0
  const [highlight, setHighlight] = useState({ key: listKey, index: firstIndex })
  const active = rows.length === 0 ? -1 : highlight.key === listKey ? Math.min(highlight.index, rows.length - 1) : firstIndex

  // Keeps the highlighted row in view, also when a new list replaces the old one.
  const listRef = useRef<HTMLDivElement>(null)
  const highlightKey = active < 0 ? null : `${listKey}\n${active}`
  useEffect(() => {
    if (highlightKey === null) return
    listRef.current?.querySelector("[data-highlighted]")?.scrollIntoView?.({ block: "nearest" })
  }, [highlightKey])

  const leave = () => {
    selectToken.current++
    setSelectWhenListed(false)
    setAtStart(false)
    setStartProblem(null)
  }

  const open = (path: string) => {
    leave()
    setDirKey(path)
    setInput(withSeparator(path))
  }

  const select = () => {
    if (current) {
      if (current.error === undefined) onSelect(current.path)
      return
    }
    if (!query.isFetching || selectWhenListed) return
    const token = ++selectToken.current
    setSelectWhenListed(true)
    fetch().then(
      (listing) => {
        if (token !== selectToken.current) return
        setSelectWhenListed(false)
        if (listing.error === undefined) onSelect(listing.path)
      },
      () => {
        if (token === selectToken.current) setSelectWhenListed(false)
      },
    )
  }

  const change = (next: string) => {
    setInput(next)
    const { dir } = splitInput(next, os)
    if (dir !== null && dir !== dirKey) {
      leave()
      setDirKey(dir)
    }
  }

  const onKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.nativeEvent.isComposing) return
    const target = event.currentTarget
    switch (event.key) {
      case "ArrowDown":
      case "ArrowUp": {
        event.preventDefault()
        if (rows.length === 0) return
        const step = event.key === "ArrowDown" ? 1 : -1
        setHighlight({ key: listKey, index: Math.max(0, Math.min(rows.length - 1, active + step)) })
        return
      }
      case "Enter": {
        event.preventDefault()
        if (event.ctrlKey || event.metaKey) return select()
        const row = rows[active]
        if (row) open(row.path)
        else if (input.trim() !== "") open(directoryKey(input.trim(), os))
        return
      }
      case "Backspace": {
        const atEnd = target.selectionStart === input.length && target.selectionEnd === input.length
        if (filter === "" && input !== "" && atEnd && current && current.parent !== "") {
          event.preventDefault()
          open(current.parent)
        }
        return
      }
    }
  }

  const failed = query.isError ? requestErrorMessage(query.error, device) : null
  // Loading counts: "Select" then waits for the listing.
  const selectable = failed === null && (current ? current.error === undefined : query.isFetching)
  const sheet = layout === "sheet"

  return (
    <div className={cn("flex min-h-0 flex-col", sheet && "flex-1")}>
      <div className={cn("shrink-0", sheet ? "px-4 pb-2" : "p-2 pb-1.5")}>
        <div className="relative">
          <Input
            ref={inputRef}
            value={input}
            role="combobox"
            aria-label={t("folders.path")}
            aria-expanded
            aria-controls={listId}
            aria-autocomplete="list"
            aria-activedescendant={active >= 0 ? optionId(active) : undefined}
            data-quiet-focus={quietFocus ? "" : undefined}
            autoCapitalize="none"
            autoComplete="off"
            autoCorrect="off"
            spellCheck={false}
            enterKeyHint="go"
            className="pr-8 font-mono"
            onChange={(e) => change(e.target.value)}
            onKeyDown={onKeyDown}
            onBlur={() => setQuietFocus(false)}
          />
          {query.isFetching && (
            // The list says it is busy; this only shows it.
            <Spinner
              aria-hidden
              className="pointer-events-none absolute top-1/2 right-2.5 size-3.5 -translate-y-1/2 text-muted-foreground"
            />
          )}
        </div>
      </div>
      <div className={cn("flex min-h-24 flex-col border-t", sheet ? "flex-1" : "h-72 shrink")}>
        {startProblem && (
          <p role="status" className={cn("shrink-0 border-b py-1.5 text-xs [overflow-wrap:anywhere] text-muted-foreground", sheet ? "px-4" : "px-3")}>
            {t("folders.startFailed", { path: startProblem.path, reason: pathErrorMessage(startProblem.error) })}
          </p>
        )}
        <div
          ref={listRef}
          id={listId}
          role="listbox"
          aria-label={t("folders.list")}
          aria-busy={query.isFetching || undefined}
          className={cn(
            "min-h-0 flex-1 overflow-y-auto overscroll-contain p-1.5 transition-opacity",
            loadingNext && "opacity-60",
          )}
        >
          {failed !== null ? (
            <Notice tone="error" message={failed}>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onMouseDown={keepFocus}
                onClick={() => void query.refetch()}
              >
                {t("common.tryAgain")}
              </Button>
            </Notice>
          ) : shown === undefined ? (
            <LoadingRows tall={sheet} />
          ) : (
            <>
              {rows.map((row, i) => (
                <FolderRow
                  key={row.kind === "up" ? ".." : row.entry.name}
                  id={optionId(i)}
                  row={row}
                  highlighted={i === active}
                  tall={sheet}
                  onOpen={() => open(row.path)}
                />
              ))}
              {shown.error !== undefined ? (
                <Notice tone="error" message={pathErrorMessage(shown.error)} />
              ) : rows.every((r) => r.kind === "up") ? (
                <Notice
                  message={filter === "" ? t("folders.empty") : t("folders.noMatch", { filter })}
                  hint={filter === "" ? undefined : t("folders.openTyped", { path: input.trim() })}
                />
              ) : null}
            </>
          )}
        </div>
        {shown?.truncated && failed === null && (
          <p className={cn("flex shrink-0 items-center gap-1.5 border-t py-1.5 text-xs text-muted-foreground", sheet ? "px-4" : "px-3")}>
            <InfoIcon className="size-3.5 shrink-0" aria-hidden />
            {t("folders.truncated", { n: shown.entries.length })}
          </p>
        )}
      </div>
      <div
        className={cn(
          "flex shrink-0 gap-2 border-t",
          sheet ? "flex-col px-4 pt-3 pb-[max(1rem,env(safe-area-inset-bottom))]" : "items-center justify-between p-2",
        )}
      >
        <label
          className="flex w-fit cursor-default items-center gap-2 py-1 text-xs text-muted-foreground select-none"
          onMouseDown={keepFocus}
        >
          <Checkbox
            checked={showHidden}
            onMouseDown={keepFocus}
            onCheckedChange={(checked) => {
              const next = checked === true
              setShowHidden(next)
              writeShowHidden(next)
            }}
          />
          {t("folders.showHidden")}
        </label>
        <div className={cn("flex gap-2", sheet ? "*:flex-1" : "justify-end")}>
          <Button type="button" variant="outline" onClick={onCancel}>
            {t("common.cancel")}
          </Button>
          <Button type="button" disabled={!selectable} onClick={select}>
            {selectWhenListed && <Spinner aria-hidden />}
            {t("folders.select")}
          </Button>
        </div>
      </div>
    </div>
  )
}

function FolderRow({
  id,
  row,
  highlighted,
  tall,
  onOpen,
}: {
  id: string
  row: Row
  highlighted: boolean
  /** Touch-sized. */
  tall: boolean
  onOpen: () => void
}) {
  const { t } = useTranslation()
  const entry = row.kind === "dir" ? row.entry : null
  const name = entry?.name ?? ".."
  const Icon = row.kind === "up" ? CornerLeftUpIcon : entry?.gitRepository ? FolderGit2Icon : entry?.symlink ? FolderSymlinkIcon : FolderIcon
  return (
    <div
      id={id}
      role="option"
      aria-selected={highlighted}
      aria-label={row.kind === "up" ? t("folders.parent") : undefined}
      data-highlighted={highlighted || undefined}
      title={entry ? name : undefined}
      className={cn(
        "flex cursor-default items-center gap-2 rounded-md px-2 text-sm select-none hover:bg-foreground/5 data-highlighted:bg-foreground/10",
        tall ? "h-10" : "h-8",
      )}
      onMouseDown={keepFocus}
      onClick={onOpen}
    >
      <Icon className="size-4 shrink-0 text-muted-foreground" aria-hidden />
      <span className="min-w-0 flex-1 truncate">{name}</span>
      {entry?.gitRepository && <span className="sr-only">{t("folders.gitRepository")}</span>}
      {entry?.symlink && (
        <>
          {entry.gitRepository && <Link2Icon className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />}
          <span className="sr-only">{t("folders.symlink")}</span>
        </>
      )}
    </div>
  )
}

/** Placeholder rows while the first listing loads, as tall as real ones. */
function LoadingRows({ tall }: { tall: boolean }) {
  const { t } = useTranslation()
  return (
    <div role="status" aria-label={t("folders.loading")}>
      {[60, 44, 72, 52, 38, 64].map((width, i) => (
        <div key={i} className={cn("flex items-center gap-2 px-2", tall ? "h-10" : "h-8")}>
          <Skeleton className="size-4 shrink-0 rounded-sm bg-foreground/10" />
          <Skeleton className="h-3 bg-foreground/10" style={{ width: `${width}%` }} />
        </div>
      ))}
    </div>
  )
}

function Notice({
  message,
  hint,
  tone,
  children,
}: {
  message: string
  hint?: string
  tone?: "error"
  children?: ReactNode
}) {
  return (
    <div
      role={tone === "error" ? "alert" : "status"}
      className="flex flex-col items-center gap-3 px-4 py-8 text-center text-sm text-muted-foreground"
    >
      {tone === "error" && <CircleAlertIcon className="size-5 text-destructive" aria-hidden />}
      <p className={cn("max-w-sm text-balance", tone === "error" && "text-foreground")}>{message}</p>
      {hint && <p className="max-w-sm text-xs break-all">{hint}</p>}
      {children}
    </div>
  )
}
