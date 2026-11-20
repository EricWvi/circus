import { createClient, type Transport } from "@connectrpc/connect"
import { DirectoryError as ProtoDirectoryError } from "@circus/proto/circus/v1/daemon_link_pb"
import {
  EnrollRole as ProtoEnrollRole,
  MachineService as MachineServiceDesc,
} from "@circus/proto/circus/v1/machine_pb"

import type { DirectoryError, EnrollRole, MachineService } from "@/api/types"

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
  }
}
