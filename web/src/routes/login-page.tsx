import { useState, type FormEvent, type ReactNode } from "react"
import { useSearchParams } from "react-router"

import { authErrorMessage } from "@/api/auth-messages"
import { useAuth, useLogin, useOidcLogin } from "@/api/hooks"
import { AuthCard, FormError, OidcButton, OrDivider } from "@/components/auth/auth-card"
import { GitHubMark, GoogleMark } from "@/components/auth/brand-marks"
import { Button } from "@/components/ui/button"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip"
import { useQuietAutoFocus } from "@/hooks/use-quiet-autofocus"
import { useRedirectErrorCode } from "@/hooks/use-redirect-error"
import { loginRedirectError } from "@/lib/auth-errors"
import { safeNext } from "@/lib/auth-redirect"

export function LoginPage() {
  const auth = useAuth()
  const methods = auth.data?.kind === "signed_out" ? auth.data.methods : undefined
  const [params] = useSearchParams()
  const login = useLogin()
  const oidcLogin = useOidcLogin()
  const quietAutoFocus = useQuietAutoFocus()
  const redirectError = useRedirectErrorCode()
  const [email, setEmail] = useState("")
  const [password, setPassword] = useState("")
  const [error, setError] = useState<string | null>(() => loginRedirectError(redirectError))

  // On success the auth guard moves the user on to `next`.
  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (email.trim() === "" || password === "") {
      setError("Enter your email and password.")
      return
    }
    setError(null)
    login.mutate({ email, password }, { onError: (e) => setError(authErrorMessage(e)) })
  }

  const continueWithOidc = () => {
    setError(null)
    const next = safeNext(params.get("next"))
    oidcLogin.mutate(next === "/" ? undefined : next, {
      onError: (e) => setError(authErrorMessage(e)),
    })
  }

  const edit = (set: (value: string) => void) => (value: string) => {
    set(value)
    setError(null)
  }
  const pending = login.isPending || oidcLogin.isPending

  return (
    <AuthCard>
      <form onSubmit={submit} noValidate className="flex flex-col gap-5">
        <FieldGroup className="gap-4">
          <Field>
            <FieldLabel htmlFor="email">Email</FieldLabel>
            <Input
              id="email"
              {...quietAutoFocus}
              name="email"
              type="email"
              autoComplete="username"
              autoCapitalize="none"
              spellCheck={false}
              value={email}
              onChange={(e) => edit(setEmail)(e.target.value)}
              aria-invalid={error !== null && email.trim() === "" ? true : undefined}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="password">Password</FieldLabel>
            <Input
              id="password"
              name="password"
              type="password"
              autoComplete="current-password"
              value={password}
              onChange={(e) => edit(setPassword)(e.target.value)}
              aria-invalid={error !== null && password === "" ? true : undefined}
            />
          </Field>
        </FieldGroup>
        <FormError>{error}</FormError>
        <Button type="submit" className="w-full" disabled={pending}>
          {login.isPending && <Spinner aria-label="Signing in" />}
          Sign in
        </Button>
      </form>
      <OrDivider />
      {methods?.oidc && (
        <OidcButton
          displayName={methods.oidc.displayName}
          pending={oidcLogin.isPending}
          disabled={login.isPending}
          onClick={continueWithOidc}
        />
      )}
      <div className="flex justify-center gap-2">
        <UnavailableProvider name="GitHub">
          <GitHubMark />
        </UnavailableProvider>
        <UnavailableProvider name="Google">
          <GoogleMark />
        </UnavailableProvider>
      </div>
    </AuthCard>
  )
}

/**
 * A sign-in provider that is planned but not offered yet. A click opens the
 * tooltip too (tooltips normally close on click), so it still explains.
 */
function UnavailableProvider({ name, children }: { name: string; children: ReactNode }) {
  const [open, setOpen] = useState(false)
  return (
    <Tooltip open={open} onOpenChange={setOpen}>
      <TooltipTrigger asChild>
        <Button
          type="button"
          variant="outline"
          size="icon-lg"
          aria-label={`Sign in with ${name}`}
          aria-disabled="true"
          onClick={(event) => {
            // Keeps the tooltip's own close-on-click from running.
            event.preventDefault()
            setOpen(true)
          }}
        >
          {children}
        </Button>
      </TooltipTrigger>
      <TooltipContent side="bottom" sideOffset={4}>
        Not available yet
      </TooltipContent>
    </Tooltip>
  )
}
