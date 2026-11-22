import { EyeIcon, EyeOffIcon } from "lucide-react"
import { useState, type ComponentProps } from "react"
import { useTranslation } from "react-i18next"

import { InputGroup, InputGroupAddon, InputGroupButton, InputGroupInput } from "@/components/ui/input-group"
import { cn } from "@/lib/utils"

/**
 * A text field for a secret (an API key, a variable value), masked until
 * the user shows it.
 *
 * Not `type="password"`: browsers offer to save any password field in a
 * form that goes away after a successful request, which would put the key
 * into the password manager as a site login. A text field masked with
 * `-webkit-text-security` is never taken for a login. `autocomplete="off"`
 * and the opt-out attributes of the common password-manager extensions
 * (1Password, LastPass, Bitwarden, Dashlane) keep those away too. The
 * mask is only visual: copying and screen readers get the text, as the
 * user typed it themselves. Autocorrect, autocapitalisation and spell
 * checking are off so keyboards don't learn it.
 */
export function SecretInput({ className, ...props }: Omit<ComponentProps<"input">, "type">) {
  const { t } = useTranslation()
  const [shown, setShown] = useState(false)
  // The name stays; the pressed state says whether it is shown.
  const label = t("providers.form.showSecret")
  return (
    <InputGroup className={className}>
      <InputGroupInput
        {...props}
        type="text"
        autoComplete="off"
        autoCorrect="off"
        autoCapitalize="off"
        spellCheck={false}
        data-1p-ignore=""
        data-lpignore="true"
        data-bwignore=""
        data-form-type="other"
        data-secret-masked={shown ? undefined : ""}
        className={cn(!shown && "[-webkit-text-security:disc]")}
      />
      <InputGroupAddon align="inline-end">
        <InputGroupButton
          size="icon-xs"
          aria-label={label}
          title={label}
          aria-pressed={shown}
          aria-controls={props.id}
          onClick={() => setShown((s) => !s)}
        >
          {shown ? <EyeOffIcon /> : <EyeIcon />}
        </InputGroupButton>
      </InputGroupAddon>
    </InputGroup>
  )
}
