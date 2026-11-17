// What the sign-in and setup pages say about a failed auth call. Messages
// are chosen by error code (and field) on the client; the server's own
// message text is for logs, not for people.

import { i18n } from "@/i18n/i18n"

import { isApiError } from "./errors"
import { PASSWORD_MAX, PASSWORD_MIN } from "./validation"

/** The messages, read from the catalog in the active language at each use. */
export const authMessages = {
  get wrongCredentials() {
    return i18n.t("auth.errors.wrongCredentials")
  },
  get tooManyAttempts() {
    return i18n.t("auth.errors.tooManyAttempts")
  },
  get invalidEmail() {
    return i18n.t("auth.errors.invalidEmail")
  },
  get invalidPassword() {
    return i18n.t("auth.errors.invalidPassword", { min: PASSWORD_MIN, max: PASSWORD_MAX })
  },
  get invalidSetupLink() {
    return i18n.t("auth.errors.invalidSetupLink")
  },
  get wrongOrigin() {
    return i18n.t("auth.errors.wrongOrigin")
  },
  get alreadySetUp() {
    return i18n.t("auth.errors.alreadySetUp")
  },
  get notSetUp() {
    return i18n.t("auth.errors.notSetUp")
  },
  get unreachable() {
    return i18n.t("errors.unreachable")
  },
  get generic() {
    return i18n.t("errors.generic")
  },
}

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
