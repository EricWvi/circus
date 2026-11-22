// @vitest-environment happy-dom
import { cleanup, fireEvent } from "@testing-library/react"
import { MemoryRouter } from "react-router"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"

import { advance, renderWithBackend } from "@/test/render"

import { LedgerProjectContext } from "./ledger-context"
import { LedgerMarkdown } from "./ledger-markdown"

beforeEach(() => {
  vi.useFakeTimers()
})
afterEach(() => {
  cleanup()
  vi.useRealTimers()
})

/** Wall-clock milliseconds (the fake timers stop `performance.now`). */
const wallMs = () => Number(process.hrtime.bigint() / 1_000_000n)

describe("a large body", () => {
  it("renders thousands of references within budget, each a plain anchor", async () => {
    // 64 KiB with about 6,300 references. With a component, a hover card
    // and a ledger lookup per reference this took over ten seconds here;
    // now it takes well under one.
    const sentence = "See D0 and D1, also palace#D1. "
    const body = sentence.repeat(Math.floor((64 * 1024) / sentence.length))
    const t0 = wallMs()
    const { container } = await renderWithBackend(
      <MemoryRouter>
        <LedgerProjectContext.Provider value="circus">
          <LedgerMarkdown>{body}</LedgerMarkdown>
        </LedgerProjectContext.Provider>
      </MemoryRouter>,
    )
    await advance(50)
    const elapsed = wallMs() - t0

    const refs = container.querySelectorAll("a.ledger-ref")
    expect(refs).toHaveLength(6342)
    // Plain anchors: an href and a class, nothing per reference beyond that.
    expect([...refs[0].attributes].map((a) => a.name).sort()).toEqual(["class", "href"])
    expect(container.querySelectorAll("[data-slot=hover-card-trigger]")).toHaveLength(0)
    // About one element per reference: no wrappers around them.
    expect(container.querySelectorAll("*").length).toBeLessThan(refs.length + 50)
    expect(elapsed).toBeLessThan(4000)

    // One layer previews any of them.
    fireEvent.focus(refs[5000])
    await advance(400)
    await advance(50)
    expect(document.querySelectorAll("[data-slot=decision-preview]")).toHaveLength(1)
    expect(document.querySelector("[data-slot=decision-preview]")?.textContent).toContain("Store originals in object storage")
  })
})
