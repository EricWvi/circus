import { InfoIcon, PlusIcon, XIcon } from "lucide-react"
import { useId, useRef, useState, type FormEvent, type ReactNode } from "react"
import { Trans, useTranslation } from "react-i18next"

import { isApiError } from "@/api/errors"
import { useCreateModelProvider, useUpdateModelProvider, useWorkers } from "@/api/hooks"
import { MAX_ENV_VARS } from "@/api/provider-rules"
import type { ModelProvider, Worker } from "@/api/types"
import { InfoTip } from "@/components/info-tip"
import { busyClass } from "@/components/projects/busy"
import { DeviceStatusDot } from "@/components/status-dot"
import { Button } from "@/components/ui/button"
import { Checkbox } from "@/components/ui/checkbox"
import { DialogClose, DialogFooter, DialogHeader, DialogTitle } from "@/components/ui/dialog"
import { Field, FieldDescription, FieldError, FieldLabel, FieldLegend, FieldSet } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { useQuietAutoFocus } from "@/hooks/use-quiet-autofocus"
import {
  maskedSecret,
  newEnvRow,
  providerFormChanged,
  providerErrorMessage,
  storedVariable,
} from "@/lib/model-provider-form"
import { cn } from "@/lib/utils"

import { AgentChoice, AgentLabel, type ProviderAgent } from "./provider-agent"
import { SecretInput } from "./secret-input"
import { useProviderForm } from "./use-provider-form"

const SCHEMES = ["x_api_key", "bearer"] as const
const MODEL_FIELDS = ["opusModel", "sonnetModel", "haikuModel"] as const

/**
 * The form of the provider dialog: a new provider, or `editing`. Mounted
 * with the dialog content, so every opening starts from the saved state.
 * Secrets start empty: leaving them so keeps what is stored.
 *
 * A new provider starts by choosing its agent. Only Claude Code has
 * providers so far: Codex and opencode are offered for later, with no
 * fields and nothing to create. A saved provider's agent can't change.
 */
