import type { CachedAuth } from "@/api/query-client"

/** Decodes each segment the way react-router does, keeping "%2F" encoded. */
function decodePath(pathname: string): string {
  return pathname
    .split("/")
    .map((segment) => {
      try {
        return decodeURIComponent(segment).replaceAll("/", "%2F")
      } catch {
        return segment
      }
    })
    .join("/")
}

/**
 * The form react-router matches a pathname in: percent-decoded,
 * case-insensitive and without a trailing slash. Only for comparisons; never
 * navigate to the result.
 */
export function canonicalPath(pathname: string): string {
  return decodePath(pathname).toLowerCase().replace(/\/+$/, "") || "/"
}

const authPages = new Set(["/login", "/setup"])
const isAuthPage = (pathname: string) => authPages.has(canonicalPath(pathname))

/**
 * Where to send a user who asked for `next` after signing in: a path on this
 * origin, never an auth page, otherwise the home page.
 */
export function safeNext(next: string | null): string {
  if (!next || !next.startsWith("/")) return "/"
  const base = "http://circus.invalid"
  let url: URL
  try {
    url = new URL(next, base)
  } catch {
    return "/"
  }
  // Dot segments can turn "/.//host" into "//host" after parsing, which a
  // browser reads as another origin. Validate what we would navigate to.
  if (url.origin !== base || /^[/\\]{2}/.test(url.pathname) || isAuthPage(url.pathname)) {
    return "/"
  }
  return url.pathname + url.search + url.hash
}

/** The page the auth guard redirects to, or null to render the requested one. */
export function authRedirect(
  auth: CachedAuth,
  { pathname, search }: { pathname: string; search: string },
): string | null {
  const page = canonicalPath(pathname)
  switch (auth.kind) {
    case "needs_setup":
      if (page !== "/setup") return "/setup"
      // Keep the token when moving a variant such as "/Setup/" to "/setup".
      return pathname === "/setup" ? null : `/setup${search}`
    case "signed_out":
      if (page === "/login") return null
      if (page === "/" || page === "/setup" || auth.afterSignOut) return "/login"
      return `/login?next=${encodeURIComponent(pathname + search)}`
    case "signed_in":
      if (page === "/login") return safeNext(new URLSearchParams(search).get("next"))
      if (page === "/setup") return "/"
      return null
  }
}
