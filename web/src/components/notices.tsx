import { TriangleAlertIcon, XIcon } from "lucide-react"
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from "react"
import { useTranslation } from "react-i18next"

import { Button } from "@/components/ui/button"
import { cn } from "@/lib/utils"

import { NoticeContext, useNoticeList, type Notice, type ShownNotice } from "./notice-context"

/** At most this many notices stay; older ones make way. */
const MAX_NOTICES = 3
/** How long a notice stays once it has been seen. */
export const NOTICE_VISIBLE_MS = 10_000

/**
 * Keeps notices that outlive the page that raised them (such as what
 * happened to a deleted project's folders, after leaving its settings).
 * `NoticeStrip` shows them.
 */
export function NoticeProvider({ children }: { children: ReactNode }) {
  const [list, setList] = useState<ShownNotice[]>([])
  const next = useRef(0)
  const show = useCallback((notice: Notice) => {
    const id = ++next.current
    setList((l) => [...l, { ...notice, id }].slice(-MAX_NOTICES))
  }, [])
  const dismiss = useCallback((id: number) => setList((l) => l.filter((n) => n.id !== id)), [])
  const value = useMemo(() => ({ show, list, dismiss }), [show, list, dismiss])
  return <NoticeContext.Provider value={value}>{children}</NoticeContext.Provider>
}

/**
 * The notices, as strips at the top of the page area, above the page: they
 * push it down rather than cover any of it.
 */
export function NoticeStrip() {
  const { list, dismiss } = useNoticeList()
  const strip = useRef<HTMLDivElement>(null)
  const close = (id: number) => {
    // Focus doesn't drop to the document when its button goes: it goes to
    // the page, which is focusable only until it lets go of focus.
    const main = strip.current?.closest("main")
    if (main && strip.current?.contains(document.activeElement)) {
      main.tabIndex = -1
      main.addEventListener("blur", () => main.removeAttribute("tabindex"), { once: true })
      main.focus({ preventScroll: true })
    }
    dismiss(id)
  }
  return (
    // Stays mounted, so screen readers announce what is added to it.
    <div ref={strip} aria-live="polite" className="flex shrink-0 flex-col">
      {list.map((notice) => (
        <NoticeRow key={notice.id} notice={notice} onClose={() => close(notice.id)} />
      ))}
    </div>
  )
}

/** How long a closing notice takes to fold away. */
export const NOTICE_FOLD_MS = 200

const reducedMotion = () =>
  typeof window.matchMedia === "function" && window.matchMedia("(prefers-reduced-motion: reduce)").matches

function NoticeRow({ notice, onClose }: { notice: ShownNotice; onClose: () => void }) {
  const { t } = useTranslation()
  const { title, lines } = notice.content()
  const [seen, setSeen] = useState(!notice.failure)
  // While the pointer is on it or focus in it, it doesn't go by itself.
  const [hovered, setHovered] = useState(false)
  const [focused, setFocused] = useState(false)
  const [folding, setFolding] = useState(false)

  // A failure counts as seen once the user does anything after it appeared.
  useEffect(() => {
    if (seen) return
    const mark = () => setSeen(true)
    window.addEventListener("pointerdown", mark, { once: true, capture: true })
    window.addEventListener("keydown", mark, { once: true, capture: true })
    return () => {
      window.removeEventListener("pointerdown", mark, { capture: true })
      window.removeEventListener("keydown", mark, { capture: true })
    }
  }, [seen])

  // It folds away rather than vanish, so the page below slides up.
  const latestClose = useRef(onClose)
  useEffect(() => {
    latestClose.current = onClose
  })
  useEffect(() => {
    if (!folding) return
    const timer = window.setTimeout(() => latestClose.current(), reducedMotion() ? 0 : NOTICE_FOLD_MS)
    return () => window.clearTimeout(timer)
  }, [folding])

  // Once seen, it goes by itself after a while (counted again after a pause).
  const paused = hovered || focused
  useEffect(() => {
    if (!seen || paused || folding) return
    const timer = window.setTimeout(() => setFolding(true), NOTICE_VISIBLE_MS)
    return () => window.clearTimeout(timer)
  }, [seen, paused, folding])

  return (
    <div
      className={cn(
        "grid transition-[grid-template-rows,opacity] duration-200 ease-out motion-reduce:transition-none",
        folding ? "grid-rows-[0fr] opacity-0" : "grid-rows-[1fr]",
      )}
      onPointerEnter={() => setHovered(true)}
      onPointerLeave={() => setHovered(false)}
      onFocus={() => setFocused(true)}
      onBlur={(e) => {
        if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setFocused(false)
      }}
    >
      <section
        aria-label={title}
        data-notice={notice.failure ? "failure" : "info"}
        className="flex min-h-0 gap-2 overflow-hidden border-b bg-muted/60 py-2 pr-2 pl-4 text-sm"
      >
        <TriangleAlertIcon
          className={notice.failure ? "mt-0.5 size-4 shrink-0 text-destructive" : "mt-0.5 size-4 shrink-0 text-muted-foreground"}
          aria-hidden
        />
        <div className="flex min-w-0 flex-1 flex-col gap-1">
          <h2 className="font-medium">{title}</h2>
          {lines.map((line, i) => (
            <div key={i} className="flex flex-col gap-1">
              <p className="text-pretty text-muted-foreground [overflow-wrap:anywhere]">{line.text}</p>
              {line.detail && (
                <p className="font-mono text-xs whitespace-pre-wrap text-muted-foreground [overflow-wrap:anywhere]">
                  {line.detail}
                </p>
              )}
            </div>
          ))}
        </div>
        <Button
          variant="ghost"
          size="icon-sm"
          className="-my-0.5 shrink-0"
          aria-label={t("common.close")}
          onClick={() => setFolding(true)}
        >
          <XIcon />
        </Button>
      </section>
    </div>
  )
}
