import { useLayoutEffect, useRef, useState, type PointerEvent } from "react"

import { useClient } from "@/api/context"
import type { Device, GitRemote } from "@/api/types"
import { checkPath, checkRepoUrl } from "@/api/validation"
import type { LocationRow } from "@/lib/location-form"
import { isPrimaryPress, watchPress } from "@/lib/pointer-press"

/** A repository URL read from a location's folder, as the form shows it. */
export interface RemoteSuggestion {
  /**
   * "filled": the URL is in the field. "differs": the field holds another
   * URL, which the suggestion may replace.
   */
  kind: "filled" | "differs"
  /**
   * The row the folder belongs to. A hint lasts while the row keeps this
   * device and path; a fill note while the field keeps the URL.
   */
  rowKey: string
  device: string
  /** The location's path, trimmed, as it was read. */
  path: string
  url: string
}

/** How a location's path was settled: chosen in the folder picker, or typed and left. */
export type PathCommit = "picked" | "blurred"

const folderKey = (device: string, path: string) => `${device}\0${path}`

/**
 * Reads the git origin URL of a location's folder from its device when the
 * path is picked or typed, and offers it as the project's repository URL:
 * an empty field is filled in (with a note saying where from), a field
 * holding another URL gets a hint instead. Failures of any kind are silent.
 *
 * An answer counts only while its row still has the device and path it was
 * asked for, and only if the field's own rules accept the URL. The latest
 * answer replaces the note or hint. A hint lasts only while its row keeps
 * that device and path; a note, which explains the field's value, lasts
 * until the field changes. A URL this hook put in the field, unedited
 * since, counts as empty.
 *
 * A fill never happens behind the person's back: while saving, for reads
 * sent while a submit button is pressed or before a submit started, when
 * the path was left for a submit button, or while the repository URL
 * field has focus, an answer only hints.
 */
