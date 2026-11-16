import { BinaryWriter, WireType } from "@bufbuild/protobuf/wire"
import { Code, ConnectError, createRouterTransport, type ServiceImpl } from "@connectrpc/connect"
import { createConnectTransport } from "@connectrpc/connect-web"
import { AuthService, User_Role } from "@circus/proto/circus/v1/auth_pb"
import { describe, expect, it } from "vitest"

import { ApiError, type ErrorCode } from "@/api/errors"

import { createConnectAuth } from "./auth"
import { badRequestFields, connectErrorBodiesOnly } from "./errors"

/** A client against a fake AuthService; unimplemented methods fail as unimplemented. */
function client(impl: Partial<ServiceImpl<typeof AuthService>>) {
  return createConnectAuth(createRouterTransport(({ service }) => service(AuthService, impl)))
}

/** Serializes a google.rpc.BadRequest with one violation per field. */
function badRequest(...fields: string[]): Uint8Array {
  const w = new BinaryWriter()
  for (const field of fields) {
    w.tag(1, WireType.LengthDelimited).fork()
    w.tag(1, WireType.LengthDelimited).string(field)
    w.tag(2, WireType.LengthDelimited).string(`${field} is wrong`)
    w.join()
  }
  return w.finish()
}

const badRequestDetail = (...fields: string[]) => ({
  type: "google.rpc.BadRequest",
  value: badRequest(...fields),
})

/** A ConnectError carrying details as raw `Any` values, the way a Go server sends them. */
function withDetails(message: string, code: Code, details: { type: string; value: Uint8Array }[]) {
  const error = new ConnectError(message, code)
  error.details.push(...details)
  return error
}

async function rejection(promise: Promise<unknown>): Promise<ApiError> {
  const error = await promise.then(
    () => undefined,
    (e: unknown) => e,
  )
  expect(error).toBeInstanceOf(ApiError)
  return error as ApiError
}

const oidc = { displayName: "Authelia" }

describe("whoAmI", () => {
  it("maps each state with the login methods", async () => {
    expect(
      await client({
        whoAmI: () => ({ state: { case: "needsSetup", value: {} }, methods: { password: true, oidc } }),
      }).whoAmI(),
    ).toEqual({ kind: "needs_setup", methods: { password: true, oidc } })

    expect(
      await client({
        whoAmI: () => ({ state: { case: "signedOut", value: {} }, methods: { password: true } }),
      }).whoAmI(),
    ).toStrictEqual({ kind: "signed_out", methods: { password: true } })
  })

  it("maps the signed-in user and their role", async () => {
    const whoAmI = (role: User_Role) =>
      client({
        whoAmI: () => ({
          state: { case: "user", value: { userId: "u1", email: "a@example.com", role } },
          methods: { password: true },
        }),
      }).whoAmI()

    expect(await whoAmI(User_Role.SUPER)).toEqual({
      kind: "signed_in",
      user: { userId: "u1", email: "a@example.com", role: "super" },
    })
    expect(await whoAmI(User_Role.USER)).toMatchObject({ user: { role: "user" } })
    // An unknown role gets the least privilege.
    expect(await whoAmI(User_Role.UNSPECIFIED)).toMatchObject({ user: { role: "user" } })
  })

  it("rejects a response without a state", async () => {
    const error = await rejection(client({ whoAmI: () => ({ methods: { password: true } }) }).whoAmI())
    expect(error.code).toBe("internal")
  })
})

