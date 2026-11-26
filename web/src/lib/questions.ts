// The model's AskUserQuestion input and the user's answers to it, as the
// control plane carries them (see SessionApproval.input_json and
// ResolveApprovalRequest.answers in proto/circus/v1/session.proto).

import type { Question, QuestionAnswer } from "./types"

/** The SDK tool the model asks the user questions with. */
export const ASK_USER_QUESTION = "AskUserQuestion"

const isObject = (v: unknown): v is Record<string, unknown> => typeof v === "object" && v !== null && !Array.isArray(v)
const str = (v: unknown): string => (typeof v === "string" ? v : "")

/**
 * The questions of an AskUserQuestion input (an object, or its JSON), each
 * option label once. Null when it isn't one: no questions, or a question
 * without text or options.
 */
export function parseQuestions(input: unknown): Question[] | null {
  if (typeof input === "string") {
    try {
      input = JSON.parse(input)
    } catch {
      return null
    }
  }
  if (!isObject(input) || !Array.isArray(input.questions) || input.questions.length === 0) return null
  const questions: Question[] = []
  for (const q of input.questions) {
    if (!isObject(q) || !str(q.question) || !Array.isArray(q.options)) return null
    // A label is what an answer names: only the first option with a label counts.
    const labels = new Set<string>()
    const options = q.options
      .filter(isObject)
      .map((o) => ({ label: str(o.label), description: str(o.description), preview: str(o.preview) }))
      .filter((o) => o.label !== "" && !labels.has(o.label) && labels.add(o.label))
    if (options.length === 0) return null
    questions.push({ question: str(q.question), header: str(q.header), options, multiSelect: q.multiSelect === true })
  }
  return questions
}

/** The answers an APPROVAL_RESOLVED of a question records; malformed entries are dropped. */
export function parseAnswers(value: unknown): QuestionAnswer[] {
  if (!Array.isArray(value)) return []
  return value.filter(isObject).map((a) => ({
    question: str(a.question),
    selected: Array.isArray(a.selected) ? a.selected.filter((s): s is string => typeof s === "string") : [],
    other: str(a.other),
  }))
}

/** An answer says something: an option chosen or text of the user's own. */
export const answered = (a: Pick<QuestionAnswer, "selected" | "other">) =>
  a.selected.length > 0 || a.other.trim() !== ""

/** An answer as one line: the chosen labels, then the user's own text. */
export const answerLine = (answer: Pick<QuestionAnswer, "selected" | "other">) =>
  [...answer.selected, ...(answer.other ? [answer.other] : [])].join(", ")

/** The most an answer's own text may hold, in UTF-8 bytes (QuestionAnswer.other). */
export const MAX_OTHER_BYTES = 4096

/** The UTF-8 length of `text`. */
export const utf8Length = (text: string) => new TextEncoder().encode(text).length
