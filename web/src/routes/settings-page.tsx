import { MinusIcon, PlusIcon } from "lucide-react"
import { memo, useMemo, useRef, useState, type ReactNode } from "react"
import { useTranslation } from "react-i18next"

import { CodeBlock } from "@/components/ai-elements/code-block"
import { PageHeader } from "@/components/page-header"
import { isTheme, themeChoices } from "@/components/theme/theme-choices"
import { useTheme } from "@/components/theme/theme-context"
import { Button } from "@/components/ui/button"
import { ButtonGroup } from "@/components/ui/button-group"
import { Command, CommandGroup, CommandInput, CommandItem, CommandList } from "@/components/ui/command"
import { Field, FieldDescription, FieldLabel, FieldTitle } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select"
import { Spinner } from "@/components/ui/spinner"
import { useOverlayFocus } from "@/hooks/use-overlay-focus"
import { isPreference, languageNames, languagePreferences } from "@/i18n/language"
import { setLanguagePreference, useLanguagePreference } from "@/i18n/preference"
import { lastInputWasKeyboard } from "@/lib/input-modality"
import {
  DEFAULT_CODE_FONT_STACK,
  MAX_CODE_FONT_SIZE,
  MIN_CODE_FONT_SIZE,
  normalizeFamily,
  resetCodeFont,
  setCodeFontFamily,
  setCodeFontSize,
  useCodeFont,
} from "@/settings/code-font"
import {
  canListLocalFonts,
  canvasMeasure,
  cssFamily,
  isFontAvailable,
  isGenericFamily,
  listLocalFonts,
  type LocalFamily,
  type LocalFontsResult,
} from "@/settings/local-fonts"

/** Code for the preview; the Chinese comment shows how CJK falls through. */
export const PREVIEW_CODE = `// fetchToken retries three times. 令牌过期时重新获取。
func fetchToken(ctx context.Context) (Token, error) {
	for attempt := 1; attempt <= 3; attempt++ {
		if tok, err := refresh(ctx); err == nil {
			return tok, nil // 成功
		}
	}
	return Token{}, ErrExpired // 0O 1lI {} [] => !=
}`

/** Browser-level settings: how the app looks on this device. */
export function SettingsPage() {
  const { t } = useTranslation()
  return (
    <div className="flex h-full min-h-0 flex-col">
      <PageHeader>
        <h1 className="min-w-0 truncate font-medium">{t("settings.title")}</h1>
      </PageHeader>
      <div className="min-h-0 flex-1 overflow-y-auto">
        <div className="mx-auto flex w-full max-w-[760px] flex-col gap-10 px-4 py-8">
          <section aria-labelledby="settings-appearance" className="flex flex-col gap-6">
            <h2 id="settings-appearance" className="text-sm font-medium">
              {t("settings.appearance")}
            </h2>
            <ThemeField />
            <LanguageField />
            <CodeFontField />
            <CodeFontSizeField />
            <CodePreview />
          </section>
        </div>
      </div>
    </div>
  )
}

/** A labelled select whose focus handling leaves no ring after pointer use. */
function SettingSelect({
  id,
  label,
  value,
  onChange,
  children,
}: {
  id: string
  label: string
  value: string
  onChange: (value: string) => void
  children: ReactNode
}) {
  const { triggerRef, onOpenChange, onCloseAutoFocus } = useOverlayFocus<HTMLButtonElement>()
  return (
    <Field className="max-w-sm">
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      <Select value={value} onValueChange={onChange} onOpenChange={onOpenChange}>
        <SelectTrigger id={id} ref={triggerRef} className="w-full">
          <SelectValue />
        </SelectTrigger>
        <SelectContent position="popper" onCloseAutoFocus={onCloseAutoFocus}>
          {children}
        </SelectContent>
      </Select>
    </Field>
  )
}

function ThemeField() {
  const { t } = useTranslation()
  const { theme, setTheme } = useTheme()
  return (
    <SettingSelect
      id="settings-theme"
      label={t("settings.theme")}
      value={theme}
      onChange={(value) => isTheme(value) && setTheme(value)}
    >
      {themeChoices.map(({ value, icon: Icon }) => (
        <SelectItem key={value} value={value}>
          <Icon />
          {t(`userMenu.themes.${value}`)}
        </SelectItem>
      ))}
    </SettingSelect>
  )
}

