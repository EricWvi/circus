import { createClient, type Transport } from "@connectrpc/connect"
import {
  AgentKind as ProtoAgentKind,
  AgentStatus_State as ProtoAgentState,
  type WorkerAgent as ProtoWorkerAgent,
} from "@circus/proto/circus/v1/agent_pb"
import {
  DirectoryError as ProtoDirectoryError,
  GitRemoteError as ProtoGitRemoteError,
} from "@circus/proto/circus/v1/daemon_link_pb"
import {
  EnrollRole as ProtoEnrollRole,
  MachineService as MachineServiceDesc,
  type Worker as ProtoWorker,
} from "@circus/proto/circus/v1/machine_pb"

import type {
  AgentKind,
  AgentState,
  DirectoryError,
  EnrollRole,
  GitRemoteError,
  MachineService,
  Worker,
  WorkerAgent,
} from "@/api/types"

import { call, malformed } from "./call"

const enrollRoles: Record<EnrollRole, ProtoEnrollRole> = {
  daemon: ProtoEnrollRole.DAEMON,
  worker: ProtoEnrollRole.WORKER,
  both: ProtoEnrollRole.BOTH,
}

const directoryErrors: Partial<Record<ProtoDirectoryError, DirectoryError>> = {
  [ProtoDirectoryError.INVALID_PATH]: "invalid_path",
  [ProtoDirectoryError.NOT_FOUND]: "not_found",
  [ProtoDirectoryError.NOT_DIRECTORY]: "not_directory",
  [ProtoDirectoryError.PERMISSION_DENIED]: "permission_denied",
  [ProtoDirectoryError.TIMEOUT]: "timeout",
  [ProtoDirectoryError.IO]: "io",
}

/** A directory error; one this client doesn't know yet reads as an I/O error. */
const directoryError = (error: ProtoDirectoryError): DirectoryError | undefined =>
  error === ProtoDirectoryError.UNSPECIFIED ? undefined : (directoryErrors[error] ?? "io")

const gitRemoteErrors: Partial<Record<ProtoGitRemoteError, GitRemoteError>> = {
  [ProtoGitRemoteError.INVALID_PATH]: "invalid_path",
  [ProtoGitRemoteError.NOT_FOUND]: "not_found",
  [ProtoGitRemoteError.NOT_REPOSITORY]: "not_repository",
  [ProtoGitRemoteError.NO_ORIGIN]: "no_origin",
  [ProtoGitRemoteError.GIT_UNAVAILABLE]: "git_unavailable",
  [ProtoGitRemoteError.TIMEOUT]: "timeout",
  [ProtoGitRemoteError.FAILED]: "failed",
}

/** A git remote error; one this client doesn't know yet reads as a failure. */
const gitRemoteError = (error: ProtoGitRemoteError): GitRemoteError | undefined =>
  error === ProtoGitRemoteError.UNSPECIFIED ? undefined : (gitRemoteErrors[error] ?? "failed")

const agentKinds: Partial<Record<ProtoAgentKind, AgentKind>> = {
  [ProtoAgentKind.CLAUDE_CODE]: "claude_code",
}

const protoAgentKinds: Record<AgentKind, ProtoAgentKind> = {
  claude_code: ProtoAgentKind.CLAUDE_CODE,
}

const agentStates: Partial<Record<ProtoAgentState, AgentState>> = {
  [ProtoAgentState.NOT_INSTALLED]: "not_installed",
  [ProtoAgentState.INSTALLING]: "installing",
  [ProtoAgentState.INSTALLED]: "installed",
  [ProtoAgentState.OUTDATED]: "outdated",
  [ProtoAgentState.FAILED]: "failed",
  [ProtoAgentState.UNINSTALLING]: "uninstalling",
}

/** One agent of a worker; none for an agent this client doesn't know. */
function agentOf(a: ProtoWorkerAgent): WorkerAgent[] {
  const status = a.status
  const agent = status && agentKinds[status.agent]
  if (!agent) return []
  return [
    {
      agent,
      // Never reported, or a state this client doesn't know yet.
      state: agentStates[status.state] ?? "unknown",
      installedVersion: status.installedVersion,
      targetVersion: status.targetVersion,
      error: status.error,
      downloadedBytes: Number(status.downloadedBytes),
      totalBytes: Number(status.totalBytes),
      wanted: a.wanted,
      wantedVersion: a.wantedVersion,
      reportedAtUnixMs: Number(a.reportedAtUnixMs),
    },
  ]
}

const workerOf = (w: ProtoWorker): Worker => ({
  workerId: w.workerId,
  name: w.name,
  online: w.online,
  lastSeenUnixMs: Number(w.lastSeenUnixMs),
  os: w.os,
  arch: w.arch,
  hostname: w.hostname,
  version: w.version,
  localDevices: [...w.localDevices],
  integrator: w.integrator,
  agents: w.agents.flatMap(agentOf),
})

/** The machine service of a real control plane, over Connect. */
export function createConnectMachines(transport: Transport): MachineService {
  const client = createClient(MachineServiceDesc, transport)
  return {
    listDevices: () =>
      call(async () =>
        (await client.listDevices({})).devices.map((d) => ({
          name: d.name,
          online: d.online,
          lastSeenUnixMs: Number(d.lastSeenUnixMs),
          os: d.os,
          arch: d.arch,
          version: d.version,
        })),
      ),
    listWorkers: () => call(async () => (await client.listWorkers({})).workers.map(workerOf)),
    setWorkerGrant: ({ workerId, integrator }) =>
      call(async () => {
        const { worker } = await client.setWorkerGrant({ workerId, integrator })
        if (!worker) throw malformed("worker")
        return workerOf(worker)
      }),
    setWorkerAgent: ({ workerId, agent, install }) =>
      call(async () => {
        const { worker } = await client.setWorkerAgent({ workerId, agent: protoAgentKinds[agent], install })
        if (!worker) throw malformed("worker")
        return workerOf(worker)
      }),
    createEnrollLink: ({ role }) =>
      call(async () => {
        const res = await client.createEnrollLink({ role: enrollRoles[role] })
        if (res.link === "") throw malformed("enrollment link")
        return {
          link: res.link,
          expiresUnixMs: Number(res.expiresUnixMs),
          serverVersion: res.serverVersion,
        }
      }),
    listDirectory: ({ device, path, includeHidden }) =>
      call(async () => {
        const { result } = await client.listDirectory({ device, path, includeHidden })
        if (!result) throw malformed("directory listing")
        const error = directoryError(result.error)
        return {
          path: result.path,
          parent: result.parent,
          home: result.home,
          entries: error
            ? []
            : result.entries.map((e) => ({ name: e.name, gitRepository: e.gitRepository, symlink: e.symlink })),
          truncated: result.truncated,
          ...(error ? { error } : {}),
        }
      }),
    getGitRemote: ({ device, path }) =>
      call(async () => {
        const { result } = await client.getGitRemote({ device, path })
        if (!result) throw malformed("git remote")
        const error = gitRemoteError(result.error)
        return error ? { url: "", error } : { url: result.url }
      }),
  }
}
