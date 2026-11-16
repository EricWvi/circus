import { describe, expect, it } from "vitest"

import { loginRedirectError, setupRedirectError } from "./auth-errors"

describe("loginRedirectError", () => {
  it("shows nothing without a code", () => {
    expect(loginRedirectError(null)).toBeNull()
    expect(loginRedirectError("")).toBeNull()
  })

  it("explains the server's codes", () => {
    expect(loginRedirectError("oidc_failed")).toMatch(/failed/)
    expect(loginRedirectError("email_unverified")).toMatch(/not verified/)
    expect(loginRedirectError("too_many_attempts")).toMatch(/Too many attempts/)
  })

  it("falls back to a generic failure for unknown codes", () => {
    expect(loginRedirectError("something_new")).toBe(loginRedirectError("oidc_failed"))
  })
})

describe("setupRedirectError", () => {
  it("shows nothing without a code", () => {
    expect(setupRedirectError(null)).toBeNull()
    expect(setupRedirectError("")).toBeNull()
  })

  it("maps the server's codes", () => {
    expect(setupRedirectError("invalid_token")).toEqual({ kind: "invalid_token" })
    expect(setupRedirectError("already_set_up")).toEqual({ kind: "already_set_up" })
    expect(setupRedirectError("oidc_failed")).toEqual({
      kind: "failed",
      message: expect.stringMatching(/failed/),
    })
    expect(setupRedirectError("too_many_attempts")).toEqual({
      kind: "failed",
      message: expect.stringMatching(/Too many attempts/),
    })
    expect(setupRedirectError("email_unverified")).toEqual({
      kind: "failed",
      message: expect.stringMatching(/not verified/),
    })
  })

  it("treats unknown codes as a failed attempt", () => {
    expect(setupRedirectError("something_new")).toMatchObject({ kind: "failed" })
  })
})
