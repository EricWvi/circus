// @vitest-environment happy-dom
// Guards the devices page's memoization: its status polls render the page
// again, and must not render the add-device dialog with it (every render of
// the dialog renders its Dialog root, which this file counts).
import { cleanup, fireEvent, screen } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { DEVICES_LIVE_INTERVAL_MS } from "@/api/hooks"
import { seedState } from "@/api/mock/state"
import type * as DialogModule from "@/components/ui/dialog"
import { SidebarProvider } from "@/components/ui/sidebar"
import { advance, renderWithBackend } from "@/test/render"

import { DevicesPage } from "./devices-page"

const counts = vi.hoisted(() => ({ dialogRenders: 0 }))

vi.mock("@/components/ui/dialog", async (importOriginal) => {
  const real = await importOriginal<typeof DialogModule>()
  return {
    ...real,
    Dialog: (props: Parameters<typeof real.Dialog>[0]) => {
      counts.dialogRenders++
      return real.Dialog(props)
    },
  }
})

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-22T12:00:00Z"))
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

async function renderPage() {
  const router = createMemoryRouter(
    [
      {
        path: "/devices",
        element: (
          <SidebarProvider>
            <DevicesPage />
          </SidebarProvider>
        ),
      },
    ],
    { initialEntries: ["/devices"] },
  )
  const result = await renderWithBackend(<RouterProvider router={router} />, { state: seedState() })
  await advance(50)
  return result
}

describe("the devices page's polls", () => {
  it("leave the add-device dialog alone, closed or open", async () => {
    const { state } = await renderPage()
    // The page does render again on every poll: last seen times move.
    const before = counts.dialogRenders
    state.devices[0].lastSeenUnixMs -= 1
    await advance(3 * DEVICES_LIVE_INTERVAL_MS)
    expect(counts.dialogRenders).toBe(before)

    fireEvent.click(screen.getByRole("button", { name: "Add device" }))
    await advance(50)
    const open = counts.dialogRenders
    await advance(3 * DEVICES_LIVE_INTERVAL_MS)
    // The dialog follows the devices itself (to see one join), but nothing
    // of the page's polls reaches its root.
    expect(counts.dialogRenders).toBe(open)
  })
})
