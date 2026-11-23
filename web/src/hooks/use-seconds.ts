import { useEffect, useRef, useState } from "react"

/** The time by `clock` (the latest one given), read again every `intervalMs` while `active`. */
export function useSeconds(active: boolean, clock: () => number, intervalMs = 1000): number {
  const latest = useRef(clock)
  useEffect(() => {
    latest.current = clock
  })
  const [now, setNow] = useState(clock)
  useEffect(() => {
    if (!active) return
    const tick = () => setNow(latest.current())
    const first = window.setTimeout(tick, 0)
    const timer = window.setInterval(tick, intervalMs)
    return () => {
      window.clearTimeout(first)
      window.clearInterval(timer)
    }
  }, [active, intervalMs])
  return now
}
