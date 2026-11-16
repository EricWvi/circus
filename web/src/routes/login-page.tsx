import { useState, type FormEvent } from "react"

import { errorMessage } from "@/api/errors"
import { useLogin } from "@/api/hooks"
import { AuthCard, FormError } from "@/components/auth/auth-card"
import { Button } from "@/components/ui/button"
import { Field, FieldGroup, FieldLabel } from "@/components/ui/field"
import { Input } from "@/components/ui/input"
import { Spinner } from "@/components/ui/spinner"
import { useQuietAutoFocus } from "@/hooks/use-quiet-autofocus"

export function LoginPage() {
  const login = useLogin()
  const quietAutoFocus = useQuietAutoFocus()
  const [username, setUsername] = useState("")
  const [password, setPassword] = useState("")
  const [error, setError] = useState<string | null>(null)

  // On success the auth guard moves the user on to `next`.
  const submit = (event: FormEvent) => {
    event.preventDefault()
    if (username === "" || password === "") {
      setError("Enter your username and password.")
      return
    }
    setError(null)
    login.mutate({ username, password }, { onError: (e) => setError(errorMessage(e)) })
  }

  const edit = (set: (value: string) => void) => (value: string) => {
    set(value)
    setError(null)
  }

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
              aria-invalid={error !== null && username === "" ? true : undefined}
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
        <Button type="submit" className="w-full" disabled={login.isPending}>
          {login.isPending && <Spinner aria-label="Signing in" />}
          Sign in
        </Button>
      </form>
    </AuthCard>
  )
}
