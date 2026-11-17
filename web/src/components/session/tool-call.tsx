import type { ReactNode } from "react"
import { useTranslation } from "react-i18next"
import type { BundledLanguage } from "shiki"

import { CodeBlock } from "@/components/ai-elements/code-block"
import {
  Tool,
  ToolContent,
  ToolHeader,
  ToolInput,
  ToolOutput,
} from "@/components/ai-elements/tool"
import { INTERRUPTED_TOOL_ERROR } from "@/lib/settle"
import type { ToolCallEvent } from "@/lib/types"

import { toToolPartState } from "./tool-state"

const languageByExtension: Record<string, BundledLanguage> = {
  go: "go",
  json: "json",
  ts: "typescript",
  tsx: "tsx",
  js: "javascript",
  md: "markdown",
  sh: "bash",
  yaml: "yaml",
  yml: "yaml",
}

function languageFor(path: string): BundledLanguage {
  const ext = path.split(".").pop() ?? ""
  return languageByExtension[ext] ?? "log"
}

/** Strip `cat -n` style line-number prefixes, as returned by the Read tool. */
function stripLineNumbers(text: string): string {
  const lines = text.split("\n")
  const numbered = /^\s*\d+\t/
  if (!lines.every((l) => numbered.test(l))) return text
  return lines.map((l) => l.replace(numbered, "")).join("\n")
}

function toDiff(oldText: string, newText: string): string {
  const minus = oldText.split("\n").map((l) => `-${l}`)
  const plus = newText.split("\n").map((l) => `+${l}`)
  return [...minus, ...plus].join("\n")
}

const str = (v: unknown): string => (typeof v === "string" ? v : "")

function summary(event: ToolCallEvent): string {
  const { tool, input } = event
  switch (tool) {
    case "Bash":
      return str(input.command)
    case "Read":
    case "Edit":
    case "Write":
      return str(input.file_path)
    default:
      return ""
  }
}

function Section({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="space-y-2">
      <h4 className="text-xs font-medium tracking-wide text-muted-foreground uppercase">{label}</h4>
      {children}
    </div>
  )
}

function Body({ event }: { event: ToolCallEvent }) {
  const { t } = useTranslation()
  const { tool, input, output, error } = event

  const errorBlock = error ? (
    <ToolOutput output={undefined} errorText={error} className="[&>div]:p-3" />
  ) : null

  switch (tool) {
    case "Bash":
      return (
        <>
          <Section label={t("session.tool.command")}>
            <CodeBlock code={str(input.command)} language="bash" />
          </Section>
          {output !== undefined && (
            <Section label={t("session.tool.output")}>
              <CodeBlock code={output} language="log" />
            </Section>
          )}
          {errorBlock}
        </>
      )
    case "Read":
      return (
        <>
          {output !== undefined && (
            <CodeBlock
              code={stripLineNumbers(output)}
              language={languageFor(str(input.file_path))}
              showLineNumbers
            />
          )}
          {errorBlock}
        </>
      )
    case "Edit":
      return (
        <>
          <CodeBlock code={toDiff(str(input.old_string), str(input.new_string))} language="diff" />
          {errorBlock}
        </>
      )
    default:
      return (
        <>
          <ToolInput input={input} />
          <ToolOutput output={output} errorText={error} />
        </>
      )
  }
}

export function ToolCall({ event }: { event: ToolCallEvent }) {
  const { t } = useTranslation()
  const target = summary(event)
  const decision = event.approval?.decision

  return (
    <Tool className="mb-0" defaultOpen={event.status === "error" || event.status === "running"}>
      <ToolHeader
        type="dynamic-tool"
        toolName={event.tool}
        state={toToolPartState(event.status)}
        title={target ? `${event.tool}  ${target}` : event.tool}
        className="whitespace-pre"
      />
      <ToolContent className="space-y-3 border-t p-3">
        {decision && (
          <p className="text-xs text-muted-foreground">
            {t(decision === "approved" ? "session.tool.approvedByYou" : "session.tool.deniedByYou")}
          </p>
        )}
        <Body
          event={
            // The client's own note for a tool call it gave up on; any other
            // error text comes from the agent and is shown as it is.
            event.error === INTERRUPTED_TOOL_ERROR
              ? { ...event, error: t("session.tool.interrupted") }
              : event
          }
        />
      </ToolContent>
    </Tool>
  )
}
