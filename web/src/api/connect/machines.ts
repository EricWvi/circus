import { createClient, type Transport } from "@connectrpc/connect"
import { MachineService as MachineServiceDesc } from "@circus/proto/circus/v1/machine_pb"

import type { MachineService } from "@/api/types"

import { call } from "./call"

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
  }
}
