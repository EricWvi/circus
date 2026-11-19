import { createClient, type Transport } from "@connectrpc/connect"
import {
  EnrollRole as ProtoEnrollRole,
  MachineService as MachineServiceDesc,
} from "@circus/proto/circus/v1/machine_pb"

import type { EnrollRole, MachineService } from "@/api/types"

import { call, malformed } from "./call"

const enrollRoles: Record<EnrollRole, ProtoEnrollRole> = {
  daemon: ProtoEnrollRole.DAEMON,
  worker: ProtoEnrollRole.WORKER,
  both: ProtoEnrollRole.BOTH,
}

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
  }
}
