// The OpenID Connect flow is a browser redirect handled by the server, not
// an RPC. These build its entry URLs; the server redirects back to /login or
// /setup, with `?error=<code>` when it failed.

import type { OidcFlow } from "./types"

const LOGIN_PATH = "/auth/oidc/login"

export function oidcLoginUrl(next?: string): string {
  if (!next) return LOGIN_PATH
  return `${LOGIN_PATH}?${new URLSearchParams({ next })}`
}

export function oidcSetupUrl(token: string): string {
  return `${LOGIN_PATH}?${new URLSearchParams({ setup_token: token })}`
}

/** Leaves the app for the provider; the promise never settles. */
function leaveFor(url: string): Promise<void> {
  window.location.assign(url)
  return new Promise(() => {})
}

/** The flow of a client talking to a real server. */
export const browserOidcFlow: OidcFlow = {
  startLogin: (next) => leaveFor(oidcLoginUrl(next)),
  startSetup: (token) => leaveFor(oidcSetupUrl(token)),
}
