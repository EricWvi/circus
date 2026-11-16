import { describe, expect, it } from "vitest"

import { authErrorMessage, authMessages, isInvalidSetupToken } from "./auth-messages"
import { ApiError } from "./errors"

describe("authErrorMessage", () => {
  it("picks the message by code, not the server's text", () => {
    const err = (code: ApiError["code"], field?: string) => new ApiError(code, "internal detail", field)
    expect(authErrorMessage(err("unauthenticated"))).toBe(authMessages.wrongCredentials)
    expect(authErrorMessage(err("resource_exhausted"))).toBe(authMessages.tooManyAttempts)
    expect(authErrorMessage(err("invalid_argument", "email"))).toBe(authMessages.invalidEmail)
    expect(authErrorMessage(err("invalid_argument", "password"))).toBe(authMessages.invalidPassword)
    expect(authErrorMessage(err("invalid_argument", "token"))).toBe(authMessages.invalidSetupLink)
    expect(authErrorMessage(err("invalid_argument"))).toBe(authMessages.generic)
    expect(authErrorMessage(err("permission_denied", "token"))).toBe(authMessages.invalidSetupLink)
    expect(authErrorMessage(err("permission_denied"))).toBe(authMessages.wrongOrigin)
    expect(authErrorMessage(err("unavailable"))).toBe(authMessages.unreachable)
    expect(authErrorMessage(new Error("boom"))).toBe(authMessages.generic)
  })

  it("blames the setup link only for a refused token", () => {
    expect(isInvalidSetupToken(new ApiError("permission_denied", "x", "token"))).toBe(true)
    // A failed Origin check: the link may be fine, the address is not.
    expect(isInvalidSetupToken(new ApiError("permission_denied", "x"))).toBe(false)
    expect(isInvalidSetupToken(new ApiError("invalid_argument", "x", "token"))).toBe(false)
    expect(isInvalidSetupToken(new Error("x"))).toBe(false)
  })
})
