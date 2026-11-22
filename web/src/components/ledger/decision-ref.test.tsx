// @vitest-environment happy-dom
import { act, cleanup, fireEvent, screen } from "@testing-library/react"
import { createMemoryRouter, RouterProvider } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { advance, renderWithBackend } from "@/test/render"

import { LedgerProjectContext } from "./ledger-context"
import { LedgerMarkdown } from "./ledger-markdown"

const BODY = "See D4, [the web](https://example.com/x) and a note[^1].\n\n[^1]: The note."

async function renderBody() {
  const router = createMemoryRouter(
    [
      {
        path: "*",
        element: (
          <LedgerProjectContext.Provider value="circus">
            <input aria-label="elsewhere" />
            <LedgerMarkdown>{BODY}</LedgerMarkdown>
          </LedgerProjectContext.Provider>
        ),
      },
    ],
    { initialEntries: ["/p/circus/ledger/D6"] },
  )
  await renderWithBackend(<RouterProvider router={router} />)
  await advance(50)
  const ref = screen.getByRole("link", { name: "D4" })
  return { router, ref }
}

const card = () => document.querySelector("[data-slot=decision-preview]")
const where = (router: { state: { location: { pathname: string; hash: string } } }) =>
  router.state.location.pathname + router.state.location.hash

/** Hovers the reference with a mouse until its card shows. */
async function openCard(ref: HTMLElement) {
  fireEvent.pointerOver(ref, { pointerType: "mouse" })
  await advance(400)
  await advance(50)
  expect(card()?.textContent).toContain("Ledger 以 Git 仓库存储")
}

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

describe("clicks in a body", () => {
  it("leave other links to the browser", async () => {
    const { router } = await renderBody()
    for (const link of [screen.getByRole("link", { name: "the web" }), document.querySelector<HTMLElement>('a[href="#user-content-fn-1"]')!]) {
      // Not prevented, and the app doesn't navigate.
      expect(fireEvent.click(link)).toBe(true)
      await advance(10)
      expect(where(router)).toBe("/p/circus/ledger/D6")
    }
  })

  it("leave clicks with a modifier on a reference to the browser", async () => {
    const { router, ref } = await renderBody()
    for (const modifier of ["ctrlKey", "metaKey", "shiftKey", "altKey"]) {
      expect(fireEvent.click(ref, { [modifier]: true })).toBe(true)
    }
    expect(fireEvent.click(ref, { button: 1 })).toBe(true)
    await advance(10)
    expect(where(router)).toBe("/p/circus/ledger/D6")
  })

  it("follow a reference within the app", async () => {
    const { router, ref } = await renderBody()
    expect(fireEvent.click(ref)).toBe(false)
    await advance(10)
    expect(where(router)).toBe("/p/circus/ledger/D4")
  })
})

describe("the preview card", () => {
  it("doesn't open for a finger", async () => {
    const { ref } = await renderBody()
    fireEvent.pointerOver(ref, { pointerType: "touch" })
    await advance(500)
    expect(card()).toBeNull()
  })

  it("closes on Escape, even with focus outside the body", async () => {
    const { ref } = await renderBody()
    await openCard(ref)
    const elsewhere = screen.getByRole("textbox", { name: "elsewhere" })
    act(() => elsewhere.focus())
    fireEvent.keyDown(elsewhere, { key: "Escape" })
    expect(card()).toBeNull()
  })

  it("closes on scroll and on resize", async () => {
    const { ref } = await renderBody()
    await openCard(ref)
    act(() => void window.dispatchEvent(new Event("scroll")))
    expect(card()).toBeNull()
    await openCard(ref)
    act(() => void window.dispatchEvent(new Event("resize")))
    expect(card()).toBeNull()
  })

  it("closes when the reference loses focus", async () => {
    const { ref } = await renderBody()
    act(() => ref.focus())
    await advance(400)
    await advance(50)
    expect(card()).not.toBeNull()
    act(() => ref.blur())
    await advance(200)
    expect(card()).toBeNull()
  })

  it("closes when the pointer leaves the reference, unless it goes onto the card", async () => {
    const { ref } = await renderBody()
    await openCard(ref)
    fireEvent.pointerOut(ref, { pointerType: "mouse", relatedTarget: document.body })
    await advance(200)
    expect(card()).toBeNull()

    await openCard(ref)
    fireEvent.pointerOut(ref, { pointerType: "mouse", relatedTarget: document.body })
    fireEvent.pointerEnter(card()!)
    await advance(200)
    expect(card()).not.toBeNull()
    fireEvent.pointerLeave(card()!)
    await advance(200)
    expect(card()).toBeNull()
  })
})
