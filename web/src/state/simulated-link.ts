// The simulated transport behind the connection core while sessions are
// simulated: outages on demand, and reconnect attempts that take a moment
// and succeed once the outage is over. The real stream replaces it.

/** How long a simulated reconnect attempt takes. */
export const ATTEMPT_LATENCY_MS = 300

/** How the simulation paces itself; tests shorten it. */
export interface SimulationTiming {
  /** How long a simulated connection outage lasts. */
  outageMs: number
  /** How long a reply takes to stream once it started. */
  streamMs: number
  streamTickMs: number
  firstTokenDelayMs: number
  /** How long a reconnect attempt takes. */
  attemptLatencyMs: number
}

export const defaultTiming: SimulationTiming = {
  outageMs: 10_000,
  streamMs: 2_000,
  streamTickMs: 80,
  firstTokenDelayMs: 500,
  attemptLatencyMs: ATTEMPT_LATENCY_MS,
}

export interface SimulatedLink {
  /** One reconnect attempt, for the connection core. */
  attempt: () => Promise<boolean>
  /** Every attempt fails for `ms` from now; the caller reports the loss. */
  dropFor: (ms: number) => void
}

export function createSimulatedLink({ latencyMs = ATTEMPT_LATENCY_MS } = {}): SimulatedLink {
  let outageUntil = 0
  return {
    attempt: () =>
      new Promise((resolve) => {
        setTimeout(() => resolve(Date.now() >= outageUntil), latencyMs)
      }),
    dropFor(ms) {
      outageUntil = Math.max(outageUntil, Date.now() + ms)
    },
  }
}
