import { describe, expect, it } from "vitest"

import type { RawSessionEvent, SessionEventKind } from "@/api/types"
import { fakeWorkerEvents } from "@/test/fake-worker-events"
import { interruptedTurnInput } from "@/lib/turns"
import type { QuestionEvent, SessionEvent } from "@/lib/types"

import { applyEvents, emptyTranscript, type Transcript } from "./transcript"

// Hand-written events of an AskUserQuestion call, in the shapes docs/wire.md
// describes: the tool use, the QUESTION approval, the worker's denial
// (an error tool result for the model), the RESULT, then the user's
// answer (APPROVAL_RESOLVED, naming the answer input) and the message that
// takes it to the model (pushed under that input's id). They cover what
// the recorded scenarios (`ask`, `ask_dismiss`, at the end) don't.

const at = (seq: number, kind: SessionEventKind, payload: object): RawSessionEvent => ({
  seq,
  tsUnixMs: 1_000 * seq,
  kind,
  payloadJson: JSON.stringify(payload),
  ephemeral: false,
  truncated: false,
})

const run = (events: RawSessionEvent[], from: Transcript = emptyTranscript) => applyEvents(from, events)
const kinds = (t: Transcript) => t.events.map((e) => e.kind)
const questionOf = (t: Transcript) => t.events.find((e): e is QuestionEvent => e.kind === "question")!

const input = {
  questions: [
    {
      question: "Which layout?",
      header: "Layout",
      options: [
        { label: "Tabs", description: "One per section", preview: "[A|B|C]" },
        { label: "Single page", description: "Everything at once" },
      ],
      multiSelect: false,
    },
    {
      question: "Which checks?",
      header: "Checks",
      options: [
        { label: "Unit", description: "" },
        { label: "Lint", description: "" },
      ],
      multiSelect: true,
    },
  ],
}

const SENT = "The questions were sent to the user. End your turn."

const start = [
  at(1, "input_accepted", { input_id: "in-1", client_msg_id: "c-1", priority: "normal", text: "ask me" }),
  at(2, "turn_started", { turn_no: "1", origin: "input" }),
  at(3, "user", { type: "user", uuid: "in-1", message: { role: "user", content: "ask me" } }),
]
const toolUse = (seq: number, id = "toolu_q", body: object = input) =>
  at(seq, "assistant", {
    type: "assistant",
    message: { id: `msg_${seq}`, content: [{ type: "tool_use", id, name: "AskUserQuestion", input: body }] },
  })
const pending = (seq: number, approvalId = "ap-1", toolUseId = "toolu_q", extra: object = {}) =>
  at(seq, "approval_pending", {
    approval_id: approvalId,
    kind: "question",
    tool: "AskUserQuestion",
    device: "",
    tool_use_id: toolUseId,
    display: "Which layout?",
    reason: "",
    ...extra,
  })
const denial = (seq: number, id = "toolu_q") =>
  at(seq, "user", {
    type: "user",
    uuid: `u-${seq}`,
    message: { role: "user", content: [{ type: "tool_result", tool_use_id: id, content: SENT, is_error: true }] },
  })
const result = (seq: number) => at(seq, "result", { type: "result", subtype: "success", is_error: false, result: "" })
const answers = [
  { question: "Which layout?", selected: ["Tabs"], other: "" },
  { question: "Which checks?", selected: ["Unit", "Lint"], other: "and e2e" },
]
const answered = (seq: number, approvalId = "ap-1", inputId = "in-2", given: object[] = answers) =>
  at(seq, "approval_resolved", { approval_id: approvalId, kind: "question", state: "answered", answers: given, input_id: inputId })
const ANSWER_TEXT = 'The user answered: "Which layout?"="Tabs"'
const answerMessage = (seq: number, uuid = "in-2") =>
  at(seq, "user", {
    type: "user",
    uuid,
    message: { role: "user", content: ANSWER_TEXT },
    circus_note: "question_answer",
  })

const asked = [...start, toolUse(4), pending(5), denial(6), result(7)]

