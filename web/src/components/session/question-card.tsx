import { ArrowRightIcon, CheckIcon, CircleSlashIcon, MessageCircleQuestionIcon, ReplaceIcon } from "lucide-react"
import { useId, useState, type ReactNode } from "react"
import { useTranslation } from "react-i18next"

import { Message, MessageContent } from "@/components/ai-elements/message"
import { DisabledReason } from "@/components/disabled-reason"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { Input } from "@/components/ui/input"
import { RadioGroup, RadioGroupItem } from "@/components/ui/radio-group"
import { answered, answerLine, MAX_OTHER_BYTES, utf8Length } from "@/lib/questions"
import type { Question, QuestionAnswer, QuestionAnswerEvent, QuestionEvent } from "@/lib/types"
import { cn } from "@/lib/utils"

import { NoteDisclosure } from "./note-disclosure"

/** What the user has chosen for one question so far. */
interface Draft {
  selected: string[]
  /** "Other" is chosen; its text is the answer. */
  other: boolean
  otherText: string
}

const emptyDraft: Draft = { selected: [], other: false, otherText: "" }

/** The answer a draft gives; a single-select question takes either an option or "Other". */
function answerOf(question: Question, draft: Draft): QuestionAnswer {
  const other = draft.other ? draft.otherText.trim() : ""
  const selected = question.multiSelect || !draft.other ? draft.selected : []
  return { question: question.question, selected, other }
}

/** The radio value of "Other"; option labels are never empty, so it can't clash. */
const OTHER = ""

// The checkbox marks itself checked with Radix's data-state.
const CHECKED = "data-[state=checked]:border-primary data-[state=checked]:bg-primary data-[state=checked]:text-primary-foreground"

/**
 * The model's AskUserQuestion: while it waits for the user, a form with
 * each question's options and a field of their own, which they submit or
 * dismiss; once decided, a read-only summary.
 */
export function QuestionCard({
  event,
  disabledReason = null,
  onAnswer,
  onDismiss,
}: {
  event: QuestionEvent
  /** Why the question can't be answered or dismissed right now; null when it can. */
  disabledReason?: string | null
  onAnswer: (answers: QuestionAnswer[]) => void
  onDismiss: () => void
}) {
  const { status } = event
  if (status === "pending" || status === "asked") {
    return <QuestionForm event={event} disabledReason={disabledReason} onAnswer={onAnswer} onDismiss={onDismiss} />
  }
  return <QuestionSummary event={event} status={status} />
}

function Title({ count, children }: { count: number; children?: ReactNode }) {
  const { t } = useTranslation()
  return (
    <div className="flex flex-wrap items-center gap-x-2 gap-y-1">
      <MessageCircleQuestionIcon className="size-4 shrink-0" aria-hidden />
      <span className="font-medium">{t("session.question.title", { count: Math.max(count, 1) })}</span>
      {children}
    </div>
  )
}

function QuestionForm({
  event,
  disabledReason,
  onAnswer,
  onDismiss,
}: {
  event: QuestionEvent
  disabledReason: string | null
  onAnswer: (answers: QuestionAnswer[]) => void
  onDismiss: () => void
}) {
  const { t } = useTranslation()
  const { questions } = event
  const [drafts, setDrafts] = useState<Draft[]>(() => questions.map(() => emptyDraft))
  const asked = event.status === "asked"
  const blocked = asked ? t("session.question.notReady") : disabledReason
  const answers = questions.map((q, i) => answerOf(q, drafts[i] ?? emptyDraft))
  const complete = questions.length > 0 && answers.every(answered)
  const tooLong = answers.some((a) => utf8Length(a.other) > MAX_OTHER_BYTES)
  let submitBlocked = blocked
  if (submitBlocked === null && tooLong) submitBlocked = t("session.question.otherTooLong")
  if (submitBlocked === null && !complete) submitBlocked = t("session.question.unanswered")
  // The questions can arrive after the card (a request before its tool use).
  const change = (i: number, draft: Draft) =>
    setDrafts((all) => questions.map((_, j) => (j === i ? draft : (all[j] ?? emptyDraft))))

  return (
    <section
      aria-label={t("session.question.title", { count: Math.max(questions.length, 1) })}
      data-question={event.status}
      className="rounded-lg border border-foreground bg-card px-3 py-3 text-sm text-card-foreground"
    >
      {/* Enter in a field sends the answers, as long as Submit could. */}
      <form
        className="flex flex-col gap-4"
        onSubmit={(e) => {
          e.preventDefault()
          if (submitBlocked === null) onAnswer(answers)
        }}
      >
        <Title count={questions.length} />
        {questions.length === 0 && <p className="text-muted-foreground text-pretty">{t("session.question.unshowable")}</p>}
        {questions.map((q, i) => (
          <QuestionFields
            key={i}
            question={q}
            draft={drafts[i] ?? emptyDraft}
            disabled={blocked !== null}
            onChange={(draft) => change(i, draft)}
          />
        ))}
        <div className="flex flex-wrap items-center justify-end gap-2">
          {asked && <p className="mr-auto text-xs text-muted-foreground">{t("session.question.notReady")}</p>}
          <DisabledReason reason={blocked}>
            <Button
              type="button"
              variant="outline"
              className="h-8 px-3 text-sm"
              disabled={blocked !== null}
              onClick={onDismiss}
            >
              {t("session.question.dismiss")}
            </Button>
          </DisabledReason>
          {questions.length > 0 && (
            <DisabledReason reason={submitBlocked}>
              <Button type="submit" className="h-8 px-3 text-sm" disabled={submitBlocked !== null}>
                {t("session.question.submit")}
              </Button>
            </DisabledReason>
          )}
        </div>
      </form>
    </section>
  )
}

