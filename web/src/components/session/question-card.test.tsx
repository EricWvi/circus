// @vitest-environment happy-dom
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react"
import { afterEach, describe, expect, it, vi } from "vitest"

import { TooltipProvider } from "@/components/ui/tooltip"
import { i18n } from "@/i18n/i18n"
import type { QuestionAnswerEvent, QuestionEvent } from "@/lib/types"

import { QuestionAnswerMessage, QuestionCard } from "./question-card"

afterEach(async () => {
  cleanup()
  await i18n.changeLanguage("en")
})

const question = (over: Partial<QuestionEvent> = {}): QuestionEvent => ({
  kind: "question",
  id: "tool:toolu_q",
  at: "2026-11-26T12:00:00Z",
  status: "pending",
  approvalId: "ap-1",
  questions: [
    {
      question: "Which layout?",
      header: "Layout",
      multiSelect: false,
      options: [
        { label: "Tabs", description: "One per section", preview: "[General|Devices]" },
        { label: "Single page", description: "Everything at once", preview: "General\nDevices" },
        { label: "Sidebar", description: "", preview: "" },
      ],
    },
    {
      question: "Which checks?",
      header: "Checks",
      multiSelect: true,
      options: [
        { label: "Unit", description: "Go and Vitest", preview: "" },
        { label: "Lint", description: "", preview: "" },
      ],
    },
  ],
  ...over,
})

function show(event: QuestionEvent, disabledReason: string | null = null) {
  const onAnswer = vi.fn()
  const onDismiss = vi.fn()
  render(
    <TooltipProvider>
      <QuestionCard event={event} disabledReason={disabledReason} onAnswer={onAnswer} onDismiss={onDismiss} />
    </TooltipProvider>,
  )
  return { onAnswer, onDismiss }
}

const submit = () => screen.getByRole("button", { name: "Send answers" })
const otherField = (i: number) =>
  screen.getByRole("textbox", { name: `Your own answer to: ${["Which layout?", "Which checks?"][i]}` })