describe("an AskUserQuestion call", () => {
  it("shows the questions as asked until they reach the user", () => {
    const t = run([...start, toolUse(4)])
    expect(kinds(t)).toEqual(["user_message", "question"])
    expect(questionOf(t)).toMatchObject({
      id: "tool:toolu_q",
      status: "asked",
      questions: [
        {
          question: "Which layout?",
          header: "Layout",
          multiSelect: false,
          options: [
            { label: "Tabs", description: "One per section", preview: "[A|B|C]" },
            { label: "Single page", description: "Everything at once", preview: "" },
          ],
        },
        { question: "Which checks?", header: "Checks", multiSelect: true },
      ],
    })
  })

  it("waits for an answer once its approval is pending, and doesn't show the denial as an error", () => {
    const t = run(asked)
    expect(kinds(t)).toEqual(["user_message", "question"])
    expect(questionOf(t)).toMatchObject({ status: "pending", approvalId: "ap-1" })
    expect(questionOf(t).agentText).toBeUndefined()
    expect(t.events.some((e) => e.kind === "tool_call")).toBe(false)
  })

  it("waits for an answer when the denial arrives before the approval", () => {
    const t = run([...start, toolUse(4), denial(5), pending(6), result(7)])
    expect(questionOf(t)).toMatchObject({ status: "pending", approvalId: "ap-1" })
    expect(questionOf(t).agentText).toBeUndefined()
  })

  it("is a tool approval when the request's kind says tool, whatever the tool", () => {
    for (const kind of ["tool", undefined]) {
      const t = run([...start, toolUse(4), pending(5, "ap-1", "toolu_q", { kind, reason: "The mode asks" })])
      expect(t.events.at(-1)).toMatchObject({
        kind: "tool_call",
        tool: "AskUserQuestion",
        status: "awaiting_approval",
        approval: { approvalId: "ap-1", reason: "The mode asks" },
        input: { questions: [{ question: "Which layout?" }, { question: "Which checks?" }] },
      })
    }
  })

  it("fills in the questions of a request that came before its tool use", () => {
    const early = run([...start, pending(4)])
    expect(questionOf(early)).toMatchObject({ status: "pending", questions: [] })
    const t = run([toolUse(5), denial(6)], early)
    expect(kinds(t)).toEqual(["user_message", "question"])
    expect(questionOf(t)).toMatchObject({ status: "pending", approvalId: "ap-1" })
    expect(questionOf(t).questions.map((q) => q.question)).toEqual(["Which layout?", "Which checks?"])
  })

  it("takes the questions of a request that came before its tool use from the request", () => {
    const t = run([...start, pending(4, "ap-1", "toolu_q", { input_json: JSON.stringify(input) }), toolUse(5)])
    expect(questionOf(t).questions).toHaveLength(2)
  })

  it("stays decided when its request arrives again", () => {
    for (const decided of [answered(8), at(8, "approval_resolved", { approval_id: "ap-1", kind: "question", state: "dismissed" })]) {
      const t = run([...asked, decided, pending(9), pending(0)])
      expect(questionOf(t).status).not.toBe("pending")
    }
  })

  it("collapses to the answers once answered, and shows the answer message as the user's answers", () => {
    const t = run([...asked, answered(8), answerMessage(9)])
    expect(kinds(t)).toEqual(["user_message", "question", "question_answer"])
    expect(questionOf(t)).toMatchObject({ status: "answered", answers })
    expect(t.events[2]).toEqual({
      kind: "question_answer",
      id: "answer:in-2",
      at: expect.any(String),
      answers,
      text: ANSWER_TEXT,
    })
  })

  it("is the same after a replay from any point", () => {
    const all = [...asked, answered(8), answerMessage(9)]
    const whole = run(all)
    for (let i = 1; i < all.length; i++) {
      // The stream reopens after what was applied; the rest arrives again.
      const resumed = run(all, run(all.slice(0, i)))
      expect(resumed.events).toEqual(whole.events)
    }
  })

  it("shows an answer message whose question isn't in the view as its text", () => {
    const t = run([...start, answerMessage(4)])
    expect(t.events.at(-1)).toMatchObject({ kind: "question_answer", answers: null, text: ANSWER_TEXT })
  })

  it("shows an answer message that names no answer input of a question as its text", () => {
    const t = run([...asked, answered(8, "ap-1", "in-2"), answerMessage(9, "in-other")])
    expect(t.events.at(-1)).toMatchObject({ kind: "question_answer", answers: null, text: ANSWER_TEXT })
  })

  it("ends a question still waiting when the session stops", () => {
    const t = run([
      ...asked,
      at(8, "approval_resolved", { approval_id: "ap-1", kind: "question", state: "expired" }),
      at(9, "session_state", { state: "stopped", reason: "user" }),
    ])
    expect(questionOf(t)).toMatchObject({ status: "expired" })
  })

  it.each(["dismissed", "superseded"] as const)("marks a question %s", (state) => {
    const t = run([...asked, at(8, "approval_resolved", { approval_id: "ap-1", kind: "question", state })])
    expect(questionOf(t)).toMatchObject({ status: state })
    expect(questionOf(t).answers).toBeUndefined()
  })

  it("supersedes an older question with a newer one", () => {
    const t = run([
      ...asked,
      at(8, "approval_resolved", { approval_id: "ap-1", kind: "question", state: "superseded" }),
      toolUse(9, "toolu_q2"),
      pending(10, "ap-2", "toolu_q2"),
    ])
    const questions = t.events.filter((e): e is QuestionEvent => e.kind === "question")
    expect(questions.map((q) => [q.id, q.status])).toEqual([
      ["tool:toolu_q", "superseded"],
      ["tool:toolu_q2", "pending"],
    ])
  })

  const second = [{ question: "Which layout?", selected: ["Single page"], other: "" }]
  const replies = (t: Transcript) => t.events.filter((e) => e.kind === "question_answer").map((e) => e.answers)

  it("gives each answer message the answers of its own question, in whatever order they come", () => {
    const t = run([
      ...asked,
      answered(8, "ap-1", "in-2"),
      toolUse(9, "toolu_q2"),
      pending(10, "ap-2", "toolu_q2"),
      answered(11, "ap-2", "in-3", second),
      answerMessage(12, "in-3"),
      answerMessage(13, "in-2"),
    ])
    expect(replies(t)).toEqual([second, answers])
  })

  it("doesn't give a later answer message the answers of one that never came", () => {
    const t = run([
      ...asked,
      answered(8, "ap-1", "in-2"),
      // The first answers' message never reaches the view.
      toolUse(9, "toolu_q2"),
      pending(10, "ap-2", "toolu_q2"),
      answered(11, "ap-2", "in-3", second),
      answerMessage(12, "in-3"),
    ])
    expect(replies(t)).toEqual([second])
  })

  it("doesn't count an earlier turn's text as the failure of the turn the answers started", () => {
    const t = run([
      ...asked.slice(0, -1),
      at(7, "assistant", { type: "assistant", message: { id: "m7", content: [{ type: "text", text: "boom" }] } }),
      result(8),
      answered(9),
      at(10, "turn_started", { turn_no: "2", origin: "input" }),
      answerMessage(11),
      at(12, "result", { type: "result", subtype: "success", is_error: true, result: "boom" }),
    ])
    expect(t.events.find((e) => e.kind === "assistant_text")).not.toHaveProperty("failed")
    expect(t.events.at(-1)).toMatchObject({ kind: "turn_error", message: "boom" })
  })

  it("didn't reach the user when its turn ended without the approval, and keeps what Claude was told", () => {
    const t = run([...start, toolUse(4), denial(5), result(6)])
    expect(questionOf(t)).toMatchObject({ status: "not_sent", agentText: SENT })
  })

  it.each([
    ["turn_stopped", { turn_no: "1", outcome: "stopped" }],
    ["turn_interrupted", { turn_no: "1" }],
    ["session_state", { state: "stopped" }],
  ] as const)("didn't reach the user when %s cut the turn off before the approval", (kind, payload) => {
    const t = run([...start, toolUse(4), at(5, kind, payload)])
    expect(questionOf(t).status).toBe("not_sent")
  })

  it("stays pending when the turn ends after the approval", () => {
    const t = run([...asked, at(8, "turn_stopped", { turn_no: "1", outcome: "stopped" })])
    expect(questionOf(t).status).toBe("pending")
  })

  it("takes the questions from the request when the tool use is not in the view", () => {
    const truncated: RawSessionEvent = { ...toolUse(4), payloadJson: "", truncated: true }
    const t = run([
      ...start,
      truncated,
      pending(5, "ap-1", "toolu_q", { kind: "question", input_json: JSON.stringify(input) }),
    ])
    expect(kinds(t)).toEqual(["user_message", "truncated", "question"])
    expect(questionOf(t)).toMatchObject({ status: "pending", approvalId: "ap-1" })
    expect(questionOf(t).questions).toHaveLength(2)
  })

  it("can only be dismissed when neither the tool use nor the request carry the questions", () => {
    const t = run([...start, pending(4)])
    expect(questionOf(t)).toMatchObject({ status: "pending", approvalId: "ap-1", questions: [] })
  })

  it("is a plain tool call when its input holds no questions", () => {
    const t = run([...start, toolUse(4, "toolu_q", { questions: [] })])
    expect(t.events.at(-1)).toMatchObject({ kind: "tool_call", tool: "AskUserQuestion", status: "running" })
  })

  it("turns a malformed call into a question once its request carries the questions", () => {
    const t = run([
      ...start,
      toolUse(4, "toolu_q", { questions: "nope" }),
      pending(5, "ap-1", "toolu_q", { input_json: JSON.stringify(input) }),
      denial(6),
    ])
    expect(kinds(t)).toEqual(["user_message", "question"])
    expect(questionOf(t)).toMatchObject({ status: "pending" })
  })

  it("gives a tool card that came before its tool use the call's own input", () => {
    const t = run([
      ...start,
      at(4, "approval_pending", {
        approval_id: "ap-r",
        kind: "tool",
        tool: "Write",
        device: "box",
        tool_use_id: "toolu_w",
        display: "/srv/app/config.yaml",
        reason: "",
      }),
      at(5, "assistant", {
        type: "assistant",
        message: {
          id: "m5",
          content: [{ type: "tool_use", id: "toolu_w", name: "Write", input: { file_path: "/srv/app/config.yaml", content: "x: 1" } }],
        },
      }),
      at(6, "user", {
        type: "user",
        uuid: "u6",
        message: { role: "user", content: [{ type: "tool_result", tool_use_id: "toolu_w", content: "sent to the user for approval", is_error: true }] },
      }),
    ])
    expect(kinds(t)).toEqual(["user_message", "tool_call"])
    expect(t.events[1]).toMatchObject({
      tool: "Write",
      input: { file_path: "/srv/app/config.yaml", content: "x: 1" },
      status: "awaiting_approval",
      approval: { approvalId: "ap-r", display: "/srv/app/config.yaml" },
    })
    expect(t.events[1]).not.toHaveProperty("error")
  })

  it("leaves tool approvals as they were", () => {
    const t = run([
      ...start,
      at(4, "assistant", {
        type: "assistant",
        message: { id: "m4", content: [{ type: "tool_use", id: "toolu_b", name: "Bash", input: { command: "git push" } }] },
      }),
      at(5, "approval_pending", { approval_id: "ap-b", tool: "Bash", device: "box", tool_use_id: "toolu_b", display: "git push", reason: "" }),
      at(6, "approval_resolved", { approval_id: "ap-b", state: "denied" }),
    ])
    expect(t.events.at(-1)).toMatchObject({
      kind: "tool_call",
      status: "denied",
      approval: { approvalId: "ap-b", decision: "denied" },
    })
  })
})

