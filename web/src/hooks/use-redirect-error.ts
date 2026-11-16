import { useEffect, useState } from "react"
import { useSearchParams } from "react-router"

/**
 * The `?error=` code the server redirected back with, read once on mount.
 * The parameter is then dropped from the URL, so a reload or a shared link
 * doesn't show the error again.
 */
export function useRedirectErrorCode(): string | null {
  const [params, setParams] = useSearchParams()
  const [code] = useState(() => params.get("error"))
  const present = params.has("error")

  useEffect(() => {
    if (!present) return
    setParams(
      (current) => {
        const next = new URLSearchParams(current)
        next.delete("error")
        return next
      },
      { replace: true },
    )
  }, [present, setParams])

  return code
}
