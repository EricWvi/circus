import { createClient, type Transport } from "@connectrpc/connect"
import {
  AuthService as AuthServiceDesc,
  User_Role,
  type LoginMethods as ProtoLoginMethods,
  type User as ProtoUser,
  type WhoAmIResponse,
} from "@circus/proto/circus/v1/auth_pb"

import type { AuthService, AuthState, LoginMethods, Role, User } from "@/api/types"

import { call, malformed } from "./call"

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
