import { ApiError } from "@/api/errors"
import { reportIfUnauthenticated } from "@/api/unauthenticated"

import { toApiError } from "./errors"

/** A response the client can't make sense of; the server broke its contract. */
export const malformed = (what: string) =>
  new ApiError("internal", "Something went wrong. Try again.", undefined, {
    cause: new Error(`malformed ${what} response`),
  })

/**
 * Runs a call, turning anything it throws into an `ApiError`. An
 * unauthenticated rejection is also reported (see `unauthenticated.ts`).
 */
export async function call<T>(run: () => Promise<T>): Promise<T> {
  try {
    return await run()
  } catch (error) {
    throw reportIfUnauthenticated(toApiError(error))
  }
}

/** Like `call`, for a server stream: every message passes through `map`. */
export async function* stream<M, T>(open: () => AsyncIterable<M>, map: (message: M) => T): AsyncGenerator<T> {
  try {
    for await (const message of open()) yield map(message)
  } catch (error) {
    throw reportIfUnauthenticated(toApiError(error))
  }
}
