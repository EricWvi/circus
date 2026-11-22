import { createContext } from "react"

/** The project whose decision is being shown: plain "D12" refers into it. */
export const LedgerProjectContext = createContext<string | null>(null)

/** What a reference in a body points at, as far as is known. */
export type ResolvedRef =
  /** A decision that exists (or whose ledger couldn't be read): a link. */
  | { kind: "link"; href: string; projectId: string; label: string }
  /** No such project or decision: muted text, with why as its title. */
  | { kind: "missing"; title: string }
  /** Not known yet: plain text. */
  | { kind: "pending" }

export interface RefResolver {
  /** `project` is the name as written, null for the project being shown. O(1). */
  resolve(project: string | null, number: number): ResolvedRef
}

/** Resolves the references of the body being rendered; null outside one. */
export const RefResolverContext = createContext<RefResolver | null>(null)