export function ProviderForm({
  editing,
  providers,
  onDone,
  reload,
  onAddWorker,
}: {
  editing?: ModelProvider
  /** The user's providers, for the name check. */
  providers: readonly ModelProvider[]
  onDone: () => void
  /** Fetches the provider again; resolves to undefined if it is gone. */
  reload: (providerId: string) => Promise<ModelProvider | undefined>
  /** With no workers to offer: closes the form to add one. */
  onAddWorker: () => void
}) {
  const { t } = useTranslation()
  const workers = useWorkers()
  const create = useCreateModelProvider()
  const update = useUpdateModelProvider()
  const quietAutoFocus = useQuietAutoFocus()
  // The version the form was loaded from: a save expects the server to
  // still have it, so edits on an older version never overwrite a newer one.
  const [base, setBase] = useState(editing)
  const others = providers.filter((p) => p.providerId !== base?.providerId)
  const form = useProviderForm({ editing: base, others })
  const { values, errors } = form
  const [stale, setStale] = useState(false)
  const [reloading, setReloading] = useState(false)
  const [reloadError, setReloadError] = useState<unknown>(null)
  const busy = create.isPending || update.isPending || reloading
  // Every saved provider is for Claude Code.
  const [agent, setAgent] = useState<ProviderAgent>("claude_code")
  const unavailable = agent !== "claude_code"
  const unavailableId = useId()
  const formRef = useRef<HTMLFormElement>(null)
  // An agent without providers yet shows only a message. The form keeps the
  // height it had with the fields, as far as the dialog shows it (its limit
  // is --provider-dialog-max, less its padding), so the dialog doesn't
  // shrink and re-centre under the pointer and the footer stays at its
  // bottom; CSS keeps that right as the window resizes.
  const [heldHeight, setHeldHeight] = useState<string>()
  const chooseAgent = (next: ProviderAgent) => {
    const element = formRef.current
    // The dialog's content is the form's scrolling parent.
    const scroller = element?.parentElement
    if (!unavailable && element && scroller) {
      const style = getComputedStyle(scroller)
      const padding = parseFloat(style.paddingTop) + parseFloat(style.paddingBottom)
      setHeldHeight(`min(${element.offsetHeight}px, calc(var(--provider-dialog-max, 100svh) - ${padding}px))`)
    }
    setAgent(next)
  }

  // Assigned workers the list no longer has (deleted since): the server
  // refuses their ids, so they are dropped from what is sent, and the form
  // says so. Unknown while the list hasn't loaded.
  const goneWorkers = workers.data
    ? values.workerIds.filter((id) => !workers.data.some((w) => w.workerId === id))
    : []

  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (busy || stale || unavailable) return
    const submitted = form.submit()
    if (!submitted) return
    const req = { ...submitted, workerIds: submitted.workerIds.filter((id) => !goneWorkers.includes(id)) }
    const onError = (error: unknown) => {
      if (isApiError(error, "aborted")) setStale(true)
      else form.serverError(error)
    }
    if (!base) {
      create.mutate(req, { onSuccess: onDone, onError })
      return
    }
    if (!providerFormChanged(base, { ...values, workerIds: req.workerIds })) {
      onDone()
      return
    }
    update.mutate(
      { ...req, providerId: base.providerId, expectedUpdatedAt: base.updatedAtUnixMs },
      { onSuccess: onDone, onError },
    )
  }

  /** Starts the form over from the provider as the server has it now. */
  const reloadNow = async () => {
    if (!base || reloading) return
    setReloading(true)
    setReloadError(null)
    try {
      const fresh = await reload(base.providerId)
      if (!fresh) {
        setReloadError(new Error("gone"))
        return
      }
      setBase(fresh)
      form.reset(fresh)
      setStale(false)
    } catch (error) {
      setReloadError(error)
    } finally {
      setReloading(false)
    }
  }

  const unreadable = base?.secretsUnreadable === true
  const fieldProps = (field: Parameters<typeof form.touch>[0], invalid: string | undefined) => ({
    readOnly: busy,
    "aria-busy": busy || undefined,
    "aria-invalid": invalid ? true : undefined,
    onBlur: () => form.touch(field),
  })

  const nameProps = fieldProps("name", errors.fields.name)
  const baseUrlProps = fieldProps("baseUrl", errors.fields.baseUrl)
  const keyProps = fieldProps("key", errors.fields.key)
  const maxSessionsProps = fieldProps("maxSessions", errors.fields.maxSessions)

  let keyPlaceholder: string | undefined
  let keyHint: string | undefined
  if (unreadable) keyHint = t("providers.form.keyReenterHint")
  else if (base) {
    keyPlaceholder = t("providers.form.keyStored", { masked: maskedSecret(base.keyLast4) })
    keyHint = t("providers.form.keyKeepHint")
  }

  return (
    <form
      ref={formRef}
      onSubmit={submit}
      noValidate
      className="flex flex-col gap-6"
      style={unavailable ? { minHeight: heldHeight } : undefined}
    >
      <DialogHeader>
        <DialogTitle>{base ? t("providers.form.editTitle", { name: base.name }) : t("providers.form.newTitle")}</DialogTitle>
      </DialogHeader>

      {base ? (
        <AgentLabel agent="claude_code" />
      ) : (
        <AgentChoice
          value={agent}
          onChange={chooseAgent}
          disabled={busy}
          describedBy={unavailable ? unavailableId : undefined}
        />
      )}

      {/* Hidden rather than unmounted: switching back finds them as left. */}
      <div hidden={unavailable} className="flex flex-col gap-6">
        <Field>
          <FieldLabel htmlFor="provider-name">{t("common.name")}</FieldLabel>
          <Input
            id="provider-name"
            autoComplete="off"
            value={values.name}
            onChange={(e) => form.set("name", e.target.value)}
            {...nameProps}
            // A new provider starts at its name (the dialog focuses it), without a focus ring.
            {...(base ? {} : { "data-quiet-focus": quietAutoFocus["data-quiet-focus"] })}
            onBlur={() => {
              quietAutoFocus.onBlur()
              // Leaving a new provider's name empty isn't touching it: the
              // user may choose the agent above first. Saving still asks for it.
              if (base || values.name !== "") form.touch("name")
            }}
          />
          {errors.fields.name && <FieldError>{errors.fields.name}</FieldError>}
        </Field>

        <FieldSet className="gap-4">
          <FieldLegend variant="label" className="mb-2">
            {t("providers.form.endpoint")}
          </FieldLegend>
          <Field>
            <FieldLabel htmlFor="provider-base-url">{t("providers.form.baseUrl")}</FieldLabel>
            <Input
              id="provider-base-url"
              autoComplete="off"
              spellCheck={false}
              inputMode="url"
              placeholder={t("providers.form.baseUrlPlaceholder")}
              value={values.baseUrl}
              onChange={(e) => form.set("baseUrl", e.target.value)}
              {...baseUrlProps}
            />
            {errors.fields.baseUrl && <FieldError>{errors.fields.baseUrl}</FieldError>}
          </Field>
          <div className="grid gap-4 sm:grid-cols-2">
            <SchemeField
              value={values.scheme}
              busy={busy}
              error={errors.fields.scheme}
              onChange={(scheme) => form.set("scheme", scheme)}
            />
            <Field>
              <FieldLabel htmlFor="provider-key">{t("providers.form.key")}</FieldLabel>
              <SecretInput
                id="provider-key"
                placeholder={keyPlaceholder}
                value={values.key}
                aria-describedby={keyHint ? "provider-key-hint" : undefined}
                onChange={(e) => form.set("key", e.target.value)}
                {...keyProps}
              />
              {errors.fields.key ? (
                <FieldError>{errors.fields.key}</FieldError>
              ) : (
                keyHint && (
                  <FieldDescription id="provider-key-hint" className={cn("text-xs", unreadable && "text-destructive")}>
                    {keyHint}
                  </FieldDescription>
                )
              )}
            </Field>
          </div>
        </FieldSet>

        <FieldSet className="gap-3">
          <FieldLegend variant="label" className="mb-2 flex items-center gap-1">
            {t("providers.form.models")}
            <HintTip id="provider-models-hint" text={t("providers.form.modelsHint")} />
          </FieldLegend>
          <div className="grid gap-3 sm:grid-cols-3">
            {MODEL_FIELDS.map((field) => (
              <Field key={field} className="gap-1.5">
                <FieldLabel htmlFor={`provider-${field}`} className="text-xs text-muted-foreground">
                  {t(`providers.form.${field}`)}
                </FieldLabel>
                <Input
                  id={`provider-${field}`}
                  autoComplete="off"
                  spellCheck={false}
                  placeholder={t("providers.form.modelPlaceholder")}
                  value={values[field]}
                  onChange={(e) => form.set(field, e.target.value)}
                  {...fieldProps(field, errors.fields[field])}
                />
                {errors.fields[field] && <FieldError>{errors.fields[field]}</FieldError>}
              </Field>
            ))}
          </div>
        </FieldSet>

        <VariablesField form={form} editing={base} busy={busy} />

        <WorkersField
          workers={workers.data}
          failed={workers.isError && !workers.data}
          value={values.workerIds}
          busy={busy}
          error={errors.fields.workerIds}
          gone={goneWorkers.length}
          onChange={(ids) => form.set("workerIds", ids)}
          onAddWorker={onAddWorker}
        />

        <Field>
          <div className="flex items-center gap-1">
            <FieldLabel htmlFor="provider-max-sessions">{t("providers.form.maxSessions")}</FieldLabel>
            <HintTip id="provider-max-sessions-hint" text={t("providers.form.maxSessionsHint")} />
          </div>
          <Input
            id="provider-max-sessions"
            inputMode="numeric"
            autoComplete="off"
            className="max-w-40"
            placeholder={t("providers.form.maxSessionsPlaceholder")}
            value={values.maxSessions}
            aria-describedby="provider-max-sessions-hint"
            onChange={(e) => form.set("maxSessions", e.target.value)}
            {...maxSessionsProps}
          />
          {errors.fields.maxSessions && <FieldError>{errors.fields.maxSessions}</FieldError>}
        </Field>

        <label className="flex cursor-default items-start gap-2.5">
          <Checkbox
            className="mt-0.5"
            checked={values.isDefault}
            disabled={busy}
            onCheckedChange={(checked) => form.set("isDefault", checked === true)}
            aria-describedby="provider-default-hint"
          />
          <span className="flex flex-col gap-0.5">
            <span className="text-sm">{t("providers.form.useAsDefault")}</span>
            <span id="provider-default-hint" className="text-xs text-muted-foreground">
              {t("providers.form.useAsDefaultHint")}
            </span>
          </span>
        </label>

        {stale && (
          <div role="alert" className="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm">
            <span className="text-destructive">{t("providers.form.changedElsewhere")}</span>
            <Button
              type="button"
              variant="outline"
              size="sm"
              aria-disabled={reloading || undefined}
              className={busyClass}
              onClick={() => void reloadNow()}
            >
              {reloading && <Spinner aria-label={t("common.loading")} />}
              {t("providers.form.discardAndReload")}
            </Button>
            {reloadError !== null && (
              <span className="text-destructive">
                {reloadError instanceof Error && reloadError.message === "gone"
                  ? t("providers.form.gone")
                  : providerErrorMessage(reloadError)}
              </span>
            )}
          </div>
        )}
        {errors.form && (
          <p role="alert" className="text-sm text-destructive">
            {errors.form}
          </p>
        )}
      </div>
      {unavailable && (
        <p id={unavailableId} className="flex-1 text-sm text-muted-foreground">
          {t("providers.form.agentUnavailable", { agent: t(`providers.form.agents.${agent}`) })}
        </p>
      )}
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t("common.cancel")}
          </Button>
        </DialogClose>
        <Button
          type="submit"
          disabled={stale || unavailable}
          aria-disabled={busy || undefined}
          aria-describedby={unavailable ? unavailableId : undefined}
          className={busyClass}
        >
          {(create.isPending || update.isPending) && (
            <Spinner aria-label={base ? t("common.saving") : t("providers.form.creating")} />
          )}
          {base ? t("common.save") : t("common.create")}
        </Button>
      </DialogFooter>
    </form>
  )
}

