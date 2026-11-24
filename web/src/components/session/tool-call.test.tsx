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

const agentText =
  "The user doesn't want to proceed with this tool use. Correction to the text above: this call was stopped while running."

const interrupted = (interruption: ToolCallEvent["interruption"]): ToolCallEvent => ({
  kind: "tool_call",
  id: "tool:toolu_2",
  at: "2026-11-23T12:00:00Z",
  tool: "Bash",
  input: { command: "sleep 20" },
  status: "interrupted",
  interruption,
  ...(interruption === "stopped_by_user" ? { agentText } : {}),
})

const header = () => screen.getByRole("button", { name: /sleep 20/ })

describe("ToolCall that was interrupted", () => {
  it("reads as interrupted, starts closed and shows no error", () => {
    render(<ToolCall event={interrupted("stopped_by_user")} />)
    expect(header().textContent).toContain("Interrupted")
    expect(header().textContent).not.toContain("Error")
    expect(header().getAttribute("aria-expanded")).toBe("false")
    expect(screen.queryByText("You stopped this call. It may have run in part.")).toBeNull()
    // A neutral state, not a failure: nothing in the header is red.
    const icons = header().querySelectorAll("svg")
    expect(icons.length).toBeGreaterThan(0)
    for (const icon of icons) expect(icon.getAttribute("class")).not.toContain("text-destructive")
  })

  it("says the user stopped it, shows the command, and keeps what Claude was told behind a disclosure", () => {
    render(<ToolCall event={interrupted("stopped_by_user")} />)
    fireEvent.click(header())
    expect(screen.getByText("You stopped this call. It may have run in part.")).toBeTruthy()
    expect(screen.getByText("Command")).toBeTruthy()
    expect(screen.queryByText("Error")).toBeNull()
    const disclosure = screen.getByRole("button", { name: "Told Claude: the call was stopped" })
    expect(disclosure.getAttribute("aria-expanded")).toBe("false")
    expect(screen.queryByText(agentText)).toBeNull()
    fireEvent.click(disclosure)
    expect(screen.getByText(agentText)).toBeTruthy()
  })

  it("says a cut-off call was interrupted before it finished, with nothing to disclose", () => {
    render(<ToolCall event={interrupted("cut_off")} />)
    expect(header().getAttribute("aria-expanded")).toBe("false")
    fireEvent.click(header())
    expect(screen.getByText("Interrupted before the tool finished. It may have run in part.")).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Told Claude: the call was stopped" })).toBeNull()
  })

  it("says the same in Chinese", async () => {
    await i18n.changeLanguage("zh-CN")
    render(<ToolCall event={interrupted("stopped_by_user")} />)
    expect(header().textContent).toContain("已中断")
    fireEvent.click(header())
    expect(screen.getByText("你停止了这次调用，可能已部分执行。")).toBeTruthy()
    fireEvent.click(screen.getByRole("button", { name: "已告知 Claude：调用已被停止" }))
    expect(screen.getByText(agentText)).toBeTruthy()
    cleanup()
    render(<ToolCall event={interrupted("cut_off")} />)
    fireEvent.click(header())
    expect(screen.getByText("工具结束前被中断，可能已部分执行。")).toBeTruthy()
  })
})
