// The rendering rules for decision bodies, apart from the components
// (see `components/ledger/ledger-markdown.tsx`): what the sanitizer keeps,
// which links may be links, and the rehype plugin that finds references.

import { defaultRehypePlugins } from "streamdown"

import { tokenizeDecisionRefs } from "./ledger-refs"

/** The tag the references become in the rendered tree. */
export const DECISION_REF_TAG = "decision-ref"
/** The tag images become: a link to the image, never the image itself. */
export const IMAGE_LINK_TAG = "ledger-image"

// The few hast shapes the plugin touches.
interface HastText {
  type: "text"
  value: string
}
interface HastElement {
  type: "element"
  tagName: string
  properties: Record<string, unknown>
  children: HastNode[]
}
type HastNode = HastText | HastElement | { type: "root" | "comment" | "doctype" | "raw"; children?: HastNode[] }

/** Elements whose text is never searched for references. */
const LITERAL = new Set(["code", "pre", "a", "kbd", "samp"])

/**
 * A rehype plugin, run after sanitizing: turns references in text into
 * `decision-ref` elements, except inside code and links, and images into
 * `ledger-image` elements (plain alt text inside a link).
 */
export function rehypeLedger({ projectNames }: { projectNames: readonly string[] }) {
  const walk = (node: HastNode, literal: boolean, inLink: boolean) => {
    if (!("children" in node) || !node.children) return
    const out: HastNode[] = []
    for (const child of node.children) {
      if (child.type === "text" && !literal) {
        for (const token of tokenizeDecisionRefs(child.value, projectNames)) {
          if (token.kind === "text") out.push({ type: "text", value: token.text })
          else
            out.push({
              type: "element",
              tagName: DECISION_REF_TAG,
              properties: { dataNumber: String(token.number), dataProject: token.project ?? "" },
              children: [{ type: "text", value: token.text }],
            })
        }
        continue
      }
      if (child.type === "element" && child.tagName === "img") {
        const alt = typeof child.properties.alt === "string" ? child.properties.alt : ""
        const src = typeof child.properties.src === "string" ? child.properties.src : ""
        out.push(
          inLink
            ? { type: "text", value: alt }
            : { type: "element", tagName: IMAGE_LINK_TAG, properties: { dataSrc: src, dataAlt: alt }, children: [] },
        )
        continue
      }
      if (child.type === "element") {
        walk(child, literal || LITERAL.has(child.tagName), inLink || child.tagName === "a")
      }
      out.push(child)
    }
    node.children = out
  }
  return (tree: HastNode) => walk(tree, false, false)
}

const [sanitizePlugin, streamdownSchema] = defaultRehypePlugins.sanitize as [
  unknown,
  { protocols?: Record<string, string[]> } & Record<string, unknown>,
]

/** rehype-sanitize, as Streamdown bundles it. */
export const sanitize = sanitizePlugin

/** Streamdown's sanitizing schema, with links and images narrowed to what the ledger allows. */
export const sanitizeSchema = {
  ...streamdownSchema,
  protocols: { ...streamdownSchema.protocols, href: ["http", "https", "mailto"], src: ["http", "https"] },
}

/**
 * The rehype plugins a body is rendered with, in order: the sanitizer,
 * then references and images. Without rehype-raw among them, Streamdown
 * shows raw HTML as text.
 */
export function ledgerRehypePlugins(projectNames: readonly string[]): Array<[unknown, unknown]> {
  return [
    [sanitize, sanitizeSchema],
    [rehypeLedger, { projectNames }],
  ]
}

/**
 * Whether an href is a footnote's link within the same body
 * ("#user-content-fn-1", "#user-content-fnref-1"), as remark-rehype makes
 * them.
 */
export const isFootnoteHref = (href: unknown): href is string =>
  typeof href === "string" && /^#user-content-[A-Za-z0-9_-]+$/.test(href)

/**
 * The URL if it may be a link here: absolute http, https or mailto. Null
 * for anything else, relative links included (they point into files that
 * aren't part of the ledger).
 */
export function safeHref(href: unknown): string | null {
  if (typeof href !== "string" || href === "") return null
  try {
    const url = new URL(href)
    return ["http:", "https:", "mailto:"].includes(url.protocol) ? href : null
  } catch {
    return null
  }
}

