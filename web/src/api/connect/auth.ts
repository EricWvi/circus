import { createClient, type Transport } from "@connectrpc/connect"
import { createConnectTransport } from "@connectrpc/connect-web"
import {
  AuthService as AuthServiceDesc,
  User_Role,
  type LoginMethods as ProtoLoginMethods,
  type User as ProtoUser,
  type WhoAmIResponse,
} from "@circus/proto/circus/v1/auth_pb"

import { ApiError } from "@/api/errors"
import type { AuthService, AuthState, LoginMethods, Role, User } from "@/api/types"

import { connectErrorBodiesOnly, toApiError } from "./errors"

/** A response the client can't make sense of; the server broke its contract. */
const malformed = (what: string) =>
  new ApiError("internal", "Something went wrong. Try again.", undefined, {
    cause: new Error(`malformed ${what} response`),
  })

/** Anything but a super user gets the least privilege. */
export const toRole = (role: User_Role): Role => (role === User_Role.SUPER ? "super" : "user")

function toUser(user: ProtoUser | undefined, rpc: string): User {
  if (!user) throw malformed(rpc)
  return { userId: user.userId, email: user.email, role: toRole(user.role) }
}

function toMethods(methods: ProtoLoginMethods | undefined): LoginMethods {
  const out: LoginMethods = { password: methods?.password ?? false }
  if (methods?.oidc) out.oidc = { displayName: methods.oidc.displayName }
  return out
}

export function toAuthState(res: WhoAmIResponse): AuthState {
  switch (res.state.case) {
    case "needsSetup":
      return { kind: "needs_setup", methods: toMethods(res.methods) }
    case "signedOut":
      return { kind: "signed_out", methods: toMethods(res.methods) }
    case "user":
      return { kind: "signed_in", user: toUser(res.state.value, "WhoAmI") }
    default:
      throw malformed("WhoAmI")
  }
}

/** Runs a call, turning anything it throws into an `ApiError`. */
async function call<T>(run: () => Promise<T>): Promise<T> {
  try {
    return await run()
  } catch (error) {
    throw toApiError(error)
  }
}

/** The auth service of a real control plane, over Connect. */
export function createConnectAuth(transport: Transport): AuthService {
  const client = createClient(AuthServiceDesc, transport)
  return {
    whoAmI: () => call(async () => toAuthState(await client.whoAmI({}))),
    setup: (req) => call(async () => toUser((await client.setup(req)).user, "Setup")),
    login: (req) => call(async () => toUser((await client.login(req)).user, "Login")),
    logout: () => call(async () => void (await client.logout({}))),
  }
}

/** The auth service of the control plane at `baseUrl`, over Connect with JSON. */
export function createServerAuth(baseUrl: string): AuthService {
  return createConnectAuth(
    createConnectTransport({
      baseUrl,
      // Same origin, so cookies flow anyway; explicit for clarity.
      fetch: connectErrorBodiesOnly((input, init) =>
        fetch(input, { ...init, credentials: "include" }),
      ),
    }),
  )
}
