// References in a body and their preview. A body may hold tens of
// thousands of references, so each is a plain anchor resolved in O(1)
// (`use-ref-resolver.ts`), and one layer around the whole body handles
// clicks, hover and focus for all of them by delegation.

import { FolderIcon } from "lucide-react"
import {
  useContext,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type FocusEvent,
  type MouseEvent,
  type PointerEvent,
  type ReactNode,
} from "react"
import { createPortal } from "react-dom"
import { useTranslation } from "react-i18next"
import { useNavigate } from "react-router"

import { useDecision } from "@/api/hooks"
import { Skeleton } from "@/components/ui/skeleton"
import { lastInputWasTouch } from "@/lib/input-modality"
import { bodyPreview } from "@/lib/ledger-preview"

import { RefResolverContext } from "./ledger-context"

const OPEN_DELAY_MS = 350
const CLOSE_DELAY_MS = 150
const CARD_GAP_PX = 4
const EDGE_PX = 8

/** The class of a reference's anchor; its look is set once, on the body (`refStyles`). */
export const REF_CLASS = "ledger-ref"

// How references look, set once on the area that holds them rather than on
// each of possibly tens of thousands of anchors.
export const refStyles =
  "[&_.ledger-ref]:rounded-sm [&_.ledger-ref]:font-medium [&_.ledger-ref]:text-foreground [&_.ledger-ref]:underline [&_.ledger-ref]:decoration-foreground/40 [&_.ledger-ref]:decoration-dotted [&_.ledger-ref]:underline-offset-4 [&_.ledger-ref]:outline-none [&_.ledger-ref:hover]:decoration-foreground [&_.ledger-ref:hover]:decoration-solid [&_.ledger-ref:focus-visible]:ring-2 [&_.ledger-ref:focus-visible]:ring-ring/50"

/**
 * One reference: a plain anchor (its href says which decision, its text is
 * the label), muted text when its project or decision doesn't exist, or
 * plain text until that is known.
 */
export function DecisionRefTag({ number, project, children }: { number: number; project: string | null; children: ReactNode }) {
  const resolved = useContext(RefResolverContext)?.resolve(project, number) ?? PENDING
  if (resolved.kind === "pending") return <>{children}</>
  if (resolved.kind === "missing") {
    return (
      <span className="text-muted-foreground" title={resolved.title}>
        {children}
      </span>
    )
  }
  return (
    <a href={resolved.href} className={REF_CLASS}>
      {children}
    </a>
  )
}

const PENDING = { kind: "pending" } as const

interface Target {
  anchor: HTMLAnchorElement
  projectId: string
  number: number
  label: string
}

const DECISION_HREF = /^\/p\/([^/]+)\/ledger\/D([0-9]+)$/

const refAnchor = (el: EventTarget | null): HTMLAnchorElement | null =>
  el instanceof Element ? el.closest<HTMLAnchorElement>(`a.${REF_CLASS}`) : null

/** What the preview needs, read back from the anchor. */
function targetOf(anchor: HTMLAnchorElement): Target | null {
  const match = DECISION_HREF.exec(anchor.getAttribute("href") ?? "")
  if (!match) return null
  return { anchor, projectId: decodeURIComponent(match[1]), number: Number(match[2]), label: anchor.textContent ?? "" }
}

/**
 * Wraps a body: a click on a reference navigates within the app, and
 * hovering or focusing one shows its preview after a moment. On touch
 * screens a tap only follows the link.
 *
 * The preview is visual only: it isn't announced to screen readers, which
 * get the reference's text and can follow the link.
 */
