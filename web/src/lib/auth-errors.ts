// The OIDC flow ends with the server redirecting back to /login or /setup,
// with `?error=<code>` when it failed. These turn the codes into what the
// pages show.

import { authMessages } from "@/api/auth-messages"

const unverified = "Your account's email address is not verified with the provider."
const providerFailed = "Sign-in with the provider failed. Try again."

/** The message for a code the server put on /login, or null for none. */
export function loginRedirectError(code: string | null): string | null {
  switch (code) {
    case null:
    case "":
      return null
    case "email_unverified":
      return unverified
    case "too_many_attempts":
      return authMessages.tooManyAttempts
    case "oidc_failed":
    default:
      return providerFailed
  }
}

export type SetupRedirectError =
  /** The setup link is not (or no longer) valid; the form can't be used. */
  | { kind: "invalid_token" }
  /** A super user already exists. */
  | { kind: "already_set_up" }
  /** Setup failed for this attempt; the form can be used again. */
  | { kind: "failed"; message: string }

/** What a code the server put on /setup means, or null for none. */
export function setupRedirectError(code: string | null): SetupRedirectError | null {
  switch (code) {
    case null:
    case "":
      return null
    case "invalid_token":
      return { kind: "invalid_token" }
    case "already_set_up":
      return { kind: "already_set_up" }
    case "email_unverified":
      return { kind: "failed", message: unverified }
    case "too_many_attempts":
      return { kind: "failed", message: authMessages.tooManyAttempts }
    case "oidc_failed":
    default:
      return { kind: "failed", message: "Setup with the provider failed. Try again." }
  }
}
