// The mock's model providers and the part of scheduling that depends on
// them: which worker may take a session, and why a queued one waits. In
// this tab's memory only, like the sessions.

import { nanoid } from "nanoid"

import { ApiError } from "@/api/errors"
import {
  baseUrlProblem,
  envNameProblem,
  keyProblem,
  MAX_ENV_BYTES,
  MAX_ENV_VARS,
  MAX_PROVIDER_WORKERS,
  MAX_PROVIDERS,
  maxSessionsProblem,
  modelProblem,
  providerNameProblem,
  utf8Length,
} from "@/api/provider-rules"
import type { ModelProvider, ModelProviderInput, ProviderVersion, QueueReason, Worker } from "@/api/types"
import { sameProjectName } from "@/api/validation"
import { DEFAULT_AGENT, localWorker, runsAgent } from "@/lib/agents"

import type { MockSession } from "./sessions"
import type { MockProvider, MockState } from "./state"

/** The worker has the sessions' agent installed at its target version. */
export const runsSessions = (w: Pick<Worker, "agents">) => runsAgent(w, DEFAULT_AGENT)

const invalid = (message: string, field: string) => new ApiError("invalid_argument", message, field)

/** The last four characters of a secret (fewer when shorter). */
const last4 = (secret: string) => [...secret].slice(-4).join("")

export interface MockModelProvidersHost {
  state: MockState
  /** The signed-in user's id; throws when signed out. */
  currentUser(): string
  now(): number
  /** The owner of a session (its project's owner). */
  ownerOf(s: MockSession): string | undefined
  /** Something scheduling depends on changed: queued sessions are looked at again. */
  reschedule(): void
}

