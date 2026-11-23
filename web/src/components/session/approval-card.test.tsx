// @vitest-environment happy-dom
import { cleanup, render, screen } from "@testing-library/react"
import { afterEach, describe, expect, it } from "vitest"

import { TooltipProvider } from "@/components/ui/tooltip"
import { i18n } from "@/i18n/i18n"
import type { ToolCallEvent } from "@/lib/types"

import { ApprovalCard } from "./approval-card"

afterEach(async () => {
  cleanup()
  await i18n.changeLanguage("en")
})

const pending = (reason: string): ToolCallEvent => ({
  kind: "tool_call",
  id: "tool:toolu_1",
  at: "2026-11-23T12:00:00Z",
  tool: "Bash",
  input: { command: "rm -rf build" },
  status: "awaiting_approval",
  approval: { approvalId: "a1", reason, display: "rm -rf build", requestedAtUnixMs: 0 },
})

const show = (event: ToolCallEvent) =>
  render(
    <TooltipProvider>
      <ApprovalCard event={event} deviceName="box" onDecide={() => {}} />
    </TooltipProvider>,
  )

describe("ApprovalCard's reason", () => {
  it("says the permission mode denied the tool when the denial gave no reason", () => {
    show(pending(""))
    expect(screen.getByText("The permission mode denied Bash")).toBeTruthy()
  })

  it("says it in Chinese too", async () => {
    await i18n.changeLanguage("zh-CN")
    show(pending(""))
    expect(screen.getByText("权限模式拒绝了 Bash")).toBeTruthy()
  })

  it("shows a reason of its own as it is, in either language", async () => {
    show(pending("Scripted Bash approval required"))
    expect(screen.getByText("Scripted Bash approval required")).toBeTruthy()
    expect(screen.queryByText(/permission mode/)).toBeNull()
    cleanup()
    await i18n.changeLanguage("zh-CN")
    show(pending("Scripted Bash approval required"))
    expect(screen.getByText("Scripted Bash approval required")).toBeTruthy()
    expect(screen.queryByText(/权限模式/)).toBeNull()
  })
})
