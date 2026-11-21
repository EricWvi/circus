// Renders app pieces against the in-browser mock backend for DOM tests.
// Tests that use it run in happy-dom (`// @vitest-environment happy-dom`)
// with fake timers; advance them with `advance`.

import { QueryClientProvider } from "@tanstack/react-query"
import { act, render } from "@testing-library/react"
import type { ReactNode } from "react"
import { onTestFinished, vi } from "vitest"

import { ClientContext } from "@/api/context"
import { createMockBackend } from "@/api/mock/backend"
import type { MockSessionTiming } from "@/api/mock/sessions"
import { SEED_OWNER_ID, seedState, type MockState } from "@/api/mock/state"
import { createQueryClient, followUnauthenticated } from "@/api/query-client"
import type { CircusClient } from "@/api/types"
import { NoticeProvider, NoticeStrip } from "@/components/notices"
import { TooltipProvider } from "@/components/ui/tooltip"
import { OutboxProvider } from "@/state/outbox-provider"

/** Fast pacing for the mock's scripted replies. */
export const testSessionTiming: MockSessionTiming = { stepMs: 100, chunkMs: 50 }

/** Runs timers (and the promises they resolve) for `ms`, inside act. */
export async function advance(ms: number) {
  await act(async () => {
    await vi.advanceTimersByTimeAsync(ms)
  })
}

/**
 * Renders `ui` inside the providers a signed-in page has, backed by a mock
 * server holding `state` (the demo seed by default), signed in as its owner.
 * `wrapClient` stands between the pages and the mock, to make calls fail.
 */
export async function renderWithBackend(
  ui: ReactNode,
  {
    state = seedState(),
    wrapClient = (client) => client,
  }: {
    state?: MockState
    wrapClient?: (client: CircusClient) => CircusClient
  } = {},
) {
  // Signed in as the demo data's owner (the mock's cookie), without a call
  // that would wait on the fake timers.
  state.cookie = SEED_OWNER_ID
  const backend = createMockBackend({ state, latency: () => 0, sessionTiming: testSessionTiming })
  const queryClient = createQueryClient()
  // As ApiProvider does; the test's cleanup unmounts the tree, not this.
  onTestFinished(followUnauthenticated(queryClient))
  const client = wrapClient(backend.client)
  const wrap = (children: ReactNode) => (
    <ClientContext.Provider value={client}>
      <QueryClientProvider client={queryClient}>
        <TooltipProvider>
          <OutboxProvider>
            <NoticeProvider>
              <NoticeStrip />
              {children}
            </NoticeProvider>
          </OutboxProvider>
        </TooltipProvider>
      </QueryClientProvider>
    </ClientContext.Provider>
  )
  const result = render(wrap(ui))
  await advance(0)
  return { ...result, backend, state, queryClient, client, rerender: (next: ReactNode) => result.rerender(wrap(next)) }
}