export function RefPreviewLayer({ children }: { children: ReactNode }) {
  const navigate = useNavigate()
  const [target, setTarget] = useState<Target | null>(null)
  const timer = useRef<number | undefined>(undefined)
  const pending = useRef<HTMLAnchorElement | null>(null)

  const schedule = (run: () => void, ms: number) => {
    window.clearTimeout(timer.current)
    timer.current = window.setTimeout(run, ms)
  }
  const open = (anchor: HTMLAnchorElement) => {
    if (target?.anchor === anchor) {
      window.clearTimeout(timer.current)
      return
    }
    pending.current = anchor
    schedule(() => {
      if (pending.current === anchor && anchor.isConnected) setTarget(targetOf(anchor))
    }, OPEN_DELAY_MS)
  }
  const close = () => {
    pending.current = null
    schedule(() => setTarget(null), CLOSE_DELAY_MS)
  }
  const closeNow = () => {
    pending.current = null
    window.clearTimeout(timer.current)
    setTarget(null)
  }
  useEffect(() => () => window.clearTimeout(timer.current), [])

  // Scrolling or resizing moves the reference away from its card.
  useEffect(() => {
    if (!target) return
    const away = () => {
      pending.current = null
      window.clearTimeout(timer.current)
      setTarget(null)
    }
    // Escape closes it wherever focus is (it may have opened on hover).
    const escape = (event: globalThis.KeyboardEvent) => {
      if (event.key === "Escape") away()
    }
    window.addEventListener("scroll", away, true)
    window.addEventListener("resize", away)
    document.addEventListener("keydown", escape)
    return () => {
      window.removeEventListener("scroll", away, true)
      window.removeEventListener("resize", away)
      document.removeEventListener("keydown", escape)
    }
  }, [target])

  const onClick = (event: MouseEvent) => {
    const anchor = refAnchor(event.target)
    if (!anchor || event.defaultPrevented || event.button !== 0) return
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return
    event.preventDefault()
    closeNow()
    void navigate(anchor.getAttribute("href") ?? "")
  }
  const onPointerOver = (event: PointerEvent) => {
    if (event.pointerType === "touch") return
    const anchor = refAnchor(event.target)
    if (anchor) open(anchor)
  }
  const onPointerOut = (event: PointerEvent) => {
    const anchor = refAnchor(event.target)
    if (anchor && !anchor.contains(event.relatedTarget as Node | null)) close()
  }
  const onFocus = (event: FocusEvent) => {
    const anchor = refAnchor(event.target)
    // A tap focuses the link on some touch screens; it should only follow it.
    if (anchor && !lastInputWasTouch()) open(anchor)
  }
  const onBlur = (event: FocusEvent) => {
    if (refAnchor(event.target)) close()
  }
  return (
    <div onClick={onClick} onPointerOver={onPointerOver} onPointerOut={onPointerOut} onFocus={onFocus} onBlur={onBlur}>
      {children}
      {target &&
        createPortal(
          <PreviewCard
            target={target}
            onPointerEnter={() => window.clearTimeout(timer.current)}
            onPointerLeave={close}
          />,
          document.body,
        )}
    </div>
  )
}

/** The card, under its reference (above when there is no room below), inside the window. */
function PreviewCard({
  target,
  onPointerEnter,
  onPointerLeave,
}: {
  target: Target
  onPointerEnter: () => void
  onPointerLeave: () => void
}) {
  const card = useRef<HTMLDivElement>(null)
  const [place, setPlace] = useState<{ top: number; left: number } | null>(null)
  useLayoutEffect(() => {
    const el = card.current
    if (!el) return
    const position = () => {
      const anchor = target.anchor.getBoundingClientRect()
      const { offsetWidth: width, offsetHeight: height } = el
      const below = anchor.bottom + CARD_GAP_PX
      const above = anchor.top - CARD_GAP_PX - height
      const top = below + height > window.innerHeight - EDGE_PX && above >= EDGE_PX ? above : below
      const left = Math.max(EDGE_PX, Math.min(anchor.left, window.innerWidth - width - EDGE_PX))
      setPlace((p) => (p && p.top === top && p.left === left ? p : { top, left }))
    }
    position()
    // The card grows once the decision has loaded.
    if (typeof ResizeObserver === "undefined") return
    const observer = new ResizeObserver(position)
    observer.observe(el)
    return () => observer.disconnect()
  }, [target])
  return (
    <div
      ref={card}
      data-slot="decision-preview"
      onPointerEnter={onPointerEnter}
      onPointerLeave={onPointerLeave}
      style={{ top: place?.top ?? 0, left: place?.left ?? 0, visibility: place ? "visible" : "hidden" }}
      className="fixed z-50 w-80 max-w-[calc(100vw-2rem)] rounded-lg bg-popover p-3 text-sm text-popover-foreground shadow-md ring-1 ring-foreground/10 animate-in fade-in-0 duration-100"
    >
      <DecisionPreview projectId={target.projectId} number={target.number} label={target.label} />
    </div>
  )
}

/** Number, title, directory and the start of the body; loaded once, then cached. */
function DecisionPreview({ projectId, number, label }: { projectId: string; number: number; label: string }) {
  const { t } = useTranslation()
  const decision = useDecision({ projectId, number })
  if (decision.isError) {
    return <p className="text-xs text-muted-foreground">{t("ledger.previewFailed", { ref: label })}</p>
  }
  if (!decision.data) {
    return (
      <div className="flex flex-col gap-2" role="status" aria-label={t("common.loading")}>
        <Skeleton className="h-4 w-3/4" />
        <Skeleton className="h-3 w-1/3" />
        <Skeleton className="h-3 w-full" />
        <Skeleton className="h-3 w-5/6" />
      </div>
    )
  }
  const d = decision.data
  const preview = bodyPreview(d.body)
  return (
    <div className="flex min-w-0 flex-col gap-1.5">
      <p className="flex min-w-0 items-baseline gap-2 font-medium">
        <span className="shrink-0 font-mono text-xs text-muted-foreground">{label}</span>
        <span className="min-w-0 [overflow-wrap:anywhere]">{d.title}</span>
      </p>
      <p className="flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground">
        <FolderIcon className="size-3 shrink-0" aria-hidden />
        <span className="truncate">{d.dir || t("ledger.rootDir")}</span>
      </p>
      {preview && <p className="line-clamp-4 text-xs leading-relaxed text-muted-foreground [overflow-wrap:anywhere]">{preview}</p>}
    </div>
  )
}