function LanguageField() {
  const { t } = useTranslation()
  const language = useLanguagePreference()
  return (
    <SettingSelect
      id="settings-language"
      label={t("settings.language")}
      value={language}
      onChange={(value) => isPreference(value) && setLanguagePreference(value)}
    >
      {languagePreferences.map((choice) => (
        // Language names are shown in their own language.
        <SelectItem key={choice} value={choice} lang={choice === "system" ? undefined : choice}>
          {choice === "system" ? t("userMenu.systemLanguage") : languageNames[choice]}
        </SelectItem>
      ))}
    </SettingSelect>
  )
}

type FontsState = { state: "idle" } | { state: "loading" } | LocalFontsResult

const DEFAULT_ITEM = "\u0000default"
const TYPED_ITEM = "\u0000typed"
const NO_FAMILIES: LocalFamily[] = []

/** A family name set in its own font, falling back to the UI font. */
const previewStyle = (family: string) => ({ fontFamily: `${cssFamily(family)}, var(--font-sans)` })

/**
 * One font in the list, set in its own font. Memoized: the page re-renders
 * whenever the highlight moves, and the list can hold hundreds of fonts.
 */
const FontItem = memo(function FontItem({
  family,
  checked,
  hint,
}: {
  family: string
  checked: boolean
  hint?: string
}) {
  return (
    <CommandItem value={family} data-checked={checked} onSelect={() => setCodeFontFamily(family)}>
      <span className="min-w-0 truncate" style={previewStyle(family)}>
        {family}
      </span>
      {hint && <Hint>{hint}</Hint>}
    </CommandItem>
  )
})

const FontGroup = memo(function FontGroup({
  heading,
  families,
  chosen,
}: {
  heading: string
  families: LocalFamily[]
  chosen: string | null
}) {
  if (families.length === 0) return null
  return (
    <CommandGroup heading={heading}>
      {families.map((f) => (
        <FontItem key={f.family} family={f.family} checked={f.family === chosen} />
      ))}
    </CommandGroup>
  )
})

