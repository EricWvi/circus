// Renders app pieces against the in-browser mock backend for DOM tests.
// Tests that use it run in happy-dom (`// @vitest-environment happy-dom`)
// with fake timers; advance them with `advance`.

import { QueryClientProvider } from "@tanstack/react-query"
import { act, render } from "@testing-library/react"
import type { ReactNode } from "react"
import { vi } from "vitest"

import { ClientContext } from "@/api/context"
import { createMockBackend } from "@/api/mock/backend"
import { SEED_OWNER_ID, seedState, type MockState } from "@/api/mock/state"
import { createQueryClient } from "@/api/query-client"
import { TooltipProvider } from "@/components/ui/tooltip"
import { ConversationStoreContext } from "@/state/conversations-context"
import { ConversationsProvider } from "@/state/conversations-provider"
import { DeviceSimulationContext } from "@/state/device-simulation-context"
import type { SimulationTiming } from "@/state/simulated-link"

/** Fast pacing: replies stream for 1 s, outages last 3 s, attempts take 100 ms. */
export const testTiming: SimulationTiming = {
  outageMs: 3_000,
  streamMs: 1_000,
  streamTickMs: 100,
  firstTokenDelayMs: 100,
  attemptLatencyMs: 100,
}

/** Runs timers (and the promises they resolve) for `ms`, inside act. */
export async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

/**
 * Renders `ui` inside the providers a signed-in page has, backed by a mock
 * server holding `state` (the demo seed by default), signed in as its owner.
 */
export async function renderWithBackend(
  ui: ReactNode,
  {
    state = seedState(),
    timing = {},
  }: { state?: MockState; timing?: Partial<SimulationTiming> } = {},
) {
  // Signed in as the demo data's owner (the mock's cookie), without a call
  // that would wait on the fake timers.
  state.cookie = SEED_OWNER_ID
  const backend = createMockBackend({ state, latency: () => 0 })
  const queryClient = createQueryClient()
  const wrap = (children: ReactNode) => (
    <ClientContext.Provider value={backend.client}>
      <QueryClientProvider client={queryClient}>
        <ConversationStoreContext.Provider value={backend.conversations}>
          <DeviceSimulationContext.Provider value={backend.simulation}>
            <TooltipProvider>
              <ConversationsProvider timing={{ ...testTiming, ...timing }}>{children}</ConversationsProvider>
            </TooltipProvider>
          </DeviceSimulationContext.Provider>
        </ConversationStoreContext.Provider>
      </QueryClientProvider>
    </ClientContext.Provider>
  )
  const result = render(wrap(ui))
  await advance(0)
  return { ...result, backend, state, queryClient, rerender: (next: ReactNode) => result.rerender(wrap(next)) }
}
