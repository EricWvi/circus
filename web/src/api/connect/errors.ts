import { Code, ConnectError } from "@connectrpc/connect"
import { BinaryReader, WireType } from "@bufbuild/protobuf/wire"

import { ApiError, type ErrorCode } from "@/api/errors"

const codeNames: Record<Code, ErrorCode> = {
  [Code.Canceled]: "canceled",
  [Code.Unknown]: "unknown",
  [Code.InvalidArgument]: "invalid_argument",
  [Code.DeadlineExceeded]: "deadline_exceeded",
  [Code.NotFound]: "not_found",
  [Code.AlreadyExists]: "already_exists",
  [Code.PermissionDenied]: "permission_denied",
  [Code.ResourceExhausted]: "resource_exhausted",
  [Code.FailedPrecondition]: "failed_precondition",
  [Code.Aborted]: "aborted",
  [Code.OutOfRange]: "out_of_range",
  [Code.Unimplemented]: "unimplemented",
  [Code.Internal]: "internal",
  [Code.Unavailable]: "unavailable",
  [Code.DataLoss]: "data_loss",
  [Code.Unauthenticated]: "unauthenticated",
}

const BAD_REQUEST = "google.rpc.BadRequest"
const PRECONDITION_FAILURE = "google.rpc.PreconditionFailure"

/**
 * The first string field of each message in a serialized message's first
 * repeated message field: both `google.rpc.BadRequest` (field_violations,
 * then field) and `google.rpc.PreconditionFailure` (violations, then type)
 * hold what the client needs there. `gen/ts` has no schema for google.rpc,
 * so this reads the wire directly; everything else is skipped. Throws on
 * malformed input.
 */
function violationStrings(bytes: Uint8Array): string[] {
  const out: string[] = []
  const reader = new BinaryReader(bytes)
  while (reader.pos < reader.len) {
    const [no, type] = reader.tag()
    if (no !== 1 || type !== WireType.LengthDelimited) {
      reader.skip(type, no)
      continue
    }
    const violation = new BinaryReader(reader.bytes())
    let value = ""
    while (violation.pos < violation.len) {
      const [vno, vtype] = violation.tag()
      if (vno === 1 && vtype === WireType.LengthDelimited) value = violation.string()
      else violation.skip(vtype, vno)
    }
    out.push(value)
  }
  return out
}

/** The `field` of each field violation in a serialized `google.rpc.BadRequest`. */
export const badRequestFields = violationStrings

/** The `type` of each violation in a serialized `google.rpc.PreconditionFailure`. */
export const preconditionTypes = violationStrings

/** The type of the error's first PreconditionFailure violation that names one. */
function violatedPrecondition(error: ConnectError): string | undefined {
  for (const detail of error.details) {
    if (!("type" in detail) || detail.type !== PRECONDITION_FAILURE) continue
    try {
      const type = preconditionTypes(detail.value).find((t) => t !== "")
      if (type) return type
    } catch {
      // A malformed detail carries no type.
    }
  }
  return undefined
}

/** The field of the error's first BadRequest field violation that names one. */
function violatedField(error: ConnectError): string | undefined {
  for (const detail of error.details) {
    if (!("type" in detail) || detail.type !== BAD_REQUEST) continue
    try {
      const field = badRequestFields(detail.value).find((f) => f !== "")
      if (field) return field
    } catch {
      // A malformed detail carries no field.
    }
  }
  return undefined
}

/**
 * Converts whatever a Connect call threw into an `ApiError`. The code maps
 * one to one, the field comes from a BadRequest detail and the
 * precondition type from a PreconditionFailure detail. The server's
 * message text is kept only as the `cause`: what the UI says is chosen from
 * the code (see `auth-messages.ts`).
 */
export function toApiError(error: unknown): ApiError {
  if (error instanceof ApiError) return error
  const connectError = ConnectError.from(error)
  let code = codeNames[connectError.code] ?? "unknown"
  // An error the server didn't send means there was no usable answer: the
  // request failed, or something else (a proxy, a firewall) answered and the
  // code was only guessed from its HTTP status. A 401 from a proxy must not
  // read as wrong credentials. Only our own cancellation keeps its code.
  // (`isWireError` marks errors parsed from a Connect error body; see
  // `connectErrorBodiesOnly` for JSON bodies that aren't one.)
  if (!connectError.isWireError && code !== "canceled") code = "unavailable"
  return new ApiError(code, "Something went wrong. Try again.", violatedField(connectError), {
    cause: connectError,
    preconditionType: violatedPrecondition(connectError),
  })
}

/**
 * Wraps fetch so that an error response with a JSON body that isn't a
 * Connect error (one has a string `code`) reaches Connect as a plain-text
 * response. Connect would otherwise read such a body as a server error with
 * a code guessed from the HTTP status; as plain text it is an error the
 * server didn't send, which `toApiError` reads as unavailable.
 */
export function connectErrorBodiesOnly(fetch: typeof globalThis.fetch): typeof globalThis.fetch {
  return async (input, init) => {
    const res = await fetch(input, init)
    if (res.ok || !res.headers.get("content-type")?.toLowerCase().startsWith("application/json")) {
      return res
    }
    const body = await res.clone().text()
    let isConnectError = false
    try {
      const json: unknown = JSON.parse(body)
      isConnectError =
        typeof json === "object" &&
        json !== null &&
        typeof (json as { code?: unknown }).code === "string"
    } catch {
      // Not JSON at all.
    }
    if (isConnectError) return res
    return new Response(body, {
      status: res.status,
      statusText: res.statusText,
      headers: { "content-type": "text/plain" },
    })
  }
}
