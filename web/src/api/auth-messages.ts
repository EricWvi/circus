// What the sign-in and setup pages say about a failed auth call. Messages
// are chosen by error code (and field) on the client; the server's own
// message text is for logs, not for people.

import { isApiError } from "./errors"

export const authMessages = {
  wrongCredentials: "Wrong email or password.",
  tooManyAttempts: "Too many attempts. Try again in a few minutes.",
  invalidEmail: "Enter a valid email address.",
  invalidPassword: "Use a password of 8 to 72 characters.",
  invalidSetupLink: "This setup link is not valid.",
  wrongOrigin: "This page's address doesn't match the server's configured URL.",
  alreadySetUp: "Circus is already set up.",
  notSetUp: "Circus is not set up yet.",
  unreachable: "Can't reach the server. Try again.",
  generic: "Something went wrong. Try again.",
} as const

/**
 * Whether setup was refused for its token. The server refuses a bad token
 * with permission_denied on the field "token"; permission_denied without it
 * is a failed Origin check (the page is open on another address than the
 * server's public URL), which says nothing about the link.
 */
export function isInvalidSetupToken(error: unknown): boolean {
  return isApiError(error, "permission_denied") && error.field === "token"
}

/** A one-line message for an error from login, setup or the OIDC flow. */
export function authErrorMessage(error: unknown): string {
  if (!isApiError(error)) return authMessages.generic
  switch (error.code) {
    case "unauthenticated":
      return authMessages.wrongCredentials
    case "resource_exhausted":
      return authMessages.tooManyAttempts
    case "invalid_argument":
      if (error.field === "email") return authMessages.invalidEmail
      if (error.field === "password") return authMessages.invalidPassword
      if (error.field === "token") return authMessages.invalidSetupLink
      return authMessages.generic
    case "permission_denied":
      return error.field === "token" ? authMessages.invalidSetupLink : authMessages.wrongOrigin
    case "already_exists":
      return authMessages.alreadySetUp
    case "failed_precondition":
      return authMessages.notSetUp
    case "unavailable":
      return authMessages.unreachable
    default:
      return authMessages.generic
  }
}
