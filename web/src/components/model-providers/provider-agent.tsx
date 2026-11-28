import claudeCodeMark from "@lobehub/icons-static-svg/icons/claudecode.svg"
import codexMark from "@lobehub/icons-static-svg/icons/codex.svg"
import opencodeMark from "@lobehub/icons-static-svg/icons/opencode.svg"
import { RadioGroup as RadioGroupPrimitive } from "radix-ui"
import { useId, type CSSProperties } from "react"
import { useTranslation } from "react-i18next"

import { cn } from "@/lib/utils"

/** The agents the provider dialog offers, in its order. Only Claude Code has providers so far. */
const PROVIDER_AGENTS = ["claude_code", "codex", "opencode"] as const
export type ProviderAgent = (typeof PROVIDER_AGENTS)[number]

/**
 * Each agent's mark: a one-colour glyph used as a mask, painted in `color`.
 * Claude Code's is its brand orange (the fill of the package's
 * claudecode-color.svg); Codex's and opencode's marks are monochrome, so
 * they take the text colour and stay readable on both themes.
 */
const MARKS: Record<ProviderAgent, { src: string; color: string }> = {
  claude_code: { src: claudeCodeMark, color: "#d97757" },
  codex: { src: codexMark, color: "var(--foreground)" },
  opencode: { src: opencodeMark, color: "var(--foreground)" },
}

/**
 * An agent's mark, decorative: the name next to it carries the meaning.
 * `muted` paints it in the muted grey at 70% opacity (the same for every
 * mark), back to its own colour while its `group` is hovered or focused;
 * `"always"` keeps it grey (an agent that can't be chosen).
 * In forced colours a mask's background would be forced to Canvas and
 * vanish, so the mark paints itself in a system colour: Highlight when
 * `selected`, else CanvasText.
 */
export function AgentMark({
  agent,
  muted = false,
  selected = false,
  className,
}: {
  agent: ProviderAgent
  muted?: boolean | "always"
  selected?: boolean
  className?: string
}) {
  const mark = MARKS[agent]
  const mask = `url("${mark.src}")`
  const style = {
    "--mark": mark.color,
    maskImage: mask,
    maskSize: "contain",
    maskRepeat: "no-repeat",
    maskPosition: "center",
    // Older Chromium and Safari know only the prefixed properties.
    WebkitMaskImage: mask,
    WebkitMaskSize: "contain",
    WebkitMaskRepeat: "no-repeat",
    WebkitMaskPosition: "center",
  } as CSSProperties
  return (
    <span
      aria-hidden
      data-agent-mark={agent}
      className={cn(
        "size-5 shrink-0 bg-(--mark) transition-[background-color,opacity]",
        muted && "bg-muted-foreground opacity-70",
        muted === true &&
          "group-hover:bg-(--mark) group-hover:opacity-100 group-focus-visible:bg-(--mark) group-focus-visible:opacity-100",
        "forced-colors:opacity-100 forced-colors:[forced-color-adjust:none]",
        selected ? "forced-colors:bg-[Highlight]" : "forced-colors:bg-[CanvasText]",
        className,
      )}
      style={style}
    />
  )
}

/**
 * The agent a new provider is for: one radio group, at the top of the form.
 * Arrow keys move and select, as in any radio group; Space and Enter select
 * the focused option.
 */
export function AgentChoice({
  value,
  onChange,
  disabled = false,
  describedBy,
}: {
  value: ProviderAgent
  onChange: (agent: ProviderAgent) => void
  /** While the form is saving. */
  disabled?: boolean
  /** The id of what the chosen option says about it. */
  describedBy?: string
}) {
  const { t } = useTranslation()
  const labelId = useId()
  return (
    <div className="flex flex-col gap-2">
      <span id={labelId} className="text-sm leading-snug font-medium">
        {t("providers.form.agent")}
      </span>
      <RadioGroupPrimitive.Root
        aria-labelledby={labelId}
        value={value}
        disabled={disabled}
        onValueChange={(v) => {
          const agent = PROVIDER_AGENTS.find((a) => a === v)
          if (agent) onChange(agent)
        }}
        loop
        // Stacked on narrow screens, so every name shows in full.
        className="grid gap-2 sm:grid-cols-3"
      >
        {PROVIDER_AGENTS.map((agent) => {
          const selected = agent === value
          return (
            <RadioGroupPrimitive.Item
              key={agent}
              value={agent}
              aria-describedby={selected ? describedBy : undefined}
              // A radio group leaves Enter alone; here it selects too, and
              // never submits the form.
              onKeyDown={(event) => {
                if (event.key !== "Enter" || disabled) return
                event.preventDefault()
                onChange(agent)
              }}
              className={cn(
                "group flex min-w-0 items-center gap-2 text-sm transition-colors sm:justify-center",
                // Forced colours drop the ring: an outline shows focus there instead.
                "outline-none focus-visible:ring-3 focus-visible:ring-ring/50 focus-visible:outline-hidden",
                "disabled:cursor-not-allowed disabled:opacity-50",
                // The chosen one has a 2px edge (padding 1px less, so nothing
                // moves), which forced colours keep. No fill: the marks'
                // contrast is against the dialog itself.
                selected
                  ? "border-2 border-foreground px-[11px] py-[9px] text-foreground forced-colors:border-[Highlight]"
                  : "border px-3 py-2.5 text-muted-foreground enabled:hover:border-muted-foreground enabled:hover:text-foreground focus-visible:text-foreground",
              )}
            >
              <AgentMark agent={agent} muted={!selected} selected={selected} />
              <span>{t(`providers.form.agents.${agent}`)}</span>
            </RadioGroupPrimitive.Item>
          )
        })}
      </RadioGroupPrimitive.Root>
    </div>
  )
}

/** The agent of a saved provider, which can't change: shown, not offered. */
export function AgentLabel({ agent }: { agent: ProviderAgent }) {
  const { t } = useTranslation()
  const labelId = useId()
  return (
    <div role="group" aria-labelledby={labelId} className="flex flex-col gap-2">
      <span id={labelId} className="text-sm leading-snug font-medium">
        {t("providers.form.agent")}
      </span>
      <span className="flex items-center gap-2 text-sm">
        <AgentMark agent={agent} />
        {t(`providers.form.agents.${agent}`)}
      </span>
    </div>
  )
}
