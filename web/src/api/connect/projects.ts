import { createClient, type Transport } from "@connectrpc/connect"
import {
  ProjectService as ProjectServiceDesc,
  type Project as ProtoProject,
} from "@circus/proto/circus/v1/project_pb"

import type { Location, Project, ProjectService, ProjectVersion } from "@/api/types"

import { call, malformed } from "./call"

/**
 * The project as the UI keeps it. `updatedAtUnixMs` stays the exact int64
 * (as a decimal string), since it goes back as the expected version;
 * `createdAtUnixMs` is only displayed, so a number is fine.
 */
export function toProject(project: ProtoProject | undefined, rpc: string): Project {
  if (!project) throw malformed(rpc)
  return {
    projectId: project.projectId,
    name: project.name,
    repoUrl: project.repoUrl,
    locations: project.locations.map((l) => ({
      device: l.device,
      path: l.path,
      effectivePath: l.effectivePath,
      deviceRevoked: l.deviceRevoked,
    })),
    lastUsedLocation: project.lastUsedLocation,
    createdAtUnixMs: Number(project.createdAtUnixMs),
    updatedAtUnixMs: project.updatedAtUnixMs.toString(),
  }
}

/** Only what a request may carry; output-only fields stay behind. */
const toProtoLocations = (locations: readonly Location[]) =>
  locations.map(({ device, path }) => ({ device, path }))

/** The version as the int64 it came from. */
export function toVersion(version: ProjectVersion): bigint {
  if (!/^-?[0-9]+$/.test(version)) throw new Error(`not a project version: "${version}"`)
  return BigInt(version)
}

/** The project service of a real control plane, over Connect. */
export function createConnectProjects(transport: Transport): ProjectService {
  const client = createClient(ProjectServiceDesc, transport)
  return {
    createProject: (req) =>
      call(async () => {
        const res = await client.createProject({
          name: req.name,
          repoUrl: req.repoUrl,
          locations: toProtoLocations(req.locations),
        })
        return toProject(res.project, "CreateProject")
      }),
    listProjects: () =>
      call(async () => (await client.listProjects({})).projects.map((p) => toProject(p, "ListProjects"))),
    getProject: (req) =>
      call(async () => toProject((await client.getProject(req)).project, "GetProject")),
    updateProject: (req) =>
      call(async () => {
        const res = await client.updateProject({
          projectId: req.projectId,
          name: req.name,
          repoUrl: req.repoUrl,
          lastUsedLocation: req.lastUsedLocation,
          locations: req.locations && { locations: toProtoLocations(req.locations) },
          expectedUpdatedAtUnixMs:
            req.expectedUpdatedAt === undefined ? undefined : toVersion(req.expectedUpdatedAt),
        })
        return toProject(res.project, "UpdateProject")
      }),
    deleteProject: (req) => call(async () => void (await client.deleteProject(req))),
  }
}
