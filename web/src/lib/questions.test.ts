import { describe, expect, it } from "vitest"

import { answerLine, parseQuestions, utf8Length } from "./questions"

describe("parseQuestions", () => {
  it("keeps each option label once, the first option that has it", () => {
    const questions = parseQuestions({
      questions: [
        {
          question: "Proceed?",
          header: "Go",
          options: [
            { label: "Yes", description: "first" },
            { label: "Yes", description: "second" },
            { label: "No", description: "" },
          ],
          multiSelect: false,
        },
      ],
    })
    expect(questions?.[0].options).toEqual([
      { label: "Yes", description: "first", preview: "" },
      { label: "No", description: "", preview: "" },
    ])
  })

  it("reads the input's JSON, and refuses what isn't an AskUserQuestion input", () => {
    const body = { questions: [{ question: "Q?", header: "", options: [{ label: "A" }], multiSelect: true }] }
    expect(parseQuestions(JSON.stringify(body))?.[0]).toMatchObject({ question: "Q?", multiSelect: true })
    expect(parseQuestions("not json")).toBeNull()
    expect(parseQuestions({ questions: [] })).toBeNull()
    expect(parseQuestions({ questions: [{ question: "", options: [{ label: "A" }] }] })).toBeNull()
    expect(parseQuestions({ questions: [{ question: "Q?", options: [{ label: "" }] }] })).toBeNull()
  })
})

describe("answers", () => {
  it("read as the chosen labels, then the user's own text", () => {
    expect(answerLine({ selected: ["A", "B"], other: "and C" })).toBe("A, B, and C")
    expect(answerLine({ selected: [], other: "C" })).toBe("C")
  })

  it("are measured in UTF-8 bytes", () => {
    expect(utf8Length("答")).toBe(3)
    expect(utf8Length("a")).toBe(1)
  })
})