function SchemeField({
  value,
  busy,
  error,
  onChange,
}: {
  value: (typeof SCHEMES)[number] | "unspecified"
  busy: boolean
  error: string | undefined
  onChange: (scheme: (typeof SCHEMES)[number]) => void
}) {
  const { t } = useTranslation()
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  return (
    <Field>
      <FieldLabel htmlFor="provider-scheme">{t("providers.form.scheme")}</FieldLabel>
      <Select
        // A scheme this client doesn't know shows no choice; the user picks one.
        value={value === "unspecified" ? "" : value}
        disabled={busy}
        onValueChange={(v) => {
          const scheme = SCHEMES.find((s) => s === v)
          if (scheme) onChange(scheme)
        }}
        onOpenChange={onOpenChange}
      >
        <SelectTrigger id="provider-scheme" ref={triggerRef} className="w-full" aria-invalid={error ? true : undefined}>
          <SelectValue placeholder={t("providers.form.schemePlaceholder")} />
        </SelectTrigger>
        <SelectContent position="popper" onCloseAutoFocus={onCloseAutoFocus}>
          {SCHEMES.map((s) => (
            <SelectItem key={s} value={s}>
              {t(`providers.form.schemeOptions.${s}`)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {error && <FieldError>{error}</FieldError>}
    </Field>
  )
}

function VariablesField({
  form,
  editing,
  busy,
}: {
  form: ReturnType<typeof useProviderForm>
  editing: ModelProvider | undefined
  busy: boolean
}) {
  const { t } = useTranslation()
  const rows = form.values.env
  const full = rows.length >= MAX_ENV_VARS
  const unreadable = editing?.secretsUnreadable === true
  const edit = (key: string, part: "name" | "value", text: string) =>
    form.setEnv(rows.map((r) => (r.key === key ? { ...r, [part]: text } : r)))

  const placeholder = (name: string) => {
    if (unreadable && editing?.env.some((v) => v.name === name.trim())) return t("providers.form.valueReenter")
    const stored = storedVariable({ name }, editing)
    if (!stored) return undefined
    return stored.valueLast4 === ""
      ? t("providers.form.valueStoredEmpty")
      : t("providers.form.valueStored", { masked: maskedSecret(stored.valueLast4) })
  }

  return (
    <FieldSet className="gap-3">
      <FieldLegend variant="label" className="mb-2">
        {t("providers.form.variables")}
      </FieldLegend>
      {rows.length > 0 && (
        <ul className="flex flex-col gap-2">
          {rows.map((row, i) => {
            const rowErrors = form.errors.env[row.key]
            const n = i + 1
            return (
              <li key={row.key} className="flex flex-col gap-1">
                <div className="flex items-start gap-2">
                  <Input
                    aria-label={t("providers.form.variableNameFor", { n })}
                    autoComplete="off"
                    spellCheck={false}
                    autoCapitalize="characters"
                    className="w-2/5"
                    placeholder={t("providers.form.variableName")}
                    value={row.name}
                    readOnly={busy}
                    aria-invalid={rowErrors?.name ? true : undefined}
                    onChange={(e) => edit(row.key, "name", e.target.value)}
                    onBlur={() => form.touch(`${row.key}.name`)}
                  />
                  <SecretInput
                    id={`provider-env-${row.key}-value`}
                    aria-label={t("providers.form.variableValueFor", { n })}
                    className="min-w-0 flex-1"
                    placeholder={
                      row.empty
                        ? t("providers.form.emptyValuePlaceholder")
                        : (placeholder(row.name) ?? t("providers.form.variableValue"))
                    }
                    value={row.value}
                    readOnly={busy || row.empty}
                    aria-invalid={rowErrors?.value ? true : undefined}
                    onChange={(e) => edit(row.key, "value", e.target.value)}
                    onBlur={() => form.touch(`${row.key}.value`)}
                  />
                  <Button
                    type="button"
                    variant={row.empty ? "secondary" : "ghost"}
                    size="sm"
                    className={cn("h-8 shrink-0", busyClass)}
                    aria-label={t("providers.form.emptyValueFor", { n })}
                    title={t("providers.form.emptyValueHint")}
                    aria-pressed={row.empty === true}
                    aria-disabled={busy || undefined}
                    onClick={() =>
                      !busy &&
                      form.setEnv(rows.map((r) => (r.key === row.key ? { ...r, value: "", empty: !r.empty } : r)))
                    }
                  >
                    {t("providers.form.emptyValue")}
                  </Button>
                  <Button
                    type="button"
                    variant="ghost"
                    size="icon"
                    aria-label={t("providers.form.removeVariable", { n })}
                    aria-disabled={busy || undefined}
                    className={busyClass}
                    onClick={() => !busy && form.setEnv(rows.filter((r) => r.key !== row.key))}
                  >
                    <XIcon />
                  </Button>
                </div>
                {rowErrors?.name && <FieldError>{rowErrors.name}</FieldError>}
                {rowErrors?.value && <FieldError>{rowErrors.value}</FieldError>}
              </li>
            )
          })}
        </ul>
      )}
      {form.errors.fields.env && <FieldError>{form.errors.fields.env}</FieldError>}
      {!unreadable && editing && editing.env.length > 0 && (
        <p className="text-xs text-muted-foreground">{t("providers.form.valueKeepHint")}</p>
      )}
      <div>
        <Button
          type="button"
          variant="outline"
          size="sm"
          disabled={full}
          title={full ? t("providers.form.maxVariables", { max: MAX_ENV_VARS }) : undefined}
          aria-disabled={busy || undefined}
          className={busyClass}
          onClick={() => !busy && form.setEnv([...rows, newEnvRow()])}
        >
          <PlusIcon />
          {t("providers.form.addVariable")}
        </Button>
      </div>
    </FieldSet>
  )
}

function WorkersField({
  workers,
  failed,
  value,
  busy,
  error,
  gone,
  onChange,
  onAddWorker,
}: {
  workers: Worker[] | undefined
  failed: boolean
  value: readonly string[]
  busy: boolean
  error: string | undefined
  /** Assigned workers the list no longer has, which saving takes off. */
  gone: number
  onChange: (ids: string[]) => void
  onAddWorker: () => void
}) {
  const { t } = useTranslation()
  let body: ReactNode
  if (failed) body = <p className="text-sm text-muted-foreground">{t("providers.form.workersLoadFailed")}</p>
  else if (!workers) body = <Spinner aria-label={t("common.loading")} />
  else if (workers.length === 0) {
    body = (
      <p className="text-sm text-muted-foreground">
        <Trans
          i18nKey="providers.form.noWorkers"
          components={{
            add: (
              <button
                type="button"
                className="rounded-sm text-foreground underline underline-offset-4 outline-none hover:text-primary focus-visible:ring-2 focus-visible:ring-ring/50"
                onClick={onAddWorker}
              />
            ),
          }}
        />
      </p>
    )
  } else {
    body = (
      <ul className="flex flex-col divide-y border">
        {workers.map((w) => (
          <li key={w.workerId}>
            <label className="flex cursor-default items-start gap-2.5 px-3 py-2">
              <Checkbox
                className="mt-0.5"
                checked={value.includes(w.workerId)}
                disabled={busy}
                onCheckedChange={(checked) =>
                  onChange(checked === true ? [...value, w.workerId] : value.filter((id) => id !== w.workerId))
                }
              />
              <span className="flex min-w-0 flex-1 flex-col gap-0.5">
                <span className="flex min-w-0 items-center gap-2 text-sm">
                  <DeviceStatusDot online={w.online} />
                  <span className="truncate">{w.name}</span>
                  <span aria-hidden className="shrink-0 text-xs text-muted-foreground">
                    {w.online ? t("devices.online") : t("devices.offline")}
                  </span>
                </span>
                {w.localDevices.length > 0 && (
                  <span className="text-xs text-muted-foreground">
                    {t("providers.form.workerSameMachine", { devices: w.localDevices.join(t("common.listSeparator")) })}
                  </span>
                )}
              </span>
            </label>
          </li>
        ))}
      </ul>
    )
  }
  return (
    <FieldSet className="gap-3">
      <FieldLegend variant="label" className="mb-2">
        {t("providers.form.workers")}
      </FieldLegend>
      {body}
      {gone > 0 && (
        <p role="status" className="text-xs text-muted-foreground">
          {t("providers.form.workersGone", { count: gone })}
        </p>
      )}
      {error && <FieldError>{error}</FieldError>}
    </FieldSet>
  )
}

/** An info icon beside a field's label whose tooltip holds the field's hint. */
function HintTip({ id, text }: { id: string; text: string }) {
  return (
    <InfoTip content={text} contentClassName="max-w-xs" side="top">
      <span
        tabIndex={0}
        className="flex cursor-default items-center rounded-sm p-0.5 font-normal text-muted-foreground outline-none hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring/50"
      >
        <InfoIcon className="size-3" aria-hidden />
        <span id={id} className="sr-only">
          {text}
        </span>
      </span>
    </InfoTip>
  )
}
