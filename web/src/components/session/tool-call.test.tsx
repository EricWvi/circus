// @vitest-environment happy-dom
import { cleanup, fireEvent, render, screen } from "@testing-library/react"
import { afterEach, describe, expect, it } from "vitest"

import { i18n } from "@/i18n/i18n"
import type { ApprovalRequest, ToolCallEvent } from "@/lib/types"

import { ToolCall } from "./tool-call"

afterEach(async () => {
  cleanup()
  await i18n.changeLanguage("en")
})

const approval = (decision: ApprovalRequest["decision"]): ApprovalRequest => ({
  approvalId: "a1",
  reason: "",
  display: "rm -rf build",
  requestedAtUnixMs: 0,
  decision,
})

const denied = (decision: ApprovalRequest["decision"]): ToolCallEvent => ({
  kind: "tool_call",
  id: "tool:toolu_1",
  at: "2026-11-23T12:00:00Z",
  tool: "Bash",
  input: { command: "rm -rf build" },
  status: "denied",
  approval: approval(decision),
})

/** Renders the card and opens it, as a denied card starts closed. */
function open(event: ToolCallEvent) {
  render(<ToolCall event={event} />)
  fireEvent.click(screen.getByRole("button", { name: /rm -rf build/ }))
}

describe("ToolCall after an approval", () => {
  it("reads as approved, and points at the retry, for an approved denial", () => {
    open(denied("approved"))
    expect(screen.getByRole("button", { name: /rm -rf build/ }).textContent).toContain("Approved")
    expect(screen.queryByText("Denied")).toBeNull()
    expect(screen.getByText("Approved by you for one retry")).toBeTruthy()
  })

  it("says the same in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    open(denied("approved"))
    expect(screen.getByRole("button", { name: /rm -rf build/ }).textContent).toContain("已批准")
    expect(screen.getByText("已由你批准重试一次")).toBeTruthy()
  })

  it("reads as denied when the user denied it", async () => {
    open(denied("denied"))
    expect(screen.getByRole("button", { name: /rm -rf build/ }).textContent).toContain("Denied")
    expect(screen.getByText("Denied by you")).toBeTruthy()
    cleanup()
    await i18n.changeLanguage("zh-CN")
    open(denied("denied"))
    expect(screen.getByRole("button", { name: /rm -rf build/ }).textContent).toContain("已拒绝")
    expect(screen.getByText("已由你拒绝")).toBeTruthy()
  })
})
