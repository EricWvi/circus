import { useQueryClient } from "@tanstack/react-query"
import { useState, type FormEvent } from "react"
import { useSearchParams } from "react-router"

import { errorMessage, isApiError } from "@/api/errors"
import { useSetup } from "@/api/hooks"
import { queryKeys } from "@/api/query-client"
import { validatePassword, validateUsername } from "@/api/validation"
import { AuthCard, FormError } from "@/components/auth/auth-card"
import { Button } from "@/components/ui/button"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { useQuietAutoFocus } from "@/hooks/use-quiet-autofocus"

type Invalid = "username" | "password" | "confirm" | null

export function SetupPage() {
  const [params] = useSearchParams()
  const token = params.get("token")
  const [rejected, setRejected] = useState(false)

  if (!token || rejected) {
    return (
      <AuthCard>
        <p className="text-center text-sm text-muted-foreground">
          Open the setup link printed in the server&apos;s terminal.
        </p>
      </AuthCard>
    )
  }
  return <SetupForm token={token} onRejected={() => setRejected(true)} />
}

function SetupForm({ token, onRejected }: { token: string; onRejected: () => void }) {
  const queryClient = useQueryClient()
  const setup = useSetup()
  const quietAutoFocus = useQuietAutoFocus()
  const [username, setUsername] = useState("")
  const [password, setPassword] = useState("")
  const [confirm, setConfirm] = useState("")
  const [error, setError] = useState<{ message: string; field: Invalid } | null>(null)

  // On success the auth guard moves the signed-in user to the home page.
  const submit = (event: FormEvent) => {
    event.preventDefault()
    const name = username.trim()
    const usernameProblem = validateUsername(name)
    if (usernameProblem) return setError({ message: usernameProblem, field: "username" })
    const passwordProblem = validatePassword(password)
    if (passwordProblem) return setError({ message: passwordProblem, field: "password" })
    if (confirm !== password) return setError({ message: "Passwords don't match.", field: "confirm" })
    setError(null)
    setup.mutate(
      { token, username: name, password },
      {
        onError: (e) => {
          if (isApiError(e, "permission_denied")) return onRejected()
          // Someone else finished setup first; let the guard re-route.
          if (isApiError(e, "failed_precondition")) {
            void queryClient.invalidateQueries({ queryKey: queryKeys.auth })
          }
          setError({ message: errorMessage(e), field: null })
        },
      },
    )
  }

  const edit = (set: (value: string) => void) => (value: string) => {
    set(value)
    setError(null)
  }
  const invalid = (field: Invalid) => (error?.field === field ? true : undefined)

  return (
    <AuthCard>
      <form onSubmit={submit} noValidate className="flex flex-col gap-5">
        <FieldGroup className="gap-4">
          <Field>
            <FieldLabel htmlFor="username">Username</FieldLabel>
            <Input
              id="username"
              {...quietAutoFocus}
              name="username"
              autoComplete="username"
              autoCapitalize="none"
              spellCheck={false}
              value={username}
              onChange={(e) => edit(setUsername)(e.target.value)}
              aria-invalid={invalid("username")}
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
        <Button type="submit" className="w-full" disabled={setup.isPending}>
          {setup.isPending && <Spinner aria-label="Creating account" />}
          Create account
        </Button>
      </form>
    </AuthCard>
  )
}