function CodeFontField() {
  const { t } = useTranslation()
  const { family: chosen } = useCodeFont()
  const [query, setQuery] = useState("")
  const [fonts, setFonts] = useState<FontsState>({ state: "idle" })
  const measure = useMemo(() => canvasMeasure(), [])
  const supported = useMemo(() => canListLocalFonts(), [])
  const input = useRef<HTMLInputElement>(null)

  const families = fonts.state === "ok" ? fonts.families : NO_FAMILIES
  const needle = query.trim().toLowerCase()
  const monospace = useMemo(
    () => families.filter((f) => f.monospace && f.family.toLowerCase().includes(needle)),
    [families, needle],
  )
  const others = useMemo(
    () => families.filter((f) => !f.monospace && f.family.toLowerCase().includes(needle)),
    [families, needle],
  )

  // The typed name, unless a listed family starts with it (pick that one).
  const typed = normalizeFamily(query)
  const showTyped =
    typed !== null && !families.some((f) => f.family.toLowerCase().startsWith(typed.toLowerCase()))
  const typedHint = useMemo(() => {
    if (typed === null) return null
    if (isGenericFamily(typed)) return "generic"
    if (!measure) return null
    return isFontAvailable(measure, typed) ? "installed" : "missing"
  }, [typed, measure])
  const chosenListed = chosen !== null && families.some((f) => f.family === chosen)

  // The row that shows the chosen font, as the list is rendered now.
  const chosenRow =
    chosen === null
      ? DEFAULT_ITEM
      : chosenListed || !needle
        ? chosen
        : showTyped && typed === chosen
          ? TYPED_ITEM
          : null

  // The highlight (cmdk's active row) starts on the chosen font and moves
  // to it whenever the choice changes (a typed name picked, a reset). It
  // shows only while the list has focus (see the Command's className); the
  // check mark marks the choice itself.
  const [highlighted, setHighlighted] = useState(chosenRow ?? DEFAULT_ITEM)
  const [shownChoice, setShownChoice] = useState(chosen)
  if (shownChoice !== chosen) {
    setShownChoice(chosen)
    if (chosenRow !== null) setHighlighted(chosenRow)
  }

  const load = async () => {
    if (fonts.state === "loading") return
    // Read before awaiting: the keyboard or a pointer pressed the button.
    const keyboard = lastInputWasKeyboard()
    setFonts({ state: "loading" })
    const result = await listLocalFonts()
    setFonts(result)
    // A keyboard user carries on in the filter box (a name can always be
    // typed). After a click, focus stays put: a focused text input always
    // shows its ring, which would be left behind by pointer use.
    if (keyboard) input.current?.focus()
  }

  const defaultItem = (
    <CommandItem
      value={DEFAULT_ITEM}
      data-checked={chosen === null}
      onSelect={() => setCodeFontFamily(null)}
    >
      <span className="min-w-0 truncate" style={{ fontFamily: DEFAULT_CODE_FONT_STACK }}>
        {t("settings.defaultFont")}
      </span>
    </CommandItem>
  )
  const typedItem = showTyped && (
    <CommandItem
      value={TYPED_ITEM}
      disabled={typedHint === "generic"}
      data-checked={chosen === typed}
      onSelect={() => setCodeFontFamily(typed)}
    >
      <span className="min-w-0 truncate" style={previewStyle(typed)}>
        {t("settings.useFont", { name: typed })}
      </span>
      {typedHint === "generic" && <Hint>{t("settings.notFontName")}</Hint>}
      {typedHint === "installed" && <Hint>{t("settings.fontInstalled")}</Hint>}
      {typedHint === "missing" && <Hint>{t("settings.fontMissing")}</Hint>}
    </CommandItem>
  )

  return (
    <Field>
      {/* cmdk sets the input's id and labels it with the Command's `label`. */}
      <FieldTitle>{t("settings.codeFont")}</FieldTitle>
      <FieldDescription>{t("settings.codeFontDescription")}</FieldDescription>
      <Command
        shouldFilter={false}
        loop
        value={highlighted}
        onValueChange={setHighlighted}
        label={t("settings.codeFont")}
        // Unfocused, the list shows no highlight, only a lighter hover tint;
        // focused, cmdk's highlight (which follows the pointer) is the only
        // grey row.
        className="h-auto rounded-lg! border bg-transparent [&:not(:focus-within)_[data-slot=command-item]:hover]:bg-muted/50 [&:not(:focus-within)_[data-slot=command-item][data-selected=true]:not(:hover)]:bg-transparent"
        onKeyDown={(event) => {
          // A generic family name can't be chosen; Enter must not fall
          // through to Default, which cmdk highlights in its place.
          if (event.key === "Enter" && showTyped && typedHint === "generic" && highlighted === DEFAULT_ITEM) {
            event.preventDefault()
          }
        }}
      >
        <CommandInput
          ref={input}
          value={query}
          onValueChange={setQuery}
          placeholder={t("settings.filterFonts")}
          autoComplete="off"
          spellCheck={false}
        />
        <CommandList className="max-h-72">
          {/* While filtering, matches come first: cmdk highlights the first
              item after each keystroke, so Enter takes the best match. */}
          {!needle && (
            <CommandGroup>
              {defaultItem}
              {chosen !== null && !chosenListed && (
                <FontItem family={chosen} checked hint={t("settings.current")} />
              )}
            </CommandGroup>
          )}
          <FontGroup heading={t("settings.monospaceFonts")} families={monospace} chosen={chosen} />
          <FontGroup heading={t("settings.otherFonts")} families={others} chosen={chosen} />
          {needle && (
            <CommandGroup>
              {typedItem}
              {defaultItem}
            </CommandGroup>
          )}
          {fonts.state === "ok" && monospace.length + others.length === 0 && (
            <p className="px-3 py-2 text-sm text-muted-foreground">{t("settings.noMatchingFonts")}</p>
          )}
        </CommandList>
      </Command>
      <FontSourceNote supported={supported} fonts={fonts} onLoad={() => void load()} />
    </Field>
  )
}

const Hint = ({ children }: { children: ReactNode }) => (
  <span className="ml-auto shrink-0 pl-2 font-sans text-xs text-muted-foreground">{children}</span>
)

