import { Code, ConnectError, createRouterTransport, type ServiceImpl } from "@connectrpc/connect"
import { BinaryWriter, WireType } from "@bufbuild/protobuf/wire"
import {
  ProviderService,
  Provider_ApiEndpoint_Scheme,
  Provider_Kind,
  type Provider,
  type UpdateProviderRequest,
} from "@circus/proto/circus/v1/provider_pb"
import { describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"
import type { ModelProviderInput } from "@/api/types"

import { createConnectModelProviders } from "./model-providers"

/** A client against a fake ProviderService, over JSON like the browser's. */
function providers(impl: Partial<ServiceImpl<typeof ProviderService>>) {
  return createConnectModelProviders(
    createRouterTransport(({ service }) => service(ProviderService, impl), {
      transport: { useBinaryFormat: false },
    }),
  )
}

const BIG = 9_007_199_254_740_993n

const protoProvider = {
  providerId: "prov-1",
  name: "Proxy",
  kind: Provider_Kind.CLAUDE_CODE,
  api: {
    baseUrl: "https://proxy.example.com",
    scheme: Provider_ApiEndpoint_Scheme.BEARER,
    key: "",
    keyLast4: "k9Qz",
    opusModel: "big",
    sonnetModel: "",
    haikuModel: "small",
  },
  env: [{ name: "TEAM", valueLast4: "-web" }],
  workerIds: ["w1"],
  maxSessions: 2,
  isDefault: true,
  leasedSessions: 1,
  secretsUnreadable: false,
  createdAtUnixMs: 1_000n,
  updatedAtUnixMs: BIG,
}

const input: ModelProviderInput = {
  name: "Proxy",
  baseUrl: "",
  scheme: "x_api_key",
  key: "",
  opusModel: "",
  sonnetModel: "",
  haikuModel: "",
  env: [{ name: "KEEP" }, { name: "NEW", value: "" }],
  workerIds: ["w1"],
  maxSessions: 0,
  isDefault: false,
}

/** A google.rpc.BadRequest detail naming one field. */
function badRequest(field: string) {
  const w = new BinaryWriter()
  w.tag(1, WireType.LengthDelimited).fork()
  w.tag(1, WireType.LengthDelimited).string(field)
  w.join()
  return { type: "google.rpc.BadRequest", value: w.finish() }
}

describe("model providers over Connect", () => {
  it("maps a provider: secrets only by their last four characters, the version exact", async () => {
    const [p] = await providers({ listProviders: () => ({ providers: [protoProvider] }) }).listProviders()
    expect(p).toEqual({
      providerId: "prov-1",
      name: "Proxy",
      baseUrl: "https://proxy.example.com",
      scheme: "bearer",
      keyLast4: "k9Qz",
      opusModel: "big",
      sonnetModel: "",
      haikuModel: "small",
      env: [{ name: "TEAM", valueLast4: "-web" }],
      workerIds: ["w1"],
      maxSessions: 2,
      isDefault: true,
      leasedSessions: 1,
      secretsUnreadable: false,
      createdAtUnixMs: 1_000,
      updatedAtUnixMs: "9007199254740993",
    })
  })

  it("reads a scheme it doesn't know as unspecified, not as one it knows", async () => {
    const list = await providers({
      listProviders: () => ({
        providers: [
          { ...protoProvider, api: { ...protoProvider.api, scheme: Provider_ApiEndpoint_Scheme.UNSPECIFIED } },
          { ...protoProvider, providerId: "p2", api: { ...protoProvider.api, scheme: 7 as Provider_ApiEndpoint_Scheme } },
          { ...protoProvider, providerId: "p3", api: { ...protoProvider.api, scheme: Provider_ApiEndpoint_Scheme.X_API_KEY } },
        ],
      }),
    }).listProviders()
    expect(list.map((p) => p.scheme)).toEqual(["unspecified", "unspecified", "x_api_key"])
  })

  it("sends an update with the expected version, an empty key and values only where typed", async () => {
    let received: UpdateProviderRequest | undefined
    const client = providers({
      updateProvider: (req) => {
        received = req
        return { provider: { ...protoProvider, updatedAtUnixMs: BIG + 1n } }
      },
    })
    const updated = await client.updateProvider({ ...input, providerId: "prov-1", expectedUpdatedAt: "9007199254740993" })
    expect(received?.expectedUpdatedAtUnixMs).toBe(BIG)
    const sent = received?.provider as Provider
    expect(sent.providerId).toBe("prov-1")
    expect(sent.kind).toBe(Provider_Kind.CLAUDE_CODE)
    expect(sent.api?.key).toBe("")
    expect(sent.api?.scheme).toBe(Provider_ApiEndpoint_Scheme.X_API_KEY)
    // An absent value keeps the stored one; an empty one is a value.
    expect(sent.env.map((v) => [v.name, v.value])).toEqual([
      ["KEEP", undefined],
      ["NEW", ""],
    ])
    expect(updated.updatedAtUnixMs).toBe("9007199254740994")
  })

  it("carries the field of a refusal", async () => {
    const client = providers({
      createProvider: () => {
        const error = new ConnectError("bad", Code.InvalidArgument)
        error.details.push(badRequest("env[1].name"))
        throw error
      },
    })
    const error = await client.createProvider(input).then(
      () => undefined,
      (e: unknown) => e,
    )
    expect(error).toBeInstanceOf(ApiError)
    expect([(error as ApiError).code, (error as ApiError).field]).toEqual(["invalid_argument", "env[1].name"])
  })
})
