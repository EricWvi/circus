// Renders a decision's body. Bodies are written by agents, so they are
// untrusted: raw HTML shows as text, only http, https and mailto links are
// links (opening in a new tab), relative links are plain text and images
// are never loaded, only linked. "D12" and "<project>#D12" become links to
// those decisions (see `decision-ref.tsx`); footnotes link within the body.

import { code } from "@streamdown/code"
import { cjk } from "@streamdown/cjk"
import { ImageIcon } from "lucide-react"
import { memo, useMemo, type ComponentProps, type ReactNode } from "react"
import { useTranslation } from "react-i18next"
import { Streamdown, type Components } from "streamdown"

import { useProjects } from "@/api/hooks"
import { useStreamdownTranslations } from "@/i18n/streamdown"
import { DECISION_REF_TAG, IMAGE_LINK_TAG, isFootnoteHref, ledgerRehypePlugins, safeHref } from "@/lib/ledger-markdown"
import { cn } from "@/lib/utils"

import { DecisionRefTag as DecisionRef, RefPreviewLayer, refStyles } from "./decision-ref"
import { RefResolverContext } from "./ledger-context"
import { useRefResolver } from "./use-ref-resolver"

/** A property hast-util-to-jsx-runtime passed down, by its attribute name. */
const attr = (props: Record<string, unknown>, name: string) => {
  const value = props[name]
  return typeof value === "string" ? value : ""
}

const linkClass =
  "wrap-anywhere font-medium text-foreground underline decoration-foreground/40 underline-offset-4 hover:decoration-foreground rounded-sm outline-none focus-visible:ring-2 focus-visible:ring-ring/50"

function SafeLink({ href, children, ...props }: { href?: string; children?: ReactNode } & Record<string, unknown>) {
  // Footnotes and their way back stay in the page.
  if (isFootnoteHref(href)) {
    const id = attr(props, "id")
    return (
      <a
        href={href}
        id={id.startsWith("user-content-") ? id : undefined}
        aria-label={attr(props, "aria-label") || undefined}
        className={linkClass}
      >
        {children}
      </a>
    )
  }
  const safe = safeHref(href)
  if (safe === null) return <span>{children}</span>
  return (
    <a href={safe} target="_blank" rel="noopener noreferrer" className={linkClass}>
      {children}
    </a>
  )
}

function ImageLink(props: Record<string, unknown>) {
  const { t } = useTranslation()
  const src = safeHref(attr(props, "data-src"))
  const alt = attr(props, "data-alt") || t("ledger.image")
  const content = (
    <>
      <ImageIcon className="mr-1 inline size-3.5 align-[-0.125em]" aria-hidden />
      {alt}
    </>
  )
  if (src === null || src.startsWith("mailto:")) return <span className="text-muted-foreground">{content}</span>
  return (
    <a href={src} target="_blank" rel="noopener noreferrer" className={linkClass}>
      {content}
    </a>
  )
}

function DecisionRefTag(props: Record<string, unknown> & { children?: ReactNode }) {
  const number = Number(attr(props, "data-number"))
  return (
    <DecisionRef number={number} project={attr(props, "data-project") || null}>
      {props.children}
    </DecisionRef>
  )
}

// The custom tags get whatever properties the plugin set, hence the cast.
const components = {
  a: SafeLink,
  [DECISION_REF_TAG]: DecisionRefTag,
  [IMAGE_LINK_TAG]: ImageLink,
} as unknown as Components

const plugins = { cjk, code }

type PluggableList = NonNullable<ComponentProps<typeof Streamdown>["rehypePlugins"]>


// Long words, inline code and headings wrap. Lists and quotes stop
// indenting after a few levels, keep a readable width and scroll inside
// their own box if they still don't fit; tables and code blocks already
// scroll inside theirs.
const contain = [
  "min-w-0 [overflow-wrap:anywhere] [&_li]:min-w-32 [&_blockquote]:min-w-32",
  "[&>ul]:overflow-x-auto [&>ol]:overflow-x-auto [&>blockquote]:overflow-x-auto",
  "[&_li_li_li_li_ul]:pl-0 [&_li_li_li_li_ol]:pl-0",
  "[&_blockquote_blockquote_blockquote_blockquote]:my-0 [&_blockquote_blockquote_blockquote_blockquote]:border-l-0 [&_blockquote_blockquote_blockquote_blockquote]:pl-0",
].join(" ")

/** A decision's body, rendered as untrusted Markdown. */
export const LedgerMarkdown = memo(function LedgerMarkdown({
  children,
  className,
}: {
  children: string
  className?: string
}) {
  const { t, i18n } = useTranslation()
  const translations = useStreamdownTranslations()
  const projects = useProjects()
  const names = useMemo(() => projects.data?.map((p) => p.name) ?? [], [projects.data])
  const rehypePlugins = useMemo(() => ledgerRehypePlugins(names) as PluggableList, [names])
  const remarkRehypeOptions = useMemo(
    () => ({ footnoteLabel: t("ledger.footnotes"), footnoteBackLabel: t("ledger.footnoteBack") }),
    // oxlint-disable-next-line react/exhaustive-deps -- the labels follow the language
    [t, i18n.language],
  )
  const resolver = useRefResolver(children)
  return (
    <RefResolverContext.Provider value={resolver}>
      <RefPreviewLayer>
        <Streamdown
          // Streamdown keeps its parse when only the options change: start
          // over once the project names (which "<project>#D12" needs) are
          // known, and when the language changes.
          key={`${i18n.language}\n${names.join("\n")}`}
          mode="static"
          parseIncompleteMarkdown={false}
          className={cn(
            // Headings in a body stay below the decision's own title.
            "size-full text-sm [&_h1]:text-xl [&_h2]:text-lg [&_h3]:text-base [&_h4]:text-sm [&_h5]:text-sm [&_h6]:text-sm [&>*:first-child]:mt-0 [&>*:last-child]:mb-0",
            contain,
            refStyles,
            className,
          )}
          plugins={plugins}
          rehypePlugins={rehypePlugins}
          remarkRehypeOptions={remarkRehypeOptions}
          components={components}
          translations={translations}
        >
          {children}
        </Streamdown>
      </RefPreviewLayer>
    </RefResolverContext.Provider>
  )
})