/** Where the list's fonts come from: the button that asks, or why it can't. */
function FontSourceNote({
  supported,
  fonts,
  onLoad,
}: {
  supported: boolean
  fonts: FontsState
  onLoad: () => void
}) {
  const { t } = useTranslation()
  if (!supported || fonts.state === "unsupported") {
    return <FieldDescription>{t("settings.fontsUnsupported")}</FieldDescription>
  }
  if (fonts.state === "ok") return null
  const loading = fonts.state === "loading"
  const denied = fonts.state === "denied"
  return (
    <div className="flex flex-col items-start gap-2">
      {denied && <FieldDescription>{t("settings.fontsDenied")}</FieldDescription>}
      {/* aria-disabled while loading, so a keyboard user keeps focus here. */}
      <Button
        type="button"
        variant="outline"
        aria-disabled={loading || undefined}
        className="aria-disabled:opacity-50"
        onClick={onLoad}
      >
        {loading && <Spinner aria-label={t("settings.loadingFonts")} />}
        {denied ? t("settings.tryAgain") : t("settings.showInstalledFonts")}
      </Button>
    </div>
  )
}

const isSize = (text: string) => {
  const n = Number(text)
  return text.trim() !== "" && Number.isInteger(n) && n >= MIN_CODE_FONT_SIZE && n <= MAX_CODE_FONT_SIZE
}

function CodeFontSizeField() {
  const { t } = useTranslation()
  const { size } = useCodeFont()
  // What the box shows while someone types; null shows the setting.
  const [draft, setDraft] = useState<string | null>(null)
  const atMin = size <= MIN_CODE_FONT_SIZE
  const atMax = size >= MAX_CODE_FONT_SIZE

  const step = (delta: number, blocked: boolean) => {
    if (blocked) return
    setDraft(null)
    setCodeFontSize(size + delta)
  }

  const onType = (text: string) => {
    setDraft(text)
    if (isSize(text)) setCodeFontSize(Number(text))
  }

  // aria-disabled, not disabled, at the limits: the button keeps focus.
  const stepButton = "aria-disabled:cursor-not-allowed aria-disabled:opacity-50"
  return (
    <Field className="max-w-sm">
      <FieldLabel htmlFor="settings-code-font-size">{t("settings.codeFontSize")}</FieldLabel>
      <div className="flex items-center gap-2">
        <ButtonGroup>
          <Button
            type="button"
            variant="outline"
            size="icon"
            aria-label={t("settings.smaller")}
            aria-controls="settings-code-font-size"
            aria-disabled={atMin || undefined}
            className={stepButton}
            onClick={() => step(-1, atMin)}
          >
            <MinusIcon />
          </Button>
          <Input
            id="settings-code-font-size"
            type="number"
            inputMode="numeric"
            min={MIN_CODE_FONT_SIZE}
            max={MAX_CODE_FONT_SIZE}
            step={1}
            value={draft ?? String(size)}
            aria-invalid={(draft !== null && !isSize(draft)) || undefined}
            aria-describedby="settings-code-font-size-hint"
            onChange={(e) => onType(e.target.value)}
            onBlur={() => setDraft(null)}
            className="w-16 text-center [appearance:textfield] [&::-webkit-inner-spin-button]:appearance-none [&::-webkit-outer-spin-button]:appearance-none"
          />
          <Button
            type="button"
            variant="outline"
            size="icon"
            aria-label={t("settings.larger")}
            aria-controls="settings-code-font-size"
            aria-disabled={atMax || undefined}
            className={stepButton}
            onClick={() => step(1, atMax)}
          >
            <PlusIcon />
          </Button>
        </ButtonGroup>
        <span id="settings-code-font-size-hint" className="text-sm text-muted-foreground">
          {t("settings.sizeRange", { min: MIN_CODE_FONT_SIZE, max: MAX_CODE_FONT_SIZE })}
        </span>
      </div>
    </Field>
  )
}

function CodePreview() {
  const { t } = useTranslation()
  return (
    <div className="flex flex-col gap-2">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 className="text-sm font-medium">{t("settings.preview")}</h3>
        <Button type="button" variant="outline" size="sm" onClick={resetCodeFont}>
          {t("settings.reset")}
        </Button>
      </div>
      <CodeBlock code={PREVIEW_CODE} language="go" />
    </div>
  )
}