function QuestionFields({
  question,
  draft,
  disabled,
  onChange,
}: {
  question: Question
  draft: Draft
  disabled: boolean
  onChange: (draft: Draft) => void
}) {
  const { t } = useTranslation()
  const id = useId()
  // The option whose preview shows: the focused one, else the latest chosen.
  const [focused, setFocused] = useState<string | null>(null)
  const chosen = draft.selected.at(-1)
  const previewed = [focused, chosen]
    .map((label) => question.options.find((o) => o.label === label))
    .find((o) => o !== undefined && o.preview !== "")

  const toggle = (label: string, on: boolean) =>
    onChange({
      ...draft,
      selected: on ? [...draft.selected.filter((l) => l !== label), label] : draft.selected.filter((l) => l !== label),
    })
  const typeOther = (otherText: string) =>
    onChange(
      question.multiSelect
        ? { ...draft, other: otherText.trim() !== "" || draft.other, otherText }
        : { selected: [], other: true, otherText },
    )

  const row =
    "flex cursor-pointer items-start gap-2.5 rounded-md border px-3 py-2 transition-colors hover:bg-muted/50 has-[[data-state=checked]]:border-foreground/50 has-[:disabled]:cursor-not-allowed has-[:disabled]:opacity-60"
  const optionText = (label: string, description: string) => (
    <span className="flex min-w-0 flex-col gap-0.5">
      <span className="font-medium [overflow-wrap:anywhere]">{label}</span>
      {description && <span className="text-xs text-muted-foreground text-pretty">{description}</span>}
    </span>
  )
  const tooLong = utf8Length(answerOf(question, draft).other) > MAX_OTHER_BYTES
  const otherField = (
    <Input
      aria-label={t("session.question.otherFor", { question: question.question })}
      aria-invalid={tooLong || undefined}
      aria-describedby={tooLong ? `${id}-too-long` : undefined}
      placeholder={t("session.question.otherPlaceholder")}
      value={draft.otherText}
      disabled={disabled}
      onFocus={() => setFocused(null)}
      onChange={(e) => typeOther(e.target.value)}
      className="h-7"
    />
  )

  return (
    <fieldset className="flex min-w-0 flex-col gap-2" disabled={disabled}>
      <legend className="mb-2 flex flex-wrap items-center gap-2">
        {question.header && (
          <Badge variant="outline" className="max-w-full truncate rounded-sm">
            {question.header}
          </Badge>
        )}
        <span id={`${id}-q`} className="font-medium text-pretty [overflow-wrap:anywhere]">
          {question.question}
        </span>
        {question.multiSelect && <span className="text-xs text-muted-foreground">{t("session.question.chooseAny")}</span>}
      </legend>
      {question.multiSelect ? (
        <div className="flex flex-col gap-2">
          {question.options.map((o, j) => (
            <label key={j} htmlFor={`${id}-${j}`} className={row}>
              <Checkbox
                id={`${id}-${j}`}
                className={cn("mt-0.5", CHECKED)}
                checked={draft.selected.includes(o.label)}
                disabled={disabled}
                onFocus={() => setFocused(o.label)}
                onCheckedChange={(on) => toggle(o.label, on === true)}
              />
              {optionText(o.label, o.description)}
            </label>
          ))}
          <div className={cn(row, "items-center")}>
            <Checkbox
              id={`${id}-other`}
              className={CHECKED}
              checked={draft.other}
              disabled={disabled}
              aria-label={t("session.question.other")}
              onFocus={() => setFocused(null)}
              onCheckedChange={(on) => onChange({ ...draft, other: on === true })}
            />
            <label htmlFor={`${id}-other`} className="shrink-0 cursor-pointer font-medium">
              {t("session.question.other")}
            </label>
            {otherField}
          </div>
        </div>
      ) : (
        <RadioGroup
          aria-labelledby={`${id}-q`}
          className="flex flex-col gap-2"
          value={draft.other ? OTHER : (draft.selected[0] ?? null)}
          disabled={disabled}
          onValueChange={(value) =>
            onChange(value === OTHER ? { ...draft, selected: [], other: true } : { ...draft, selected: [value], other: false })
          }
        >
          {question.options.map((o, j) => (
            <label key={j} htmlFor={`${id}-${j}`} className={row}>
              <RadioGroupItem id={`${id}-${j}`} value={o.label} className="mt-0.5" onFocus={() => setFocused(o.label)} />
              {optionText(o.label, o.description)}
            </label>
          ))}
          <div className={cn(row, "items-center")}>
            <RadioGroupItem
              id={`${id}-other`}
              value={OTHER}
              aria-label={t("session.question.other")}
              onFocus={() => setFocused(null)}
            />
            <label htmlFor={`${id}-other`} className="shrink-0 cursor-pointer font-medium">
              {t("session.question.other")}
            </label>
            {otherField}
          </div>
        </RadioGroup>
      )}
      {tooLong && (
        <p id={`${id}-too-long`} className="text-xs text-destructive">
          {t("session.question.otherTooLong")}
        </p>
      )}
      {previewed && (
        <pre
          // It scrolls: the keyboard can scroll it too.
          tabIndex={0}
          aria-label={t("session.question.preview", { option: previewed.label })}
          data-preview
          className="max-h-72 overflow-auto rounded-md border bg-muted/40 px-3 py-2 font-mono text-xs leading-relaxed outline-none focus-visible:ring-2 focus-visible:ring-ring/50"
        >
          {previewed.preview}
        </pre>
      )}
    </fieldset>
  )
}

