/** The subset of Connect error codes the UI distinguishes. */
export type ErrorCode =
  | "unauthenticated"
  | "invalid_argument"
  | "not_found"
  | "already_exists"
  | "failed_precondition"
  | "permission_denied"
  | "unavailable"

export class ApiError extends Error {
  readonly code: ErrorCode
  /**
   * The request field the error is about, as a path like "name" or
   * "locations[1].path" (a Connect error's BadRequest field violation).
   */
  readonly field?: string

  constructor(code: ErrorCode, message: string, field?: string) {
    super(message)
    this.name = "ApiError"
    this.code = code
    this.field = field
  }
}

export function isApiError(error: unknown, code?: ErrorCode): error is ApiError {
  return error instanceof ApiError && (code === undefined || error.code === code)
}

/** A one-line message suitable for showing inside a form. */
export function errorMessage(error: unknown): string {
  if (isApiError(error, "unavailable")) return "Can't reach the server. Try again."
  if (error instanceof ApiError) return error.message
  return "Something went wrong. Try again."
}
