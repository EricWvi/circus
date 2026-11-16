import { useQueryClient } from "@tanstack/react-query"
import { useState, type FormEvent } from "react"
import { Link } from "react-router"

import { authErrorMessage, authMessages, isInvalidSetupToken } from "@/api/auth-messages"
import { isApiError } from "@/api/errors"
import { useAuth, useOidcSetup, useSetup } from "@/api/hooks"
import { queryKeys } from "@/api/query-client"
import type { LoginMethods } from "@/api/types"
import { validateEmail, validatePassword } from "@/api/validation"
import {
  AuthCard,
  AuthMessage,
  FormError,
  OidcButton,
  OrDivider,
} from "@/components/auth/auth-card"
import { Button } from "@/components/ui/button"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { useQuietAutoFocus } from "@/hooks/use-quiet-autofocus"
import { useOneTimeParams } from "@/hooks/use-redirect-error"
import { setupRedirectError } from "@/lib/auth-errors"

type Invalid = "email" | "password" | "confirm" | null
type Blocked = "invalid_token" | "already_set_up" | null

const openTheLink = "Open the setup link printed in the server log."

export function SetupPage() {
  const auth = useAuth()
  // The token stays in this page's state only: it leaves the address bar
  // (and with it history and Referer), so a reload asks for the link again.
  const { token, error } = useOneTimeParams(["token", "error"])
  const redirect = setupRedirectError(error)
  const [blocked, setBlocked] = useState<Blocked>(
    redirect?.kind === "invalid_token" || redirect?.kind === "already_set_up" ? redirect.kind : null,
  )
  const failed = redirect?.kind === "failed" ? redirect.message : null

  // Signed out means a super user exists (the guard keeps everyone else away).
  if (blocked === "already_set_up" || auth.data?.kind === "signed_out") {
    return (
      <AuthCard>
        <AuthMessage>
          <p>
            {authMessages.alreadySetUp}{" "}
            <Link to="/login" className="text-foreground underline underline-offset-4">
              Sign in
            </Link>
          </p>
        </AuthMessage>
      </AuthCard>
    )
  }
  if (!token || blocked === "invalid_token") {
    return (
      <AuthCard>
        <AuthMessage>
          {blocked === "invalid_token" && <p className="text-foreground">{authMessages.invalidSetupLink}</p>}
          {failed && !token && (
            <p role="alert" className="text-destructive">
              {failed}
            </p>
          )}
          <p>{openTheLink}</p>
        </AuthMessage>
      </AuthCard>
    )
  }
  return (
    <SetupForm
      token={token}
      methods={auth.data?.kind === "needs_setup" ? auth.data.methods : undefined}
      initialError={failed}
      onBlocked={setBlocked}
    />
  )
}

function SetupForm({
  token,
  methods,
  initialError,
  onBlocked,
}: {
  token: string
  methods?: LoginMethods
  initialError: string | null
  onBlocked: (reason: Blocked) => void
}) {
  const queryClient = useQueryClient()
  const setup = useSetup()
  const oidcSetup = useOidcSetup()
  const quietAutoFocus = useQuietAutoFocus()
  const [email, setEmail] = useState("")
  const [password, setPassword] = useState("")
  const [confirm, setConfirm] = useState("")
  const [error, setError] = useState<{ message: string; field: Invalid } | null>(
    initialError ? { message: initialError, field: null } : null,
  )

  const onError = (e: unknown) => {
    if (isInvalidSetupToken(e)) return onBlocked("invalid_token")
    if (isApiError(e, "already_exists")) {
      // Someone else finished setup first.
      void queryClient.invalidateQueries({ queryKey: queryKeys.auth })
      return onBlocked("already_set_up")
    }
    const field = isApiError(e, "invalid_argument") && e.field
    setError({
      message: authErrorMessage(e),
      field: field === "email" || field === "password" ? field : null,
    })
  }

  // On success the auth guard moves the signed-in user to the home page.
  const submit = (event: FormEvent) => {
    event.preventDefault()
    const emailProblem = validateEmail(email)
    if (emailProblem) return setError({ message: emailProblem, field: "email" })
    const passwordProblem = validatePassword(password)
    if (passwordProblem) return setError({ message: passwordProblem, field: "password" })
    if (confirm !== password) return setError({ message: "Passwords don't match.", field: "confirm" })
    setError(null)
    setup.mutate({ token, email, password }, { onError })
  }

  const continueWithOidc = () => {
    setError(null)
    oidcSetup.mutate(token, { onError })
  }

  const edit = (set: (value: string) => void) => (value: string) => {
    set(value)
    setError(null)
  }
  const invalid = (field: Invalid) => (error?.field === field ? true : undefined)
  const pending = setup.isPending || oidcSetup.isPending

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
              aria-invalid={invalid("email")}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="password">Password</FieldLabel>
            <Input
              id="password"
              name="password"
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(e) => edit(setPassword)(e.target.value)}
              aria-invalid={invalid("password")}
            />
          </Field>
          <Field>
            <FieldLabel htmlFor="confirm">Confirm password</FieldLabel>
            <Input
              id="confirm"
              name="confirm"
              type="password"
              autoComplete="new-password"
              value={confirm}
              onChange={(e) => edit(setConfirm)(e.target.value)}
              aria-invalid={invalid("confirm")}
            />
          </Field>
        </FieldGroup>
        <FormError>{error?.message}</FormError>
        <Button type="submit" className="w-full" disabled={pending}>
          {setup.isPending && <Spinner aria-label="Creating account" />}
          Create account
        </Button>
      </form>
      {methods?.oidc && (
        <>
          <OrDivider />
          <OidcButton
            displayName={methods.oidc.displayName}
            pending={oidcSetup.isPending}
            disabled={setup.isPending}
            onClick={continueWithOidc}
          />
        </>
      )}
    </AuthCard>
  )
}
