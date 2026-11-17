import { ApiError } from "@/api/errors"

import { toApiError } from "./errors"

/** A response the client can't make sense of; the server broke its contract. */
export const malformed = (what: string) =>
  new ApiError("internal", "Something went wrong. Try again.", undefined, {
    cause: new Error(`malformed ${what} response`),
  })

/** Runs a call, turning anything it throws into an `ApiError`. */
export async function call<T>(run: () => Promise<T>): Promise<T> {
  try {
    return await run()
  } catch (error) {
    throw toApiError(error)
  }
}
