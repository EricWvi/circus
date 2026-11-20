import { Code, ConnectError, createRouterTransport, type ServiceImpl } from "@connectrpc/connect"
import { DirectoryError } from "@circus/proto/circus/v1/daemon_link_pb"
import { MachineService, type ListDirectoryRequest } from "@circus/proto/circus/v1/machine_pb"
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
