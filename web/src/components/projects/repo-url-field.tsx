import { InfoIcon } from "lucide-react"
import { useRef } from "react"
import { Trans, useTranslation } from "react-i18next"

import { InfoTip } from "@/components/info-tip"
import { Button } from "@/components/ui/button"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { useIsMobile } from "@/hooks/use-mobile"
import { lastInputWasKeyboard, lastInputWasTouch } from "@/lib/input-modality"
import { shortPath } from "@/lib/location-form"
import { cn } from "@/lib/utils"

import { busyClass } from "./busy"
import type { RemoteSuggestion } from "./use-folder-remote"

/** The repository URL forms the server accepts (see `checkRepoUrl`). */
const ACCEPTED_FORMS = [
  "https://host/owner/repo.git",
  "ssh://git@host:22/owner/repo.git",
  "git@host:owner/repo.git",
  "git://host/owner/repo.git",
  "file:///srv/git/repo.git",
]

/**
 * The optional repository URL of a project, with the accepted forms in a
 * tooltip. Under it, a URL read from a location's folder: a note when it
 * was filled in, or a hint with a button that uses it.
 */
export function RepoUrlField({
  id,
  value,
  error,
  busy = false,
  className,
  suggestion = null,
  onChange,
  onBlur,
  onUseSuggestion,
}: {
  id: string
  value: string
  error?: string
  /** Read-only while saving; keeps focus, unlike disabled. */
  busy?: boolean
  className?: string
  suggestion?: RemoteSuggestion | null
  onChange: (value: string) => void
  onBlur: () => void
  /** Puts a hinted URL into the field. */
  onUseSuggestion?: () => void
}) {
  const { t } = useTranslation()
  const input = useRef<HTMLInputElement>(null)
  const noteId = `${id}-remote`
  return (
    <Field className={className}>
      <div className="flex items-center gap-1.5">
        <FieldLabel htmlFor={id}>
          {t("projects.repoUrl")}
          <span className="font-normal text-muted-foreground">{t("projects.optional")}</span>
        </FieldLabel>
        <InfoTip
          side="top"
          align="start"
          contentClassName="block max-w-sm"
          content={
            <>
              <p className="mb-1">{t("projects.repoUrlForms")}</p>
              <ul className="flex flex-col gap-0.5 font-mono">
                {ACCEPTED_FORMS.map((form) => (
                  <li key={form} className="break-all">
                    {form}
                  </li>
                ))}
              </ul>
            </>
          }
        >
          <button
            type="button"
            aria-label={t("projects.repoUrlForms")}
            className="rounded-sm text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
          >
            <InfoIcon className="size-3.5" aria-hidden />
          </button>
        </InfoTip>
      </div>
      <Input
        ref={input}
        id={id}
        value={value}
        placeholder={t("projects.repoUrlPlaceholder")}
        autoCapitalize="none"
        autoComplete="off"
        spellCheck={false}
        readOnly={busy}
        aria-busy={busy || undefined}
        aria-invalid={error ? true : undefined}
        aria-describedby={suggestion ? noteId : undefined}
        onChange={(e) => onChange(e.target.value)}
        onBlur={onBlur}
      />
      {error && <FieldError>{error}</FieldError>}
      {/* Sized by the field, never by its own text: a long line truncates
          instead of widening the form. Empty, it takes no room: the margin
          cancels the field's gap. */}
      <div
        className={cn(
          "flex min-w-0 items-baseline gap-2 text-xs text-muted-foreground contain-inline-size",
          !suggestion && "-mt-2",
        )}
      >
        {/* Always mounted, so screen readers announce the text as it changes. */}
        <span id={noteId} role="status" className="flex min-w-0 items-baseline">
          {suggestion && <RemoteText suggestion={suggestion} />}
        </span>
        {suggestion?.kind === "differs" && (
          <UseButton
            busy={busy}
            onUse={() => {
              onUseSuggestion?.()
              // The button goes away with the hint: focus moves to the field,
              // with a ring only for the keyboard. Not after a touch, where
              // it would open the on-screen keyboard.
              if (lastInputWasTouch()) return
              input.current?.focus({ focusVisible: lastInputWasKeyboard() } as FocusOptions)
            }}
          />
        )}
      </div>
    </Field>
  )
}

/**
 * Where a URL was read from, or that a folder uses another URL, on one
 * line. When the line is too long, the folder gives way first, then the
 * URL; each shows in full on hover, and screen readers get it all.
 */
function RemoteText({ suggestion }: { suggestion: RemoteSuggestion }) {
  const mobile = useIsMobile()
  const { kind, device, path, url } = suggestion
  const key = kind === "filled" ? "projects.repoUrlReadFrom" : "projects.repoUrlFolderUses"
  const shortFolder = shortPath(path, mobile ? { head: 6, tail: 16 } : undefined)
  // Phones have room for little of the URL: keep its end, the repository's name.
  const shortUrl = mobile ? shortPath(url, { head: 4, tail: 12 }) : url
  return (
    <>
      <span className="sr-only">
        <Trans i18nKey={key} values={{ device, path, url }} components={{ src: <span />, url: <span /> }} />
      </span>
      {/* Text between the tags keeps its spaces. The folder's huge shrink
          factor leaves the URL whole until the folder is at its minimum. */}
      <span aria-hidden className="flex min-w-0 items-baseline whitespace-pre">
        <Trans
          i18nKey={key}
          values={{ device, path: shortFolder, url: shortUrl }}
          components={{
            src: <span className="min-w-[8ch] shrink-[1000000] truncate" title={`${device}:${path}`} />,
            url: <span className="min-w-0 truncate" title={url} />,
          }}
        />
      </span>
    </>
  )
}

function UseButton({ busy, onUse }: { busy: boolean; onUse: () => void }) {
  const { t } = useTranslation()
  return (
    <Button
      type="button"
      variant="link"
      size="xs"
      aria-disabled={busy || undefined}
      className={`h-auto shrink-0 rounded-sm px-0.5 py-0 ${busyClass}`}
      onClick={() => {
        if (!busy) onUse()
      }}
    >
      {t("projects.useRepoUrl")}
    </Button>
  )
}
