import { Code, ConnectError, createRouterTransport, type ServiceImpl } from "@connectrpc/connect"
import { AgentKind, AgentStatus_State } from "@circus/proto/circus/v1/agent_pb"
import { DirectoryError, GitRemoteError } from "@circus/proto/circus/v1/daemon_link_pb"
import {
  MachineService,
  MachineUpdateFailure,
  MachineUpdateState,
  type GetGitRemoteRequest,
  type ListDirectoryRequest,
  type SetWorkerAgentRequest,
  type SetWorkerGrantRequest,
  type UpdateMachineRequest,
} from "@circus/proto/circus/v1/machine_pb"
import { describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"

import { createConnectMachines } from "./machines"

/** A machine client against a fake MachineService, over JSON like the browser's. */
function machines(impl: Partial<ServiceImpl<typeof MachineService>>) {
  return createConnectMachines(
    createRouterTransport(({ service }) => service(MachineService, impl), {
      transport: { useBinaryFormat: false },
    }),
  )
}

async function rejection(promise: Promise<unknown>): Promise<ApiError> {
  const error = await promise.then(
    () => undefined,
    (e: unknown) => e,
  )
  expect(error).toBeInstanceOf(ApiError)
  return error as ApiError
}

describe("listDirectory over Connect", () => {
  it("sends the request and maps a listing", async () => {
    let received: ListDirectoryRequest | undefined
    const client = machines({
      listDirectory: (req) => {
        received = req
        return {
          result: {
            path: "/home/eric/src",
            parent: "/home/eric",
            home: "/home/eric",
            entries: [
              { name: "circus", gitRepository: true },
              { name: "notes", symlink: true },
              { name: "plain" },
            ],
            truncated: true,
          },
        }
      },
    })
    const listing = await client.listDirectory({ device: "box", path: "~/src", includeHidden: true })
    expect([received?.device, received?.path, received?.includeHidden]).toEqual(["box", "~/src", true])
    expect(listing).toEqual({
      path: "/home/eric/src",
      parent: "/home/eric",
      home: "/home/eric",
      entries: [
        { name: "circus", gitRepository: true, symlink: false },
        { name: "notes", gitRepository: false, symlink: true },
        { name: "plain", gitRepository: false, symlink: false },
      ],
      truncated: true,
    })
    expect(listing).not.toHaveProperty("error")
  })

  it.each([
    [DirectoryError.INVALID_PATH, "invalid_path"],
    [DirectoryError.NOT_FOUND, "not_found"],
    [DirectoryError.NOT_DIRECTORY, "not_directory"],
    [DirectoryError.PERMISSION_DENIED, "permission_denied"],
    [DirectoryError.TIMEOUT, "timeout"],
    [DirectoryError.IO, "io"],
    [99 as DirectoryError, "io"],
  ] as const)("maps path error %s, without entries", async (protoError, error) => {
    const client = machines({
      listDirectory: () => ({
        result: { path: "/x", parent: "/", home: "/home/eric", error: protoError, entries: [{ name: "stray" }] },
      }),
    })
    expect(await client.listDirectory({ device: "box", path: "/x", includeHidden: false })).toEqual({
      path: "/x",
      parent: "/",
      home: "/home/eric",
      entries: [],
      truncated: false,
      error,
    })
  })

  it.each([
    [Code.NotFound, "not_found"],
    [Code.FailedPrecondition, "failed_precondition"],
    [Code.Unavailable, "unavailable"],
    [Code.DeadlineExceeded, "deadline_exceeded"],
    [Code.ResourceExhausted, "resource_exhausted"],
  ] as const)("keeps the code of an RPC error (%s)", async (code, apiCode) => {
    const client = machines({
      listDirectory: () => {
        throw new ConnectError("no", code)
      },
    })
    expect((await rejection(client.listDirectory({ device: "box", path: "", includeHidden: false }))).code).toBe(
      apiCode,
    )
  })

  it("treats a response without a result as malformed", async () => {
    const client = machines({ listDirectory: () => ({}) })
    expect((await rejection(client.listDirectory({ device: "box", path: "", includeHidden: false }))).code).toBe(
      "internal",
    )
  })
})

describe("getGitRemote over Connect", () => {
  it("sends the request and maps a URL", async () => {
    let received: GetGitRemoteRequest | undefined
    const client = machines({
      getGitRemote: (req) => {
        received = req
        return { result: { url: "git@github.com:owner/repo.git" } }
      },
    })
    const remote = await client.getGitRemote({ device: "box", path: "/home/eric/src/repo" })
    expect([received?.device, received?.path]).toEqual(["box", "/home/eric/src/repo"])
    expect(remote).toEqual({ url: "git@github.com:owner/repo.git" })
  })

  it.each([
    [GitRemoteError.INVALID_PATH, "invalid_path"],
    [GitRemoteError.NOT_FOUND, "not_found"],
    [GitRemoteError.NOT_REPOSITORY, "not_repository"],
    [GitRemoteError.NO_ORIGIN, "no_origin"],
    [GitRemoteError.GIT_UNAVAILABLE, "git_unavailable"],
    [GitRemoteError.TIMEOUT, "timeout"],
    [GitRemoteError.FAILED, "failed"],
    [99 as GitRemoteError, "failed"],
  ] as const)("maps error %s, without a URL", async (protoError, error) => {
    const client = machines({
      getGitRemote: () => ({ result: { url: "https://stray.example/x.git", error: protoError } }),
    })
    expect(await client.getGitRemote({ device: "box", path: "/x" })).toEqual({ url: "", error })
  })

  it.each([
    [Code.NotFound, "not_found"],
    [Code.FailedPrecondition, "failed_precondition"],
    [Code.Unavailable, "unavailable"],
    [Code.DeadlineExceeded, "deadline_exceeded"],
    [Code.ResourceExhausted, "resource_exhausted"],
  ] as const)("keeps the code of an RPC error (%s)", async (code, apiCode) => {
    const client = machines({
      getGitRemote: () => {
        throw new ConnectError("no", code)
      },
    })
    expect((await rejection(client.getGitRemote({ device: "box", path: "/x" }))).code).toBe(apiCode)
  })

  it("treats a response without a result as malformed", async () => {
    const client = machines({ getGitRemote: () => ({}) })
    expect((await rejection(client.getGitRemote({ device: "box", path: "/x" }))).code).toBe("internal")
  })
})

describe("listWorkers", () => {
  it("maps workers with the devices on their machine", async () => {
    const list = await machines({
      listWorkers: () => ({
        workers: [
          {
            workerId: "w1",
            name: "devbox",
            online: false,
            lastSeenUnixMs: 1_700_000_000_000n,
            os: "linux",
            arch: "amd64",
            hostname: "devbox.lan",
            version: "0.2.0",
            localDevices: ["devbox"],
            machineKey: "m1",
            integrator: true,
            agents: [
              {
                status: {
                  agent: AgentKind.CLAUDE_CODE,
                  state: AgentStatus_State.INSTALLING,
                  targetVersion: "2.1.3",
                  downloadedBytes: 1_500_000n,
                  totalBytes: 74_000_000n,
                },
                wanted: true,
                wantedVersion: "2.1.3",
                reportedAtUnixMs: 1_699_999_990_000n,
              },
            ],
          },
        ],
      }),
    }).listWorkers()
    expect(list).toEqual([
      {
        workerId: "w1",
        name: "devbox",
        online: false,
        lastSeenUnixMs: 1_700_000_000_000,
        os: "linux",
        arch: "amd64",
        hostname: "devbox.lan",
        version: "0.2.0",
        localDevices: ["devbox"],
        machineKey: "m1",
        integrator: true,
        updateVersion: "",
        agents: [
          {
            agent: "claude_code",
            state: "installing",
            installedVersion: "",
            targetVersion: "2.1.3",
            error: "",
            downloadedBytes: 1_500_000,
            totalBytes: 74_000_000,
            wanted: true,
            wantedVersion: "2.1.3",
            reportedAtUnixMs: 1_699_999_990_000,
          },
        ],
      },
    ])
  })

  it.each([
    [AgentStatus_State.UNSPECIFIED, "unknown"],
    [AgentStatus_State.NOT_INSTALLED, "not_installed"],
    [AgentStatus_State.INSTALLED, "installed"],
    [AgentStatus_State.OUTDATED, "outdated"],
    [AgentStatus_State.FAILED, "failed"],
    [AgentStatus_State.UNINSTALLING, "uninstalling"],
    // A state this client doesn't know yet.
    [99 as AgentStatus_State, "unknown"],
  ])("maps agent state %s to %s", async (state, mapped) => {
    const [worker] = await machines({
      listWorkers: () => ({
        workers: [{ workerId: "w1", agents: [{ status: { agent: AgentKind.CLAUDE_CODE, state } }] }],
      }),
    }).listWorkers()
    expect(worker.agents.map((a) => a.state)).toEqual([mapped])
  })

  it("leaves out agents this client doesn't know, and entries without a status", async () => {
    const [worker] = await machines({
      listWorkers: () => ({
        workers: [
          {
            workerId: "w1",
            agents: [
              { status: { agent: 7 as AgentKind, state: AgentStatus_State.INSTALLED } },
              { status: { agent: AgentKind.UNSPECIFIED } },
              { wanted: true },
              { status: { agent: AgentKind.CLAUDE_CODE, state: AgentStatus_State.FAILED, error: "disk full" } },
            ],
          },
        ],
      }),
    }).listWorkers()
    expect(worker.agents).toMatchObject([{ agent: "claude_code", state: "failed", error: "disk full" }])
  })
})

describe("setWorkerAgent", () => {
  it.each([true, false])("sends install=%s and maps the returned worker", async (install) => {
    let received: SetWorkerAgentRequest | undefined
    const worker = await machines({
      setWorkerAgent: (req) => {
        received = req
        return {
          worker: {
            workerId: req.workerId,
            name: "devbox",
            agents: [{ status: { agent: req.agent, state: AgentStatus_State.INSTALLED }, wanted: req.install }],
          },
        }
      },
    }).setWorkerAgent({ workerId: "w1", agent: "claude_code", install })
    expect([received?.workerId, received?.agent, received?.install]).toEqual(["w1", AgentKind.CLAUDE_CODE, install])
    expect(worker).toMatchObject({ workerId: "w1", agents: [{ agent: "claude_code", state: "installed", wanted: install }] })
  })

  it.each([
    [Code.NotFound, "not_found"],
    [Code.InvalidArgument, "invalid_argument"],
    [Code.FailedPrecondition, "failed_precondition"],
  ] as const)("maps %s", async (code, mapped) => {
    const error = await rejection(
      machines({
        setWorkerAgent: () => {
          throw new ConnectError("refused", code)
        },
      }).setWorkerAgent({ workerId: "w1", agent: "claude_code", install: false }),
    )
    expect(error.code).toBe(mapped)
  })

  it("fails when the answer has no worker", async () => {
    const error = await rejection(
      machines({ setWorkerAgent: () => ({}) }).setWorkerAgent({ workerId: "w1", agent: "claude_code", install: true }),
    )
    expect(error).toBeInstanceOf(ApiError)
  })
})

describe("setWorkerGrant", () => {
  it("sends the grant and maps the returned worker", async () => {
    let received: SetWorkerGrantRequest | undefined
    const worker = await machines({
      setWorkerGrant: (req) => {
        received = req
        return { worker: { workerId: req.workerId, name: "devbox", online: true, integrator: req.integrator } }
      },
    }).setWorkerGrant({ workerId: "w1", integrator: true })
    expect([received?.workerId, received?.integrator]).toEqual(["w1", true])
    expect(worker).toMatchObject({ workerId: "w1", name: "devbox", online: true, integrator: true, localDevices: [] })
  })

  it("maps NOT_FOUND", async () => {
    const client = machines({
      setWorkerGrant: () => {
        throw new ConnectError("no such worker", Code.NotFound)
      },
    })
    expect((await rejection(client.setWorkerGrant({ workerId: "w9", integrator: false }))).code).toBe("not_found")
  })

  it("rejects a response without a worker", async () => {
    const client = machines({ setWorkerGrant: () => ({}) })
    expect((await rejection(client.setWorkerGrant({ workerId: "w1", integrator: true }))).code).toBe("internal")
  })
})

describe("machine updates", () => {
  const running = {
    targetVersion: "v0.2.0",
    state: MachineUpdateState.RUNNING,
    startedUnixMs: 1_700_000_000_000n,
    deadlineUnixMs: 1_700_000_300_000n,
  }

  it("maps the offered version and the machine's update on devices and workers", async () => {
    const client = machines({
      listDevices: () => ({
        devices: [
          { deviceId: "d1", name: "box", updateVersion: "v0.2.0" },
          {
            deviceId: "d2",
            name: "mac",
            update: { ...running, state: MachineUpdateState.FAILED, error: "boom", failure: MachineUpdateFailure.COMMAND },
          },
          // A state this client doesn't know yet reads as no update.
          { deviceId: "d3", name: "pi", update: { ...running, state: 9 as MachineUpdateState } },
          { deviceId: "d4", name: "nuc", update: { ...running, state: MachineUpdateState.FAILED, failure: MachineUpdateFailure.TIMED_OUT } },
          { deviceId: "d5", name: "vm", update: { ...running, state: MachineUpdateState.FAILED, error: "link closed", failure: MachineUpdateFailure.UNREACHABLE } },
          // A failure reason this client doesn't know yet is left out.
          { deviceId: "d6", name: "arm", update: { ...running, state: MachineUpdateState.FAILED, error: "x", failure: 9 as MachineUpdateFailure } },
        ],
      }),
      listWorkers: () => ({ workers: [{ workerId: "w1", name: "box", update: running }] }),
    })
    const devices = await client.listDevices()
    expect(devices.map((d) => [d.updateVersion, d.update])).toEqual([
      ["v0.2.0", undefined],
      [
        "",
        {
          targetVersion: "v0.2.0",
          state: "failed",
          error: "boom",
          failure: "command",
          startedUnixMs: 1_700_000_000_000,
          deadlineUnixMs: 1_700_000_300_000,
        },
      ],
      ["", undefined],
      ["", expect.objectContaining({ state: "failed", error: "", failure: "timed_out" })],
      ["", expect.objectContaining({ state: "failed", error: "link closed", failure: "unreachable" })],
      ["", expect.objectContaining({ state: "failed", error: "x" })],
    ])
    expect("update" in devices[2]).toBe(false)
    expect("failure" in devices[5].update!).toBe(false)
    const [worker] = await client.listWorkers()
    expect(worker.update).toEqual({
      targetVersion: "v0.2.0",
      state: "running",
      error: "",
      startedUnixMs: 1_700_000_000_000,
      deadlineUnixMs: 1_700_000_300_000,
    })
  })

  it.each([
    [{ deviceId: "d1" }, { case: "deviceId", value: "d1" }],
    [{ workerId: "w1" }, { case: "workerId", value: "w1" }],
  ] as const)("sends the card pressed (%o) and maps the update", async (req, identity) => {
    let received: UpdateMachineRequest | undefined
    const client = machines({
      updateMachine: (r) => {
        received = r
        return { update: running }
      },
    })
    expect((await client.updateMachine(req)).state).toBe("running")
    expect(received?.identity).toEqual(identity)
  })

  it("maps errors and treats an answer without an update as malformed", async () => {
    const refused = machines({
      updateMachine: () => {
        throw new ConnectError("no update", Code.FailedPrecondition)
      },
    })
    expect((await rejection(refused.updateMachine({ deviceId: "d1" }))).code).toBe("failed_precondition")
    const empty = machines({ updateMachine: () => ({}) })
    expect((await rejection(empty.updateMachine({ workerId: "w1" }))).code).toBe("internal")
  })
})
