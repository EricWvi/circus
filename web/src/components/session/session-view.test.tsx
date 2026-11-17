// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { seedState, type MockState } from "@/api/mock/state"
import { SidebarProvider } from "@/components/ui/sidebar"
import { i18n } from "@/i18n/i18n"
import type { SessionEvent } from "@/lib/types"
import { SessionPage } from "@/routes/session-page"
import {
  useConversations,
  useConversationSimulation,
  type ConversationSimulation,
  type ConversationsContextValue,
} from "@/state/conversations-context"
import { advance, renderWithBackend } from "@/test/render"

interface Grabbed {
  conversations: ConversationsContextValue
  simulation: ConversationSimulation
}

/** Hands the test the conversation context, next to the page. */
function Grab({ onRender }: { onRender: (grabbed: Grabbed) => void }) {
  onRender({ conversations: useConversations(), simulation: useConversationSimulation()! })
  return null
}

const at = "2026-11-17T11:00:00Z"
const image = { id: "img", filename: "ci.png", mediaType: "image/png", url: "data:image/png;base64,AA" }

/** The demo data, with `dotfiles-go` reduced to an interrupted turn. */
function interruptedState(): MockState {
  const state = seedState()
  state.conversations["dotfiles-go"] = [
    { kind: "user_message", id: "u1", at, text: "Rewrite install.sh in Go", attachments: [image] },
    { kind: "turn_interrupted", id: "i1", at },
  ]
  return state
}

async function renderSession(path: string, state: MockState = seedState()) {
  const page = (
    <SidebarProvider>
      <SessionPage />
    </SidebarProvider>
  )
  const router = createMemoryRouter([{ path: "/p/:projectId/s/:sessionId", element: page }], {
    initialEntries: [path],
  })
  const grabbed: { current?: Grabbed } = {}
  const result = await renderWithBackend(
    <>
      <RouterProvider router={router} />
      <Grab onRender={(value) => (grabbed.current = value)} />
    </>,
    { state },
  )
  await advance(50)
  return { ...result, router, grabbed }
}

const userMessages = (events: SessionEvent[]) => events.filter((e) => e.kind === "user_message")

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(Date.parse("2026-11-17T12:00:00Z"))
})
afterEach(async () => {
  cleanup()
  vi.useRealTimers()
  await i18n.changeLanguage("en")
})

describe("connection banner", () => {
  it("shows after the grace and stays when the view remounts on another session", async () => {
    const { router, grabbed } = await renderSession("/p/toolbox/s/golangci-v2")
    act(() => grabbed.current!.simulation.dropConnection())
    await advance(1_500)
    expect(screen.queryByRole("button", { name: "Retry now" })).toBeNull()
    await advance(1_000)
    expect(screen.getByRole("button", { name: "Retry now" })).toBeTruthy()

    await act(() => router.navigate("/p/palace/s/palace-new"))
    await advance(0)
    // The loss started 2.5 s ago, not when this view mounted.
    expect(screen.getByRole("button", { name: "Retry now" })).toBeTruthy()
    expect(screen.getByRole("status").textContent).toBe("Reconnecting…")
  })

  it("keeps its live region mounted while connected", async () => {
    await renderSession("/p/toolbox/s/golangci-v2")
    expect(screen.getByRole("status").textContent).toBe("")
  })
})

describe("interrupted turn", () => {
  it("continues with the continue prompt in the active language", async () => {
    await i18n.changeLanguage("zh-CN")
    const { grabbed } = await renderSession("/p/toolbox/s/dotfiles-go", interruptedState())
    fireEvent.click(screen.getByRole("button", { name: "继续" }))
    await advance(0)
    const sent = userMessages(grabbed.current!.conversations.events("dotfiles-go")).at(-1)
    expect(sent).toMatchObject({ text: "继续", attachments: [] })
  })

  it("retries once with the turn's images, and hides the buttons once the session runs", async () => {
    const { grabbed } = await renderSession("/p/toolbox/s/dotfiles-go", interruptedState())
    const retry = screen.getByRole("button", { name: "Retry" })
    fireEvent.click(retry)
    fireEvent.click(retry)
    await advance(0)
    const events = grabbed.current!.conversations.events("dotfiles-go")
    expect(userMessages(events)).toHaveLength(2)
    expect(userMessages(events).at(-1)).toMatchObject({ text: "Rewrite install.sh in Go", attachments: [image] })
    expect(screen.queryByRole("button", { name: "Retry" })).toBeNull()
    expect(screen.queryByRole("button", { name: "Continue" })).toBeNull()
    expect(screen.getByText("Turn interrupted")).toBeTruthy()
  })
})

describe("notices above the composer", () => {
  const broken = "Part of this session's history couldn't be saved"
  const offlineNotice = /is offline \(last seen/

  it("shows broken over device offline", async () => {
    const state = seedState()
    state.devices.find((d) => d.name === "palace-builder")!.online = false
    await renderSession("/p/palace/s/libvips", state)
    expect(screen.getByText(broken)).toBeTruthy()
    expect(screen.queryByText(offlineNotice)).toBeNull()
    expect(screen.getByRole("textbox")).toHaveProperty("disabled", true)
  })

  it("replaces the composer in a stopped session, with no other notice", async () => {
    const state = seedState()
    state.sessions.find((s) => s.sessionId === "libvips")!.state = "stopped"
    state.devices.find((d) => d.name === "palace-builder")!.online = false
    await renderSession("/p/palace/s/libvips", state)
    expect(screen.getByText("This session is stopped.")).toBeTruthy()
    expect(screen.queryByText(broken)).toBeNull()
    expect(screen.queryByText(offlineNotice)).toBeNull()
    expect(screen.queryByRole("textbox")).toBeNull()
  })

  it("shows device offline alone, with the composer enabled", async () => {
    const state = seedState()
    state.devices.find((d) => d.name === "devbox")!.online = false
    await renderSession("/p/toolbox/s/golangci-v2", state)
    expect(screen.getByText(offlineNotice)).toBeTruthy()
    expect(screen.getByRole("textbox")).toHaveProperty("disabled", false)
  })
})
