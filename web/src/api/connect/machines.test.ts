import { Code, ConnectError, createRouterTransport, type ServiceImpl } from "@connectrpc/connect"
import { DirectoryError, GitRemoteError } from "@circus/proto/circus/v1/daemon_link_pb"
import {
  MachineService,
  type GetGitRemoteRequest,
  type ListDirectoryRequest,
  type SetWorkerGrantRequest,
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
            integrator: true,
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
        integrator: true,
      },
    ])
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