describe("an interrupted turn the answers started", () => {
  it("retries with the text Claude was sent", () => {
    const t = run([...asked, answered(8), answerMessage(9), at(10, "turn_interrupted", { turn_no: "2" })])
    expect(interruptedTurnInput(t.events as SessionEvent[])).toBe(ANSWER_TEXT)
  })

  it("has nothing to retry once answers were sent after the interruption", () => {
    const t = run([
      ...start,
      at(4, "turn_interrupted", { turn_no: "1" }),
      toolUse(5),
      pending(6),
      answered(7),
      answerMessage(8),
    ])
    expect(interruptedTurnInput(t.events as SessionEvent[])).toBeNull()
  })
})

describe("the recorded ask scenarios", () => {
  it("asks with the tool use's questions and waits for an answer, without showing the denial", () => {
    const events = fakeWorkerEvents("ask")
    const t = run(events.slice(0, events.findIndex((e) => e.kind === "approval_resolved")))
    expect(kinds(t)).toEqual(["user_message", "question", "assistant_text"])
    const q = questionOf(t)
    expect(q).toMatchObject({ status: "pending", approvalId: expect.any(String) })
    expect(q.agentText).toBeUndefined()
    expect(q.questions).toMatchObject([
      {
        question: "Which color should the button be?",
        header: "Color",
        multiSelect: false,
        options: [
          { label: "Red", description: "A warm color" },
          { label: "Blue", description: "A cool color" },
        ],
      },
      { question: "Which features do you want?", header: "Features", multiSelect: true },
    ])
  })

  it("collapses to the answers and shows the answer message as the user's", () => {
    const t = run(fakeWorkerEvents("ask"))
    expect(kinds(t)).toEqual(["user_message", "question", "assistant_text", "question_answer", "assistant_text"])
    expect(questionOf(t)).toMatchObject({
      status: "answered",
      answers: [
        { question: "Which color should the button be?", selected: ["Blue"], other: "" },
        { question: "Which features do you want?", selected: ["Dark mode", "Export"], other: 'and "keyboard" shortcuts' },
      ],
    })
    expect(t.events[3]).toMatchObject({ kind: "question_answer", text: expect.stringMatching(/^The user answered your questions: /) })
  })

  it("shows the recorded answer message as the answers of its question, paired by the answer input", () => {
    const t = run(fakeWorkerEvents("ask"))
    const message = t.events.find((e) => e.kind === "question_answer")
    expect(message?.answers).toEqual(questionOf(t).answers)
    expect(message?.answers).toHaveLength(2)
  })

  it("is dismissed without an answer message", () => {
    const t = run(fakeWorkerEvents("ask_dismiss"))
    expect(kinds(t)).toEqual(["user_message", "question", "assistant_text"])
    expect(questionOf(t)).toMatchObject({ status: "dismissed" })
  })

  it("is the same after a replay from any point", () => {
    for (const scenario of ["ask", "ask_dismiss"] as const) {
      const all = fakeWorkerEvents(scenario)
      const whole = run(all)
      for (let i = 1; i < all.length; i++) expect(run(all, run(all.slice(0, i))).events).toEqual(whole.events)
    }
  })
})