describe("a pending question", () => {
  it("shows each question with its header, options and descriptions, as radios or checkboxes", () => {
    show(question())
    expect(screen.getByRole("region", { name: "Claude has 2 questions" })).toBeTruthy()
    const [first, second] = screen.getAllByRole("group")
    expect(within(first).getByText("Layout")).toBeTruthy()
    expect(within(first).getByText("Which layout?")).toBeTruthy()
    expect(within(first).getAllByRole("radio").map((r) => r.textContent ?? r.getAttribute("aria-label"))).toHaveLength(4)
    expect(within(first).getByText("One per section")).toBeTruthy()
    expect(within(second).getAllByRole("checkbox")).toHaveLength(3)
    expect(within(second).getByText("Choose any")).toBeTruthy()
  })

  it("submits one answer per question, in order, once every question has one", () => {
    const { onAnswer } = show(question())
    expect(submit()).toHaveProperty("disabled", true)
    fireEvent.click(screen.getByRole("radio", { name: /^Single page/ }))
    expect(submit()).toHaveProperty("disabled", true)
    fireEvent.click(screen.getByRole("checkbox", { name: /^Unit/ }))
    fireEvent.click(screen.getByRole("checkbox", { name: /^Lint/ }))
    fireEvent.click(screen.getByRole("checkbox", { name: /^Unit/ }))
    fireEvent.click(submit())
    expect(onAnswer).toHaveBeenCalledWith([
      { question: "Which layout?", selected: ["Single page"], other: "" },
      { question: "Which checks?", selected: ["Lint"], other: "" },
    ])
  })

  it("takes the user's own answer in place of an option for a single choice, and beside options for many", () => {
    const { onAnswer } = show(question())
    fireEvent.click(screen.getByRole("radio", { name: /^Tabs/ }))
    // Typing chooses Other.
    fireEvent.change(otherField(0), { target: { value: "  Accordion  " } })
    expect(screen.getByRole("radio", { name: "Other" }).getAttribute("aria-checked")).toBe("true")
    fireEvent.click(screen.getByRole("checkbox", { name: /^Unit/ }))
    fireEvent.change(otherField(1), { target: { value: "E2E" } })
    fireEvent.click(submit())
    expect(onAnswer).toHaveBeenCalledWith([
      { question: "Which layout?", selected: [], other: "Accordion" },
      { question: "Which checks?", selected: ["Unit"], other: "E2E" },
    ])
  })

  it("doesn't count Other chosen without text as an answer", () => {
    show(question())
    fireEvent.click(screen.getByRole("radio", { name: "Other" }))
    fireEvent.click(screen.getByRole("checkbox", { name: /^Lint/ }))
    expect(submit()).toHaveProperty("disabled", true)
    fireEvent.change(otherField(0), { target: { value: "   " } })
    expect(submit()).toHaveProperty("disabled", true)
  })

  it("shows the preview of the focused option, else of the chosen one", () => {
    show(question())
    expect(document.querySelector("[data-preview]")).toBeNull()
    fireEvent.focus(screen.getByRole("radio", { name: /^Single page/ }))
    expect(screen.getByLabelText("Preview of Single page").textContent).toBe("General\nDevices")
    fireEvent.click(screen.getByRole("radio", { name: /^Tabs/ }))
    fireEvent.focus(screen.getByRole("radio", { name: /^Tabs/ }))
    expect(screen.getByLabelText("Preview of Tabs").textContent).toBe("[General|Devices]")
    // An option without a preview shows the chosen one's.
    fireEvent.focus(screen.getByRole("radio", { name: /^Sidebar/ }))
    expect(screen.getByLabelText("Preview of Tabs")).toBeTruthy()
  })

  it("names each choice group by its question", () => {
    show(question())
    expect(screen.getByRole("radiogroup", { name: "Which layout?" })).toBeTruthy()
  })

  it("sends the answers with Enter in a field, once they are complete", () => {
    const { onAnswer } = show(question())
    fireEvent.change(otherField(0), { target: { value: "Accordion" } })
    fireEvent.submit(otherField(0).closest("form")!)
    expect(onAnswer).not.toHaveBeenCalled()
    fireEvent.click(screen.getByRole("checkbox", { name: /^Lint/ }))
    fireEvent.submit(otherField(0).closest("form")!)
    expect(onAnswer).toHaveBeenCalledWith([
      { question: "Which layout?", selected: [], other: "Accordion" },
      { question: "Which checks?", selected: ["Lint"], other: "" },
    ])
  })

  it("holds back an answer of the user's own longer than 4 KiB in UTF-8, and says why", () => {
    const { onAnswer } = show(question())
    fireEvent.click(screen.getByRole("checkbox", { name: /^Lint/ }))
    // 1,366 Chinese characters are 4,098 bytes, though fewer than 4,096 UTF-16 units.
    fireEvent.change(otherField(0), { target: { value: "答".repeat(1366) } })
    expect(submit()).toHaveProperty("disabled", true)
    expect(otherField(0).getAttribute("aria-invalid")).toBe("true")
    expect(screen.getByText("Your own answer is too long (at most 4 KiB).")).toBeTruthy()
    fireEvent.submit(otherField(0).closest("form")!)
    expect(onAnswer).not.toHaveBeenCalled()
    fireEvent.change(otherField(0), { target: { value: "答".repeat(1365) } })
    expect(submit()).toHaveProperty("disabled", false)
    expect(screen.queryByText("Your own answer is too long (at most 4 KiB).")).toBeNull()
  })

  it("lets the keyboard scroll a preview", () => {
    show(question())
    fireEvent.click(screen.getByRole("radio", { name: /^Tabs/ }))
    expect(screen.getByLabelText("Preview of Tabs").getAttribute("tabindex")).toBe("0")
  })

  it("dismisses", () => {
    const { onDismiss, onAnswer } = show(question())
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }))
    expect(onDismiss).toHaveBeenCalledOnce()
    expect(onAnswer).not.toHaveBeenCalled()
  })

  it("can't be answered or dismissed while blocked", () => {
    show(question(), "Disconnected")
    expect(screen.getByRole("button", { name: "Dismiss" })).toHaveProperty("disabled", true)
    expect(submit()).toHaveProperty("disabled", true)
    expect(screen.getByRole("radio", { name: /^Tabs/ })).toHaveProperty("disabled", true)
  })

  it("waits, read-only, until the question reaches the user", () => {
    show(question({ status: "asked", approvalId: undefined }))
    expect(screen.getByText("The questions haven't reached you yet.")).toBeTruthy()
    expect(screen.getByRole("button", { name: "Dismiss" })).toHaveProperty("disabled", true)
  })

  it("can only be dismissed when its questions can't be shown", () => {
    const { onDismiss } = show(question({ questions: [] }))
    expect(screen.getByRole("region", { name: "Claude has a question" })).toBeTruthy()
    expect(screen.getByText("These questions couldn't be shown here. You can dismiss them.")).toBeTruthy()
    expect(screen.queryByRole("button", { name: "Send answers" })).toBeNull()
    fireEvent.click(screen.getByRole("button", { name: "Dismiss" }))
    expect(onDismiss).toHaveBeenCalledOnce()
  })
})