describe("setup and login", () => {
  it("sends the request and returns the user", async () => {
    const seen: unknown[] = []
    const auth = client({
      setup: (req) => {
        seen.push({ token: req.token, email: req.email, password: req.password })
        return { user: { userId: "u1", email: "admin@example.com", role: User_Role.SUPER } }
      },
      login: (req) => {
        seen.push({ email: req.email, password: req.password })
        return { user: { userId: "u2", email: "dev@example.com", role: User_Role.USER } }
      },
    })

    expect(await auth.setup({ token: "t", email: "admin@example.com", password: "password1" })).toEqual({
      userId: "u1",
      email: "admin@example.com",
      role: "super",
    })
    expect(await auth.login({ email: "dev@example.com", password: "secret12" })).toEqual({
      userId: "u2",
      email: "dev@example.com",
      role: "user",
    })
    expect(seen).toEqual([
      { token: "t", email: "admin@example.com", password: "password1" },
      { email: "dev@example.com", password: "secret12" },
    ])
  })

  it("maps errors by code and keeps the server's text out of the message", async () => {
    const auth = client({
      login: () => {
        throw new ConnectError("incorrect email or password", Code.Unauthenticated)
      },
    })
    const error = await rejection(auth.login({ email: "a@example.com", password: "x" }))
    expect(error.code).toBe("unauthenticated")
    expect(error.field).toBeUndefined()
    expect(error.message).not.toMatch(/incorrect/)
    expect((error.cause as ConnectError).rawMessage).toBe("incorrect email or password")
  })

  it("takes the field from a BadRequest detail", async () => {
    const failWith = (code: Code, ...fields: string[]) =>
      client({
        setup: () => {
          throw withDetails("rejected", code, [badRequestDetail(...fields)])
        },
      }).setup({ token: "t", email: "x", password: "y" })

    expect(await rejection(failWith(Code.InvalidArgument, "email"))).toMatchObject({
      code: "invalid_argument",
      field: "email",
    })
    expect(await rejection(failWith(Code.PermissionDenied, "token"))).toMatchObject({
      code: "permission_denied",
      field: "token",
    })
    // The first violation names the field.
    expect((await rejection(failWith(Code.InvalidArgument, "password", "email"))).field).toBe("password")
    // ...the first one that names one at all.
    expect((await rejection(failWith(Code.InvalidArgument, "", "email"))).field).toBe("email")
    expect((await rejection(failWith(Code.InvalidArgument, ""))).field).toBeUndefined()
  })

  it("ignores other details and malformed BadRequests", async () => {
    const auth = (details: { type: string; value: Uint8Array }[]) =>
      client({
        login: () => {
          throw withDetails("rejected", Code.InvalidArgument, details)
        },
      }).login({ email: "a", password: "b" })

    const other = { type: "google.rpc.ErrorInfo", value: badRequest("email") }
    expect((await rejection(auth([other]))).field).toBeUndefined()
    const truncated = { type: "google.rpc.BadRequest", value: badRequest("email").slice(0, 4) }
    expect((await rejection(auth([truncated]))).field).toBeUndefined()
    expect((await rejection(auth([truncated, badRequestDetail("password")]))).field).toBe("password")
  })

  it("maps every Connect code to the code of the same name", async () => {
    const names: [Code, ErrorCode][] = [
      [Code.Canceled, "canceled"],
      [Code.Unknown, "unknown"],
      [Code.InvalidArgument, "invalid_argument"],
      [Code.DeadlineExceeded, "deadline_exceeded"],
      [Code.NotFound, "not_found"],
      [Code.AlreadyExists, "already_exists"],
      [Code.PermissionDenied, "permission_denied"],
      [Code.ResourceExhausted, "resource_exhausted"],
      [Code.FailedPrecondition, "failed_precondition"],
      [Code.Aborted, "aborted"],
      [Code.OutOfRange, "out_of_range"],
      [Code.Unimplemented, "unimplemented"],
      [Code.Internal, "internal"],
      [Code.Unavailable, "unavailable"],
      [Code.DataLoss, "data_loss"],
      [Code.Unauthenticated, "unauthenticated"],
    ]
    for (const [code, name] of names) {
      const auth = client({
        login: () => {
          throw new ConnectError("x", code)
        },
      })
      expect((await rejection(auth.login({ email: "a", password: "b" }))).code).toBe(name)
    }
  })
})

describe("logout", () => {
  it("calls the server", async () => {
    let calls = 0
    const auth = client({
      logout: () => {
        calls++
        return {}
      },
    })
    await expect(auth.logout()).resolves.toBeUndefined()
    expect(calls).toBe(1)
  })
})