const summaryStates = {
  answered: { key: "session.question.answered", Icon: CheckIcon },
  dismissed: { key: "session.question.dismissed", Icon: CircleSlashIcon },
  superseded: { key: "session.question.superseded", Icon: ReplaceIcon },
  expired: { key: "session.question.expired", Icon: CircleSlashIcon },
  not_sent: { key: "session.question.notSent", Icon: CircleSlashIcon },
} as const

/** A decided question: each question with its answer ("question → answer"), or how it ended. */
function QuestionSummary({ event, status }: { event: QuestionEvent; status: keyof typeof summaryStates }) {
  const { t } = useTranslation()
  const { key, Icon } = summaryStates[status]
  const answers = event.answers ?? []
  return (
    <section
      aria-label={t("session.question.title", { count: Math.max(event.questions.length, 1) })}
      data-question={status}
      className="flex flex-col gap-2 rounded-lg border px-3 py-2 text-sm"
    >
      <Title count={event.questions.length}>
        <span className="ml-auto flex items-center gap-1 text-xs text-muted-foreground">
          <Icon className="size-3 shrink-0" aria-hidden />
          {t(key)}
        </span>
      </Title>
      {event.questions.length > 0 && (
        <ul className="flex flex-col gap-1.5">
          {event.questions.map((q, i) => {
            const answer = answers.find((a) => a.question === q.question) ?? answers[i]
            return (
              <li key={i} className="flex flex-wrap items-baseline gap-x-1.5 gap-y-0.5">
                <span className="text-muted-foreground [overflow-wrap:anywhere]">{q.question}</span>
                {answer && (
                  <span className="flex min-w-0 items-baseline gap-1.5">
                    <ArrowRightIcon className="size-3 shrink-0 self-center text-muted-foreground" aria-hidden />
                    <span className="font-medium [overflow-wrap:anywhere]">{answerLine(answer)}</span>
                  </span>
                )}
              </li>
            )
          })}
        </ul>
      )}
      {status === "not_sent" && event.agentText && (
        <NoteDisclosure label={t("session.question.toldClaude")} text={event.agentText} />
      )}
    </section>
  )
}

/**
 * The message that took the user's answers to Claude: the answers, one per
 * line in the questions' order, as the user's own message.
 */
export function QuestionAnswerMessage({ event }: { event: QuestionAnswerEvent }) {
  const { t } = useTranslation()
  return (
    <Message from="user" data-question-answer role="group" aria-label={t("session.question.yourAnswer")}>
      <MessageContent className="gap-1 break-words whitespace-pre-wrap">
        {event.answers ? event.answers.map((a, i) => <p key={i}>{answerLine(a)}</p>) : <p>{event.text}</p>}
      </MessageContent>
    </Message>
  )
}
