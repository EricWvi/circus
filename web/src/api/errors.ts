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

  constructor(code: ErrorCode, message: string) {
    super(message)
    this.name = "ApiError"
    this.code = code
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