describe("transport failures", () => {
  const over = (fetch: typeof globalThis.fetch) =>
    createConnectAuth(createConnectTransport({ baseUrl: "http://circus.invalid", fetch }))

  it("reads a failed request as unavailable", async () => {
    const auth = over(() => Promise.reject(new TypeError("Failed to fetch")))
    expect((await rejection(auth.whoAmI())).code).toBe("unavailable")
  })

  it("reads a proxy's plain error page as unavailable", async () => {
    const auth = over(() =>
      Promise.resolve(new Response("", { status: 500, headers: { "content-type": "text/plain" } })),
    )
    expect((await rejection(auth.whoAmI())).code).toBe("unavailable")
  })

  it("keeps an unknown error the server sent as unknown", async () => {
    const auth = over(() =>
      Promise.resolve(
        Response.json({ code: "unknown", message: "boom" }, { status: 500 }),
      ),
    )
    expect((await rejection(auth.whoAmI())).code).toBe("unknown")
  })

  it("doesn't guess a code from the status of a response the server didn't send", async () => {
    for (const status of [401, 403, 404, 429, 500, 502]) {
      const auth = over(() =>
        Promise.resolve(new Response("Forbidden", { status, headers: { "content-type": "text/html" } })),
      )
      // A proxy's 401 must not read as wrong credentials, nor 403 as a bad setup link.
      expect((await rejection(auth.login({ email: "a", password: "b" }))).code).toBe("unavailable")
    }
  })

  it("reads a success that isn't a Connect response as unavailable", async () => {
    const auth = over(() =>
      Promise.resolve(new Response("<html>", { status: 200, headers: { "content-type": "text/html" } })),
    )
    expect((await rejection(auth.whoAmI())).code).toBe("unavailable")
  })

  it("keeps a canceled request canceled", async () => {
    const auth = over(() => Promise.reject(new DOMException("aborted", "AbortError")))
    expect((await rejection(auth.whoAmI())).code).toBe("canceled")
  })
})

describe("connectErrorBodiesOnly", () => {
  const over = (fetch: typeof globalThis.fetch) =>
    createConnectAuth(
      createConnectTransport({ baseUrl: "http://circus.invalid", fetch: connectErrorBodiesOnly(fetch) }),
    )
  const login = (body: unknown, status: number) =>
    rejection(over(() => Promise.resolve(Response.json(body, { status }))).login({ email: "a", password: "b" }))

  it("reads a JSON error body that isn't a Connect error as unavailable", async () => {
    // Without the wrapper Connect takes the code from the status: unauthenticated.
    expect((await login({ error: "blocked by policy" }, 401)).code).toBe("unavailable")
    expect((await login({}, 500)).code).toBe("unavailable")
    expect((await login(["x"], 403)).code).toBe("unavailable")
  })

  it("passes Connect error bodies through", async () => {
    expect((await login({ code: "unauthenticated", message: "nope" }, 401)).code).toBe("unauthenticated")
    // An unknown error without a message is still the server's own answer.
    expect((await login({ code: "unknown" }, 500)).code).toBe("unknown")
  })

  it("leaves successful responses alone", async () => {
    const auth = over(() =>
      Promise.resolve(Response.json({ signedOut: {}, methods: { password: true } }, { status: 200 })),
    )
    expect(await auth.whoAmI()).toEqual({ kind: "signed_out", methods: { password: true } })
  })
})

describe("badRequestFields", () => {
  it("reads the field of each violation", () => {
    expect(badRequestFields(badRequest("email", "locations[1].path"))).toEqual(["email", "locations[1].path"])
    expect(badRequestFields(new Uint8Array())).toEqual([])
  })

  it("reads the bytes the Go server sends", () => {
    // BadRequest{field_violations: [{field: "email", description: "invalid email address"}]}
    const bytes = new Uint8Array([
      0x0a, 0x1e, 0x0a, 0x05, ...new TextEncoder().encode("email"),
      0x12, 0x15, ...new TextEncoder().encode("invalid email address"),
    ])
    expect(badRequestFields(bytes)).toEqual(["email"])
  })

  it("skips unknown fields", () => {
    const w = new BinaryWriter()
    w.tag(7, WireType.Varint).int32(3)
    w.tag(1, WireType.LengthDelimited).fork()
    w.tag(9, WireType.Bit64).fixed64(1n)
    w.tag(1, WireType.LengthDelimited).string("name")
    w.join()
    expect(badRequestFields(w.finish())).toEqual(["name"])
  })

  it("throws on malformed input", () => {
    expect(() => badRequestFields(badRequest("email").slice(0, 5))).toThrow()
  })
})