describe("a decided question", () => {
  it("shows each question with its answer, read-only", () => {
    show(
      question({
        status: "answered",
        answers: [
          { question: "Which layout?", selected: ["Tabs"], other: "" },
          { question: "Which checks?", selected: ["Unit", "Lint"], other: "E2E" },
        ],
      }),
    )
    const card = screen.getByRole("region", { name: "Claude has 2 questions" })
    expect(within(card).getByText("Answered")).toBeTruthy()
    const rows = within(card).getAllByRole("listitem").map((li) => li.textContent)
    expect(rows).toEqual(["Which layout?Tabs", "Which checks?Unit, Lint, E2E"])
    expect(within(card).queryByRole("button")).toBeNull()
    expect(within(card).queryByRole("radio")).toBeNull()
  })

  it.each([
    ["dismissed", "Dismissed", "已忽略"],
    ["superseded", "Replaced by a newer question", "已被新的问题取代"],
    ["expired", "Ended when the session stopped", "会话停止时已结束"],
    ["not_sent", "Didn't reach you", "未送达"],
  ] as const)("says it was %s, in either language", async (status, en, zh) => {
    show(question({ status }))
    expect(screen.getByText(en)).toBeTruthy()
    expect(screen.getByText("Which layout?")).toBeTruthy()
    cleanup()
    await i18n.changeLanguage("zh-CN")
    show(question({ status }))
    expect(screen.getByText(zh)).toBeTruthy()
    expect(screen.getByText("Claude 有 2 个问题")).toBeTruthy()
  })

  it("keeps what Claude was told for a question that didn't reach the user", () => {
    show(question({ status: "not_sent", agentText: "The questions could not be sent." }))
    fireEvent.click(screen.getByRole("button", { name: "Told Claude" }))
    expect(screen.getByText("The questions could not be sent.")).toBeTruthy()
  })
})

describe("the answer message", () => {
  const message = (over: Partial<QuestionAnswerEvent>): QuestionAnswerEvent => ({
    kind: "question_answer",
    id: "answer:in-2",
    at: "2026-11-26T12:00:00Z",
    answers: null,
    text: "",
    ...over,
  })

  it("shows the answers, one per question", () => {
    render(
      <QuestionAnswerMessage
        event={message({
          answers: [
            { question: "Which layout?", selected: ["Tabs"], other: "" },
            { question: "Which checks?", selected: ["Unit"], other: "E2E" },
          ],
          text: "raw text for Claude",
        })}
      />,
    )
    const group = screen.getByRole("group", { name: "Your answer" })
    expect([...group.querySelectorAll("p")].map((p) => p.textContent)).toEqual(["Tabs", "Unit, E2E"])
    expect(screen.queryByText("raw text for Claude")).toBeNull()
  })

  it("shows what Claude was sent when the answers aren't known", () => {
    render(<QuestionAnswerMessage event={message({ text: "The user answered: Tabs" })} />)
    expect(screen.getByText("The user answered: Tabs")).toBeTruthy()
  })
})
