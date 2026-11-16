import { useEffect, useState } from "react"
import { useSearchParams } from "react-router"

/**
 * Reads the named query parameters once on mount, then drops them from the
 * URL (a history replace through the router, so its location stays in
 * sync). A reload or a shared link then no longer carries them. Removing
 * them together matters: two separate removals would each start from the
 * same URL and put the other's parameter back.
 */
export function useOneTimeParams<Name extends string>(
  names: readonly Name[],
): Record<Name, string | null> {
  const [params, setParams] = useSearchParams()
  const [values] = useState(
    () => Object.fromEntries(names.map((name) => [name, params.get(name)])) as Record<Name, string | null>,
  )
  const present = names.filter((name) => params.has(name)).join(",")

  useEffect(() => {
    if (!present) return
    setParams(
      (current) => {
        const next = new URLSearchParams(current)
        for (const name of present.split(",")) next.delete(name)
        return next
      },
      { replace: true },
    )
  }, [present, setParams])

  return values
}

/**
 * The `?error=` code the server redirected back with, read once on mount.
 * The parameter is then dropped from the URL, so a reload or a shared link
 * doesn't show the error again.
 */
export function useRedirectErrorCode(): string | null {
  return useOneTimeParams(["error"]).error
}
