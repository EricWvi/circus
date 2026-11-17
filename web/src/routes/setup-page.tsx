import { useQueryClient } from "@tanstack/react-query"
import { useState, type FormEvent } from "react"
import { Trans, useTranslation } from "react-i18next"
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

/**
 * Why setup failed, kept as the cause so its text follows the language. An
 * invalid field is checked again at render; editing clears the problem, so
 * the values are still the submitted ones.
 */
type SetupProblem =
  | { kind: "redirect"; code: string }
  | { kind: "invalid"; field: "email" | "password" | "confirm" }
  | { kind: "failed"; error: unknown }

export function SetupPage() {
  const { t } = useTranslation()
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
            <Trans
              i18nKey="auth.alreadySetUpSignIn"
              components={{
                signIn: <Link to="/login" className="text-foreground underline underline-offset-4" />,
              }}
            />
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
          <p>{t("auth.openSetupLink")}</p>
        </AuthMessage>
      </AuthCard>
    )
  }
  return (
    <SetupForm
      token={token}
      methods={auth.data?.kind === "needs_setup" ? auth.data.methods : undefined}
      redirectError={failed ? error : null}
      onBlocked={setBlocked}
    />
  )
}

function SetupForm({
  token,
  methods,
  redirectError,
  onBlocked,
}: {
  token: string
  methods?: LoginMethods
  /** The code of a failed provider setup the server redirected back with. */
  redirectError: string | null
  onBlocked: (reason: Blocked) => void
}) {
  const { t } = useTranslation()
  const queryClient = useQueryClient()
  const setup = useSetup()
  const oidcSetup = useOidcSetup()
  const quietAutoFocus = useQuietAutoFocus()
  const [email, setEmail] = useState("")
  const [password, setPassword] = useState("")
  const [confirm, setConfirm] = useState("")
  const [problem, setProblem] = useState<SetupProblem | null>(() =>
    redirectError ? { kind: "redirect", code: redirectError } : null,
  )
  const error = describe(problem)

  function describe(p: SetupProblem | null): { message: string; field: Invalid } | null {
    if (!p) return null
    if (p.kind === "redirect") {
      const redirect = setupRedirectError(p.code)
      return redirect?.kind === "failed" ? { message: redirect.message, field: null } : null
    }
    if (p.kind === "invalid") {
      const message =
        p.field === "email"
          ? validateEmail(email)
          : p.field === "password"
            ? validatePassword(password)
            : t("auth.passwordsDontMatch")
      return { message: message ?? "", field: p.field }
    }
    const field = isApiError(p.error, "invalid_argument") && p.error.field
    return {
      message: authErrorMessage(p.error),
      field: field === "email" || field === "password" ? field : null,
    }
  }

  const onError = (e: unknown) => {
    if (isInvalidSetupToken(e)) return onBlocked("invalid_token")
    if (isApiError(e, "already_exists")) {
      // Someone else finished setup first.
      void queryClient.invalidateQueries({ queryKey: queryKeys.auth })
      return onBlocked("already_set_up")
    }
    setProblem({ kind: "failed", error: e })
  }

  // On success the auth guard moves the signed-in user to the home page.
  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (validateEmail(email)) return setProblem({ kind: "invalid", field: "email" })
    if (validatePassword(password)) return setProblem({ kind: "invalid", field: "password" })
    if (confirm !== password) return setProblem({ kind: "invalid", field: "confirm" })
    setProblem(null)
    setup.mutate({ token, email, password }, { onError })
  }

  const continueWithOidc = () => {
    setProblem(null)
    oidcSetup.mutate(token, { onError })
  }

  const edit = (set: (value: string) => void) => (value: string) => {
    set(value)
    setProblem(null)
  }
  const invalid = (field: Invalid) => (error?.field === field ? true : undefined)
  const pending = setup.isPending || oidcSetup.isPending

  return (
    <AuthCard>
      <form onSubmit={submit} noValidate className="flex flex-col gap-5">
        <FieldGroup className="gap-4">
          <Field>
            <FieldLabel htmlFor="email">{t("auth.email")}</FieldLabel>
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
            <FieldLabel htmlFor="password">{t("auth.password")}</FieldLabel>
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
            <FieldLabel htmlFor="confirm">{t("auth.confirmPassword")}</FieldLabel>
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
          {setup.isPending && <Spinner aria-label={t("auth.creatingAccount")} />}
          {t("auth.createAccount")}
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
