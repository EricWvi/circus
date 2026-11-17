import { BinaryWriter, WireType } from "@bufbuild/protobuf/wire"
import { Code, ConnectError, createRouterTransport, type ServiceImpl } from "@connectrpc/connect"
import { createConnectTransport } from "@connectrpc/connect-web"
import { MachineService } from "@circus/proto/circus/v1/machine_pb"
import { ProjectService, type UpdateProjectRequest } from "@circus/proto/circus/v1/project_pb"
import { describe, expect, it } from "vitest"

import { ApiError } from "@/api/errors"

import { createConnectMachines } from "./machines"
import { createConnectProjects, toVersion } from "./projects"

/** Past Number.MAX_SAFE_INTEGER: a `number` would round it to ...992. */
const BIG = 9007199254740993n

const protoProject = {
  projectId: "p1",
  name: "web",
  repoUrl: "git@github.com:owner/web.git",
  locations: [
    { device: "box", path: "/src/web", effectivePath: "", deviceRevoked: false },
    { device: "old", path: "", effectivePath: "~/circus/projects/p1", deviceRevoked: true },
  ],
  lastUsedLocation: 1,
  createdAtUnixMs: 1_700_000_000_000n,
  updatedAtUnixMs: BIG,
}

/** Clients against a fake ProjectService, over JSON like the browser's. */
function projects(impl: Partial<ServiceImpl<typeof ProjectService>>) {
  return createConnectProjects(
    createRouterTransport(({ service }) => service(ProjectService, impl), {
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

/** A google.rpc.BadRequest detail naming one field. */
function badRequest(field: string) {
  const w = new BinaryWriter()
  w.tag(1, WireType.LengthDelimited).fork()
  w.tag(1, WireType.LengthDelimited).string(field)
  w.join()
  return { type: "google.rpc.BadRequest", value: w.finish() }
}

describe("projects over Connect", () => {
  it("maps a project, keeping its version exact", async () => {
    const project = await projects({ getProject: () => ({ project: protoProject }) }).getProject({ projectId: "p1" })
    expect(project).toEqual({
      projectId: "p1",
      name: "web",
      repoUrl: "git@github.com:owner/web.git",
      locations: [
        { device: "box", path: "/src/web", effectivePath: "", deviceRevoked: false },
        { device: "old", path: "", effectivePath: "~/circus/projects/p1", deviceRevoked: true },
      ],
      lastUsedLocation: 1,
      createdAtUnixMs: 1_700_000_000_000,
      updatedAtUnixMs: "9007199254740993",
    })
  })

  it("sends the version back unchanged as the expected one", async () => {
    let received: UpdateProjectRequest | undefined
    const client = projects({
      getProject: () => ({ project: protoProject }),
      updateProject: (req) => {
        received = req
        return { project: { ...protoProject, updatedAtUnixMs: BIG + 1n } }
      },
    })
    const project = await client.getProject({ projectId: "p1" })
    const updated = await client.updateProject({
      projectId: "p1",
      name: "web2",
      locations: project.locations,
      expectedUpdatedAt: project.updatedAtUnixMs,
    })
    expect(received?.expectedUpdatedAtUnixMs).toBe(BIG)
    expect(updated.updatedAtUnixMs).toBe("9007199254740994")
    // Only what a request carries: output-only fields stay behind.
    expect(received?.locations?.locations.map((l) => [l.device, l.path, l.effectivePath, l.deviceRevoked])).toEqual([
      ["box", "/src/web", "", false],
      ["old", "", "", false],
    ])
  })

  it("leaves unset fields unset", async () => {
    let received: UpdateProjectRequest | undefined
    const client = projects({
      updateProject: (req) => {
        received = req
        return { project: protoProject }
      },
    })
    await client.updateProject({ projectId: "p1", lastUsedLocation: 0 })
    expect(received?.lastUsedLocation).toBe(0)
    expect(received?.name).toBeUndefined()
    expect(received?.repoUrl).toBeUndefined()
    expect(received?.locations).toBeUndefined()
    expect(received?.expectedUpdatedAtUnixMs).toBeUndefined()
    // "" clears the repository URL, so it must be sent, not dropped.
    await client.updateProject({ projectId: "p1", repoUrl: "", locations: [] })
    expect(received?.repoUrl).toBe("")
    expect(received?.locations?.locations).toEqual([])
  })

  it("puts the int64 on the wire as an exact decimal string", async () => {
    const bodies: string[] = []
    const client = createConnectProjects(
      createConnectTransport({
        baseUrl: "https://circus.test",
        fetch: async (_input, init) => {
          const body = init?.body
          bodies.push(typeof body === "string" ? body : new TextDecoder().decode(body as Uint8Array))
          return new Response(
            `{"project":{"projectId":"p1","name":"web","updatedAtUnixMs":"9007199254740993","createdAtUnixMs":"1700000000000"}}`,
            { headers: { "content-type": "application/json" } },
          )
        },
      }),
    )
    const project = await client.getProject({ projectId: "p1" })
    expect(project.updatedAtUnixMs).toBe("9007199254740993")
    await client.updateProject({ projectId: "p1", name: "x", expectedUpdatedAt: project.updatedAtUnixMs })
    expect(JSON.parse(bodies[1])).toEqual({ projectId: "p1", name: "x", expectedUpdatedAtUnixMs: "9007199254740993" })
  })

  it("refuses a version that isn't an integer", () => {
    expect(toVersion("12")).toBe(12n)
    expect(() => toVersion("1.5")).toThrow(/not a project version/)
  })

  it("maps a stale update to aborted, without a field", async () => {
    const error = await rejection(
      projects({
        updateProject: () => {
          throw new ConnectError("the project changed since it was read", Code.Aborted)
        },
      }).updateProject({ projectId: "p1", name: "x", expectedUpdatedAt: "1" }),
    )
    expect(error.code).toBe("aborted")
    expect(error.field).toBeUndefined()
  })

  it("keeps the code and the field of other errors", async () => {
    const fail = (code: Code, field?: string) =>
      rejection(
        projects({
          createProject: () => {
            const error = new ConnectError("no", code)
            if (field) error.details.push(badRequest(field))
            throw error
          },
        }).createProject({ name: "x", repoUrl: "", locations: [] }),
      )
    expect(await fail(Code.InvalidArgument, "locations[2].path")).toMatchObject({
      code: "invalid_argument",
      field: "locations[2].path",
    })
    expect(await fail(Code.InvalidArgument, "repo_url")).toMatchObject({ field: "repo_url" })
    expect(await fail(Code.AlreadyExists, "name")).toMatchObject({ code: "already_exists", field: "name" })
    expect(await fail(Code.ResourceExhausted)).toMatchObject({ code: "resource_exhausted" })
    expect(await fail(Code.NotFound)).toMatchObject({ code: "not_found" })
  })

  it("treats a response without a project as malformed", async () => {
    const error = await rejection(projects({ getProject: () => ({}) }).getProject({ projectId: "p1" }))
    expect(error.code).toBe("internal")
  })

  it("lists and deletes", async () => {
    let deleted = ""
    const client = projects({
      listProjects: () => ({ projects: [protoProject, { ...protoProject, projectId: "p2", locations: [] }] }),
      deleteProject: (req) => {
        deleted = req.projectId
        return {}
      },
    })
    expect((await client.listProjects()).map((p) => [p.projectId, p.locations.length])).toEqual([
      ["p1", 2],
      ["p2", 0],
    ])
    await client.deleteProject({ projectId: "p2" })
    expect(deleted).toBe("p2")
  })
})

describe("devices over Connect", () => {
  it("maps devices", async () => {
    const client = createConnectMachines(
      createRouterTransport(({ service }) =>
        service(MachineService, {
          listDevices: () => ({
            devices: [
              { name: "box", online: true, lastSeenUnixMs: 1_700_000_000_000n, os: "linux", arch: "amd64", version: "0.1.0" },
              { name: "new", online: false, lastSeenUnixMs: 0n, os: "", arch: "", version: "" },
            ],
          }),
        }),
      ),
    )
    expect(await client.listDevices()).toEqual([
      { name: "box", online: true, lastSeenUnixMs: 1_700_000_000_000, os: "linux", arch: "amd64", version: "0.1.0" },
      { name: "new", online: false, lastSeenUnixMs: 0, os: "", arch: "", version: "" },
    ])
  })
})
