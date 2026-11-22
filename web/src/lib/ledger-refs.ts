// Finds references to decisions in plain text, the way GitHub finds "#12":
// "D12" for a decision of the same project and "<project name>#D12" for one
// of another project of the user's.

import { MAX_DECISION_NUMBER } from "./tree"

export type RefToken =
  | { kind: "text"; text: string }
  /** `project` is the project name as written, or null for the same project. */
  | { kind: "ref"; text: string; number: number; project: string | null }

/** How far past a "D<n>" the rest of a project name may reach (64 characters, as UTF-16). */
const NAME_LOOKAHEAD = 128

const CJK = /[\p{Script=Han}\p{Script=Hiragana}\p{Script=Katakana}\p{Script=Hangul}]/u
const LETTER_OR_DIGIT = /[\p{L}\p{N}\p{M}_]/u

/**
 * Whether a character continues a word, so that "D12" next to it is not a
 * reference: letters, digits and "_", except Chinese, Japanese and Korean
 * characters, which are written without spaces ("见D12").
 */
const continuesWord = (ch: string | undefined) => ch !== undefined && LETTER_OR_DIGIT.test(ch) && !CJK.test(ch)

/** What may stand right before a project name: the start, a space, an opening mark or a CJK character. */
const NAME_START = /[\s([{<"'“‘「『（【《,，、;；:：!！?？]/u
const startsName = (ch: string | undefined) => ch === undefined || NAME_START.test(ch) || CJK.test(ch)

/** The character (code point) that ends right before `index`. */
function charBefore(text: string, index: number): string | undefined {
  if (index <= 0) return undefined
  const low = text.charCodeAt(index - 1)
  if (low >= 0xdc00 && low <= 0xdfff && index >= 2) return text.slice(index - 2, index)
  return text[index - 1]
}

function charAt(text: string, index: number): string | undefined {
  const cp = text.codePointAt(index)
  return cp === undefined ? undefined : String.fromCodePoint(cp)
}

/**
 * Splits text into plain parts and decision references. A reference is
 * "D<n>" with n written without leading zeros ("D0" is fine, "D012" is not)
 * and at most the highest decision number, not inside a word; or
 * "<name>#D<n>" where name is exactly one of `projectNames` (the longest
 * that fits). A "#D<n>" after anything else that isn't one of them refers
 * to an unknown project and stays text.
 */
export function tokenizeDecisionRefs(text: string, projectNames: readonly string[] = []): RefToken[] {
  const names = [...new Set(projectNames)].filter((n) => n !== "").sort((a, b) => b.length - a.length)
  const tokens: RefToken[] = []
  let done = 0
  const push = (start: number, end: number, number: number, project: string | null) => {
    if (start > done) tokens.push({ kind: "text", text: text.slice(done, start) })
    tokens.push({ kind: "ref", text: text.slice(start, end), number, project })
    done = end
  }

  /**
   * Whether "D<n>" at [at, end) lies inside a project name that is followed
   * by "#D<n>" ("Plan D2 web#D5"): that reference claims it. The name must
   * start where a name may, after what is already taken.
   */
  const insideRefName = (at: number, end: number) => {
    const hash = text.indexOf("#D", end)
    if (hash < 0 || hash - end > NAME_LOOKAHEAD || !/^#D(0|[1-9][0-9]*)/.test(text.slice(hash, hash + 12))) return false
    return names.some((n) => {
      const start = hash - n.length
      return start <= at && start >= done && text.startsWith(n, start) && startsName(charBefore(text, start))
    })
  }

  for (const match of text.matchAll(/D([0-9]+)/g)) {
    const at = match.index
    const digits = match[1]
    const end = at + match[0].length
    if (continuesWord(charAt(text, end))) continue
    if (digits.length > 1 && digits.startsWith("0")) continue
    const number = Number(digits)
    if (digits.length > 10 || number > MAX_DECISION_NUMBER) continue

    const before = charBefore(text, at)
    if (before === "#") {
      const hash = at - 1
      const name = names.find((n) => {
        const start = hash - n.length
        return start >= done && text.slice(start, hash) === n && startsName(charBefore(text, start))
      })
      if (name !== undefined) {
        push(hash - name.length, end, number, name)
        continue
      }
      // "#D12" alone is this project's; "other#D12" an unknown project's.
      if (!startsName(charBefore(text, hash))) continue
      push(at, end, number, null)
      continue
    }
    if (continuesWord(before)) continue
    // "Plan D2 web#D5": the D2 is part of a project's name, and "#D5" claims it.
    if (insideRefName(at, end)) continue
    push(at, end, number, null)
  }
  if (done < text.length) tokens.push({ kind: "text", text: text.slice(done) })
  return tokens
}