export function createMockModelProviders(host: MockModelProvidersHost) {
  const { state } = host

  const leased = (providerId: string) =>
    state.sessions.filter((s) => s.leased && s.state !== "stopped" && (s.providerId ?? "") === providerId).length

  const publicProvider = ({ owner: _, key, env, updatedAtUnixMs, ...p }: MockProvider): ModelProvider => ({
    ...p,
    keyLast4: last4(key),
    env: env.map((v) => ({ name: v.name, valueLast4: last4(v.value) })),
    leasedSessions: leased(p.providerId),
    updatedAtUnixMs: String(updatedAtUnixMs),
  })

  function owned(owner: string, providerId: string): MockProvider {
    const provider = state.providers.find((p) => p.owner === owner && p.providerId === providerId)
    if (!provider) throw new ApiError("not_found", "Provider not found.", "provider_id")
    return provider
  }

  /** The stored fields for a request, checked as the server checks them. */
  function checked(owner: string, input: ModelProviderInput, existing?: MockProvider) {
    const nameProblem = providerNameProblem(input.name ?? "")
    if (nameProblem) throw invalid(nameProblem, "name")
    const name = input.name.trim()
    if (state.providers.some((p) => p.owner === owner && p !== existing && sameProjectName(p.name, name))) {
      throw new ApiError("already_exists", `A provider named "${name}" already exists.`, "name")
    }
    const urlProblem = baseUrlProblem(input.baseUrl)
    if (urlProblem) throw invalid(urlProblem, "api.base_url")
    if (input.scheme !== "x_api_key" && input.scheme !== "bearer") throw invalid("Choose a scheme.", "api.scheme")
    // The stored secrets can't be kept once they can't be read.
    const keep = existing && !existing.secretsUnreadable ? existing : undefined
    const kProblem = keyProblem(input.key, keep === undefined)
    if (kProblem) throw invalid(kProblem, "api.key")
    for (const field of ["fableModel", "opusModel", "sonnetModel", "haikuModel"] as const) {
      const problem = modelProblem(input[field])
      if (problem) throw invalid(problem, `api.${field.replace("Model", "_model")}`)
    }
    if (input.env.length > MAX_ENV_VARS) throw invalid(`At most ${MAX_ENV_VARS} variables.`, "env")
    const env: MockProvider["env"] = []
    for (const [i, v] of input.env.entries()) {
      const problem = envNameProblem(v.name)
      if (problem) throw invalid(problem, `env[${i}].name`)
      if (env.some((e) => e.name === v.name)) throw invalid("Names are distinct.", `env[${i}].name`)
      const value = v.value ?? keep?.env.find((e) => e.name === v.name)?.value
      if (value === undefined) throw invalid("Enter a value.", `env[${i}].value`)
      env.push({ name: v.name, value })
    }
    if (env.reduce((n, v) => n + utf8Length(v.name) + utf8Length(v.value), 0) > MAX_ENV_BYTES) {
      throw invalid("Variables are at most 64 KiB.", "env")
    }
    if (input.workerIds.length > MAX_PROVIDER_WORKERS) throw invalid("Too many workers.", "worker_ids")
    for (const [i, id] of input.workerIds.entries()) {
      const known = state.workers.some((w) => w.owner === owner && w.workerId === id)
      if (!known || input.workerIds.indexOf(id) !== i) throw invalid("Unknown worker.", `worker_ids[${i}]`)
    }
    const maxProblem = maxSessionsProblem(input.maxSessions)
    if (maxProblem) throw invalid(maxProblem, "max_sessions")
    return {
      name,
      baseUrl: input.baseUrl,
      scheme: input.scheme,
      key: input.key === "" && keep ? keep.key : input.key,
      fableModel: input.fableModel,
      opusModel: input.opusModel,
      sonnetModel: input.sonnetModel,
      haikuModel: input.haikuModel,
      env,
      workerIds: [...input.workerIds],
      maxSessions: input.maxSessions,
      isDefault: input.isDefault === true,
      secretsUnreadable: false,
    }
  }

  /** Makes `provider` the owner's only default. */
  function soleDefault(provider: MockProvider) {
    for (const p of state.providers) if (p.owner === provider.owner && p !== provider) p.isDefault = false
  }

  const sorted = (owner: string) =>
    state.providers
      .filter((p) => p.owner === owner)
      .sort((a, b) => a.name.toLowerCase().localeCompare(b.name.toLowerCase()) || a.providerId.localeCompare(b.providerId))

  const service = {
    createProvider(input: ModelProviderInput): ModelProvider {
      const owner = host.currentUser()
      const fields = checked(owner, input)
      if (state.providers.filter((p) => p.owner === owner).length >= MAX_PROVIDERS) {
        throw new ApiError("resource_exhausted", `At most ${MAX_PROVIDERS} providers.`)
      }
      const at = host.now()
      const provider: MockProvider = { owner, providerId: nanoid(12), ...fields, createdAtUnixMs: at, updatedAtUnixMs: at }
      state.providers.push(provider)
      if (provider.isDefault) soleDefault(provider)
      host.reschedule()
      return publicProvider(provider)
    },
    listProviders(): ModelProvider[] {
      return sorted(host.currentUser()).map(publicProvider)
    },
    getProvider({ providerId }: { providerId: string }): ModelProvider {
      return publicProvider(owned(host.currentUser(), providerId))
    },
    updateProvider({
      providerId,
      expectedUpdatedAt,
      ...input
    }: ModelProviderInput & { providerId: string; expectedUpdatedAt?: ProviderVersion }): ModelProvider {
      const owner = host.currentUser()
      const provider = owned(owner, providerId)
      const fields = checked(owner, input, provider)
      if (expectedUpdatedAt !== undefined && expectedUpdatedAt !== String(provider.updatedAtUnixMs)) {
        throw new ApiError("aborted", "The provider changed since it was read.")
      }
      const { owner: _, providerId: __, createdAtUnixMs: ___, updatedAtUnixMs, ...before } = provider
      if (JSON.stringify(before) !== JSON.stringify({ ...before, ...fields })) {
        provider.updatedAtUnixMs = Math.max(host.now(), updatedAtUnixMs + 1)
      }
      Object.assign(provider, fields)
      if (provider.isDefault) soleDefault(provider)
      host.reschedule()
      return publicProvider(provider)
    },
    deleteProvider({ providerId }: { providerId: string }): void {
      const provider = owned(host.currentUser(), providerId)
      if (leased(provider.providerId) > 0) {
        throw new ApiError("failed_precondition", "Sessions using this provider are running.")
      }
      state.providers = state.providers.filter((p) => p !== provider)
      host.reschedule()
    },
  }

  /** A provider of the owner's that exists, or undefined. */
  const find = (owner: string, providerId: string | undefined) =>
    providerId ? state.providers.find((p) => p.owner === owner && p.providerId === providerId) : undefined

  return {
    service,
    /**
     * The provider a new session gets: the one asked for ("" is local
     * login), else the project's default, else the user's, else local
     * login. A default naming a deleted provider counts as unset.
     */
    resolve(owner: string, requested: string | undefined, projectDefault: string | undefined): string {
      if (requested !== undefined) {
        if (requested !== "" && !find(owner, requested)) throw invalid("Unknown provider.", "provider_id")
        return requested
      }
      return (
        find(owner, projectDefault)?.providerId ??
        state.providers.find((p) => p.owner === owner && p.isDefault)?.providerId ??
        ""
      )
    },
    /** Checks a project's default provider: "" or one of the owner's. */
    checkProjectDefault(owner: string, providerId: string): string {
      if (providerId !== "" && !find(owner, providerId)) throw invalid("Unknown provider.", "default_provider_id")
      return providerId
    },
    /** Checks a provider a session is switched to. */
    checkSessionProvider(owner: string, providerId: string) {
      if (providerId !== "" && !find(owner, providerId)) throw invalid("Unknown provider.", "provider_id")
    },
    /**
     * Why the session can't be leased now; `unspecified` when it can. A
     * session in a device's location runs on the worker of that device's
     * machine, if there is one; otherwise on any of the owner's workers.
     */
    queueReasonOf(s: MockSession): QueueReason {
      const owner = host.ownerOf(s)
      const workers = state.workers.filter((w) => w.owner === owner)
      const pinned = localWorker(workers, s.device)
      const candidates: Worker[] = pinned ? [pinned] : workers
      const provider = s.providerId ? find(owner ?? "", s.providerId) : undefined
      if (s.providerId && !provider) return "provider_deleted"
      if (provider?.secretsUnreadable) return "provider_secrets_unreadable"
      const assigned = (w: Worker) => !provider || provider.workerIds.includes(w.workerId)
      if (pinned && !assigned(pinned)) return "no_assigned_worker"
      if (!candidates.some((w) => w.online)) return "worker_offline"
      if (!candidates.some((w) => w.online && assigned(w))) return "no_assigned_worker"
      if (!candidates.some((w) => w.online && assigned(w) && runsSessions(w))) return "agent_not_installed"
      if (!provider) return "unspecified"
      if (provider.maxSessions > 0 && leased(provider.providerId) >= provider.maxSessions) return "provider_full"
      return "unspecified"
    },
  }
}
