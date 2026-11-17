import { InfoIcon } from "lucide-react"
import { useTranslation } from "react-i18next"

import { InfoTip } from "@/components/info-tip"
import { Field, FieldError, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"

/** The repository URL forms the server accepts (see `checkRepoUrl`). */
const ACCEPTED_FORMS = [
  "https://host/owner/repo.git",
  "ssh://git@host:22/owner/repo.git",
  "git@host:owner/repo.git",
  "git://host/owner/repo.git",
  "file:///srv/git/repo.git",
]

/** The optional repository URL of a project, with the accepted forms in a tooltip. */
export function RepoUrlField({
  id,
  value,
  error,
  busy = false,
  className,
  onChange,
  onBlur,
}: {
  id: string
  value: string
  error?: string
  /** Read-only while saving; keeps focus, unlike disabled. */
  busy?: boolean
  className?: string
  onChange: (value: string) => void
  onBlur: () => void
}) {
  const { t } = useTranslation()
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
        id={id}
        value={value}
        placeholder={t("projects.repoUrlPlaceholder")}
        autoCapitalize="none"
        autoComplete="off"
        spellCheck={false}
        readOnly={busy}
        aria-busy={busy || undefined}
        aria-invalid={error ? true : undefined}
        onChange={(e) => onChange(e.target.value)}
        onBlur={onBlur}
      />
      {error && <FieldError>{error}</FieldError>}
    </Field>
  )
}
