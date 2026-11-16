import { describe, expect, it } from "vitest"

import { authRedirect, canonicalPath, safeNext } from "./auth-redirect"

const at = (path: string) => {
  const url = new URL(path, "http://localhost")
  return { pathname: url.pathname, search: url.search }
}
const methods = { password: true }
const signedIn = {
  kind: "signed_in",
  user: { userId: "u1", email: "eric@example.com", role: "super" },
} as const

describe("canonicalPath", () => {
  it("matches the way react-router does", () => {
    expect(canonicalPath("/%6Cogin/")).toBe("/login")
    // An encoded slash is part of a segment, not a separator.
    expect(canonicalPath("/login%2F")).toBe("/login%2f")
    // Malformed escapes are compared as written.
    expect(canonicalPath("/%E0%A4%A")).toBe("/%e0%a4%a")
  })
})

describe("safeNext", () => {
  it("keeps same-origin paths", () => {
    expect(safeNext("/p/circus/s/x?tab=1#end")).toBe("/p/circus/s/x?tab=1#end")
  })

  it("falls back to home for anything else", () => {
    const rejected = [
      null,
      "",
      "p/x",
      "//evil.example",
      "/\\evil.example",
      "https://evil.example/",
      "/login",
      "/setup?token=x",
      "/login/",
      "/LOGIN",
      "/Setup/?token=x",
      "/%6Cogin",
      "/%73etup?token=x",
    ]
    for (const next of rejected) expect(safeNext(next)).toBe("/")
  })

  it("rejects paths that normalize to another origin", () => {
    for (const next of ["/.//evil.com", "/%2e//evil.com", "/a/..//evil.com", "/./\\evil.com", "/.\\/evil.com"]) {
      expect(safeNext(next)).toBe("/")
    }
  })
})

describe("authRedirect", () => {
  it("sends everything to setup until the server is set up", () => {
    const auth = { kind: "needs_setup", methods } as const
    expect(authRedirect(auth, at("/"))).toBe("/setup")
    expect(authRedirect(auth, at("/login"))).toBe("/setup")
    // The setup page itself says the server is already set up.
    expect(authRedirect(auth, at("/setup?token=t"))).toBeNull()
  })

  it("carries an OIDC error from /login over to setup", () => {
    const auth = { kind: "needs_setup", methods } as const
    expect(authRedirect(auth, at("/login?error=oidc_failed&next=%2Fx"))).toBe("/setup?error=oidc_failed")
    expect(authRedirect(auth, at("/p/a?error=oidc_failed"))).toBe("/setup")
  })

  it("sends signed-out users to login with the page they asked for", () => {
    const auth = { kind: "signed_out", methods } as const
    expect(authRedirect(auth, at("/p/a/s/b?x=1"))).toBe("/login?next=%2Fp%2Fa%2Fs%2Fb%3Fx%3D1")
    expect(authRedirect(auth, at("/"))).toBe("/login")
    // The setup page itself says the server is already set up.
    expect(authRedirect(auth, at("/setup?token=t"))).toBeNull()
    expect(authRedirect(auth, at("/login?next=/x"))).toBeNull()
  })

  it("keeps the token when canonicalizing the setup path", () => {
    const auth = { kind: "needs_setup", methods } as const
    expect(authRedirect(auth, at("/setup/?token=t"))).toBe("/setup?token=t")
    expect(authRedirect(auth, at("/SETUP?token=t"))).toBe("/setup?token=t")
  })

  it("treats auth page variants like the pages themselves", () => {
    expect(authRedirect({ kind: "signed_out", methods }, at("/Login/"))).toBeNull()
    expect(authRedirect(signedIn, at("/login/"))).toBe("/")
    expect(authRedirect(signedIn, at("/LOGIN"))).toBe("/")
    expect(authRedirect(signedIn, at("/Setup?token=t"))).toBe("/")
    expect(authRedirect(signedIn, at("/%6Cogin"))).toBe("/")
    expect(authRedirect({ kind: "needs_setup", methods }, at("/%73etup?token=t"))).toBe("/setup?token=t")
    expect(authRedirect(signedIn, at("/login?next=%2Flogin%2F"))).toBe("/")
    expect(authRedirect(signedIn, at("/login?next=%2F.%2F%2Fevil.com"))).toBe("/")
  })

  it("drops the page after an explicit sign-out", () => {
    expect(authRedirect({ kind: "signed_out", methods, afterSignOut: true }, at("/p/a/s/b"))).toBe("/login")
  })

  it("moves signed-in users off the auth pages", () => {
    expect(authRedirect(signedIn, at("/login"))).toBe("/")
    expect(authRedirect(signedIn, at("/login?next=%2Fp%2Fa%2Fs%2Fb"))).toBe("/p/a/s/b")
    expect(authRedirect(signedIn, at("/login?next=https://evil.example"))).toBe("/")
    expect(authRedirect(signedIn, at("/setup?token=t"))).toBe("/")
    expect(authRedirect(signedIn, at("/p/a/s/b"))).toBeNull()
  })
})
