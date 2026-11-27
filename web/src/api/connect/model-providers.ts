import { createClient, type Transport } from "@connectrpc/connect"
import {
  ProviderService as ProviderServiceDesc,
  Provider_ApiEndpoint_Scheme,
  Provider_Kind,
  type Provider as ProtoProvider,
} from "@circus/proto/circus/v1/provider_pb"

import type { ModelProvider, ModelProviderInput, ModelProviderService, ProviderScheme } from "@/api/types"

import { call, malformed } from "./call"
import { toVersion } from "./projects"

const schemes: Record<ProviderScheme, Provider_ApiEndpoint_Scheme> = {
  x_api_key: Provider_ApiEndpoint_Scheme.X_API_KEY,
  bearer: Provider_ApiEndpoint_Scheme.BEARER,
}

const schemeOf = (scheme: Provider_ApiEndpoint_Scheme | undefined): ModelProvider["scheme"] => {
  if (scheme === Provider_ApiEndpoint_Scheme.X_API_KEY) return "x_api_key"
  if (scheme === Provider_ApiEndpoint_Scheme.BEARER) return "bearer"
  return "unspecified"
}

/**
 * The provider as the UI keeps it. `updatedAtUnixMs` stays the exact int64
 * (as a decimal string), since it goes back as the expected version. A
 * scheme this client doesn't know reads as "unspecified": the form makes the
 * user choose one rather than rewrite it silently.
 */
export function toModelProvider(provider: ProtoProvider | undefined, rpc: string): ModelProvider {
  if (!provider) throw malformed(rpc)
  const api = provider.api
  return {
    providerId: provider.providerId,
    name: provider.name,
    baseUrl: api?.baseUrl ?? "",
    scheme: schemeOf(api?.scheme),
    keyLast4: api?.keyLast4 ?? "",
    fableModel: api?.fableModel ?? "",
    opusModel: api?.opusModel ?? "",
    sonnetModel: api?.sonnetModel ?? "",
    haikuModel: api?.haikuModel ?? "",
    env: provider.env.map((v) => ({ name: v.name, valueLast4: v.valueLast4 })),
    workerIds: [...provider.workerIds],
    maxSessions: provider.maxSessions,
    isDefault: provider.isDefault,
    leasedSessions: provider.leasedSessions,
    secretsUnreadable: provider.secretsUnreadable,
    createdAtUnixMs: Number(provider.createdAtUnixMs),
    updatedAtUnixMs: provider.updatedAtUnixMs.toString(),
  }
}

/** The request's provider: only what a request may carry. */
export const toProtoProvider = (input: ModelProviderInput, providerId = "") => ({
  providerId,
  name: input.name,
  kind: Provider_Kind.CLAUDE_CODE,
  api: {
    baseUrl: input.baseUrl,
    scheme: schemes[input.scheme],
    key: input.key,
    fableModel: input.fableModel,
    opusModel: input.opusModel,
    sonnetModel: input.sonnetModel,
    haikuModel: input.haikuModel,
  },
  env: input.env.map((v) => (v.value === undefined ? { name: v.name } : { name: v.name, value: v.value })),
  workerIds: input.workerIds,
  maxSessions: input.maxSessions,
  isDefault: input.isDefault,
})

/** The model provider service of a real control plane, over Connect. */
export function createConnectModelProviders(transport: Transport): ModelProviderService {
  const client = createClient(ProviderServiceDesc, transport)
  return {
    createProvider: (req) =>
      call(async () => toModelProvider((await client.createProvider({ provider: toProtoProvider(req) })).provider, "CreateProvider")),
    listProviders: () =>
      call(async () => (await client.listProviders({})).providers.map((p) => toModelProvider(p, "ListProviders"))),
    getProvider: (req) =>
      call(async () => toModelProvider((await client.getProvider(req)).provider, "GetProvider")),
    updateProvider: ({ providerId, expectedUpdatedAt, ...input }) =>
      call(async () => {
        const res = await client.updateProvider({
          provider: toProtoProvider(input, providerId),
          expectedUpdatedAtUnixMs: expectedUpdatedAt === undefined ? undefined : toVersion(expectedUpdatedAt),
        })
        return toModelProvider(res.provider, "UpdateProvider")
      }),
    deleteProvider: (req) => call(async () => void (await client.deleteProvider({ providerId: req.providerId }))),
  }
}