export function useFolderRemote({
  rows,
  devices,
  repoUrl,
  setRepoUrl,
  busy,
  repoUrlId,
}: {
  rows: readonly LocationRow[]
  devices: readonly Device[] | undefined
  repoUrl: string
  /** Sets the field's value, as an edit would. */
  setRepoUrl: (value: string) => void
  /** Saving: the field is read-only. */
  busy: boolean
  /** The id of the repository URL input. */
  repoUrlId: string
}) {
  const client = useClient()
  const [suggestion, setSuggestion] = useState<RemoteSuggestion | null>(null)
  // By row key: the folder whose answer was last taken.
  const lastRead = useRef(new Map<string, string>())
  // Requests in flight by folder, and the rows waiting on each.
  const inFlight = useRef(new Map<string, Promise<GitRemote>>())
  const waiting = useRef(new Set<string>())
  // The URL this hook put in the field, while nobody edited it.
  const ownFill = useRef<string | null>(null)
  // Counts submits, so a read can tell whether one started since it was sent.
  const submits = useRef(0)
  // A submit button is pressed: the path's blur may come before the click
  // (where buttons don't take focus on click, without a related target).
  const submitPressed = useRef(false)
  // Answers arrive after later renders; they compare against the form as it is then.
  const latest = useRef({ rows, repoUrl, setRepoUrl, busy })
  useLayoutEffect(() => {
    latest.current = { rows, repoUrl, setRepoUrl, busy }
  })

  const answer = (next: Omit<RemoteSuggestion, "kind">, hintOnly: boolean) => {
    const now = latest.current
    const current = now.repoUrl.trim()
    const own = ownFill.current !== null && now.repoUrl === ownFill.current
    if (current === next.url) {
      // The field already has it. A note follows the latest source of the
      // value it explains; anything this row said before no longer applies.
      setSuggestion((s) =>
        own || s?.kind === "filled" ? { ...next, kind: "filled" } : s?.rowKey === next.rowKey ? null : s,
      )
      return
    }
    const focused = typeof document !== "undefined" && document.activeElement?.id === repoUrlId
    if ((current === "" || own) && !hintOnly && !now.busy && !focused) {
      now.setRepoUrl(next.url)
      // Another answer in the same tick sees the field filled.
      now.repoUrl = next.url
      ownFill.current = next.url
      setSuggestion({ ...next, kind: "filled" })
    } else {
      setSuggestion({ ...next, kind: "differs" })
    }
  }

  const read = (key: string, device: string, path: string, leavingToSubmit: boolean) => {
    const os = devices?.find((d) => d.name === device)?.os
    if (os === undefined) return
    // Paths the form would refuse, and the default directory, aren't read.
    const checked = checkPath(path, os)
    if (!checked.ok || checked.value === "") return
    const asked = path.trim()
    const folder = folderKey(device, asked)
    const rowWait = `${key}\0${folder}`
    if (waiting.current.has(rowWait)) return
    // One request per folder at a time; rows asking for the same one share it.
    let request = inFlight.current.get(folder)
    if (!request) {
      request = client.machines.getGitRemote({ device, path: asked })
      const settled = request
      inFlight.current.set(folder, settled)
      const forget = () => {
        if (inFlight.current.get(folder) === settled) inFlight.current.delete(folder)
      }
      settled.then(forget, forget)
    }
    waiting.current.add(rowWait)
    const submitsAtSend = submits.current
    const hintOnly = leavingToSubmit || submitPressed.current
    request
      .then(
        (remote) => {
          const row = latest.current.rows.find((r) => r.key === key)
          if (!row || row.device !== device || row.path.trim() !== asked) return
          lastRead.current.set(key, folder)
          if (remote.error !== undefined || remote.url === "") return
          const url = checkRepoUrl(remote.url)
          if (!url.ok || url.value === "") return
          answer(
            { rowKey: key, device, path: asked, url: url.value },
            hintOnly || submits.current !== submitsAtSend,
          )
        },
        () => {
          const row = latest.current.rows.find((r) => r.key === key)
          if (row && row.device === device && row.path.trim() === asked) lastRead.current.set(key, folder)
        },
      )
      .finally(() => waiting.current.delete(rowWait))
  }

  // What the field shows under it now: a fill note while the field holds
  // its URL; a hint while the field differs from it and the hint's row
  // still has that folder.
  const source = suggestion && rows.find((r) => r.key === suggestion.rowKey)
  const shown =
    suggestion === null
      ? null
      : suggestion.kind === "filled"
        ? repoUrl.trim() === suggestion.url
          ? suggestion
          : null
        : source &&
            source.device === suggestion.device &&
            source.path.trim() === suggestion.path &&
            repoUrl.trim() !== suggestion.url
          ? suggestion
          : null

  return {
    suggestion: shown,
    /**
     * A location's path was picked, or typed and left. `leavingToSubmit`:
     * focus went to a submit button, so the answer may only hint.
     */
    pathCommitted(row: Pick<LocationRow, "key" | "device" | "path">, how: PathCommit, leavingToSubmit = false) {
      if (how === "blurred" && lastRead.current.get(row.key) === folderKey(row.device, row.path.trim())) return
      read(row.key, row.device, row.path, leavingToSubmit)
    },
    /** The person edits the field: a fill note no longer applies, and the value is theirs. */
    editRepoUrl(value: string) {
      setRepoUrl(value)
      ownFill.current = null
      if (suggestion?.kind === "filled") setSuggestion(null)
    },
    /** Puts the hinted URL into the field, as the person's own choice. */
    applySuggested() {
      if (busy || shown?.kind !== "differs") return
      setRepoUrl(shown.url)
      ownFill.current = null
      setSuggestion({ ...shown, kind: "filled" })
    },
    /** A submit started: answers to reads already sent may only hint. */
    submitStarted() {
      submits.current++
    },
    /**
     * The form's pointerdown (capture): pressing a submit button with the
     * primary button makes reads sent during the press hint-only.
     */
    formPointerDown(event: PointerEvent) {
      if (!isPrimaryPress(event)) return
      if (!(event.target instanceof Element) || event.target.closest("button[type=submit]") === null) return
      submitPressed.current = true
      // Cleared once the press is over (after its click has submitted).
      watchPress(() => (submitPressed.current = false))
    },
    /** The form starts over (saved, reloaded): notes, hints and fills are forgotten. */
    reset() {
      setSuggestion(null)
      ownFill.current = null
      lastRead.current.clear()
    },
  }
}
