import { i18n } from "@/i18n/i18n"

/**
 * Connect error codes, by their protocol names. The UI distinguishes the
 * first group; the rest only reach it from a real server and read as a
 * generic failure.
 */
export type ErrorCode =
  | "unauthenticated"
  | "invalid_argument"
  | "not_found"
  | "already_exists"
  | "failed_precondition"
  | "permission_denied"
  | "resource_exhausted"
  | "unavailable"
  | "canceled"
  | "unknown"
  | "deadline_exceeded"
  | "aborted"
  | "out_of_range"
  | "unimplemented"
  | "internal"
  | "data_loss"

export class ApiError extends Error {
  readonly code: ErrorCode
  /**
   * The request field the error is about, as a path like "name" or
   * "locations[1].path" (a Connect error's BadRequest field violation).
   */
  readonly field?: string

  constructor(code: ErrorCode, message: string, field?: string, options?: ErrorOptions) {
    super(message, options)
    this.name = "ApiError"
    this.code = code
    this.field = field
  }
}

export function isApiError(error: unknown, code?: ErrorCode): error is ApiError {
  return error instanceof ApiError && (code === undefined || error.code === code)
}

/**
 * A one-line message suitable for showing inside a form, chosen by code: the
 * error's own message is for logs, not for people.
 */
export function errorMessage(error: unknown): string {
  if (isApiError(error, "unavailable")) return i18n.t("errors.unreachable")
  if (isApiError(error, "resource_exhausted")) return i18n.t("errors.tooManyAttempts")
  return i18n.t("errors.generic")
}
