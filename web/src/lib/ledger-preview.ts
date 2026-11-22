/**
 * The start of a decision's body as plain text, for a preview: Markdown
 * marks, code blocks, tables and raw HTML tags are dropped, links keep
 * their text, and the result is cut at about `max` characters.
 */
export function bodyPreview(body: string, max = 280): string {
  const lines: string[] = []
  let fenced = false
  for (const raw of body.split("\n")) {
    const line = raw.trim()
    if (/^(```|~~~)/.test(line)) {
      fenced = !fenced
      continue
    }
    if (fenced || line.startsWith("|")) continue
    lines.push(line)
  }
  const text = lines
    .join("\n")
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/<\/?[A-Za-z][^>]*>/g, "")
    .replace(/^#{1,6}\s+/gm, "")
    .replace(/^>\s?/gm, "")
    .replace(/^([-*+]|\d+[.)])\s+/gm, "")
    .replace(/(\*\*|__|~~)(.+?)\1/g, "$2")
    .replace(/(\*|`)(.+?)\1/g, "$2")
    .replace(/\s+/g, " ")
    .trim()
  const chars = [...text]
  return chars.length <= max ? text : `${chars.slice(0, max).join("").trimEnd()}…`
}
