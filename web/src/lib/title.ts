import { i18n } from "@/i18n/i18n"

export const TITLE_MAX = 60

/**
 * A session title from its first message: the first non-empty line with
 * whitespace collapsed, cut at a word boundary to at most TITLE_MAX
 * characters. Null when the message has no text.
 */
export function titleFromMessage(text: string): string | null {
  const line = text
    .split("\n")
    .map((l) => l.replace(/\s+/g, " ").trim())
    .find((l) => l !== "")
  if (!line) return null
  const chars = [...line]
  if (chars.length <= TITLE_MAX) return line
  const cut = chars.slice(0, TITLE_MAX - 1).join("")
  const space = cut.lastIndexOf(" ")
  return `${(space >= TITLE_MAX / 2 ? cut.slice(0, space) : cut).trimEnd()}…`
}

/** A session's title as shown: the server leaves it empty until the first message. */
export function sessionTitle(title: string): string {
  return title === "" ? i18n.t("session.untitled") : title
}
