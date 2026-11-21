import { SEED_OWNER_ID, seedState, type MockState } from "@/api/mock/state"

/**
 * The demo seed with a project "fresh" whose locations are the default
 * folders on these seed devices, so that sessions there wait for their
 * folder (see `api/mock/workspaces.ts` for what `repoUrl` scripts).
 */
export function withDefaultProject(repoUrl: string, devices: string[] = ["devbox"], state: MockState = seedState()): MockState {
  state.projects.push({
    owner: SEED_OWNER_ID,
    projectId: "fresh",
    name: "fresh",
    repoUrl,
    locations: devices.map((device) => ({ device, path: "", deviceId: `device-${device}` })),
    lastUsedLocation: 0,
    createdAtUnixMs: 0,
    updatedAtUnixMs: 0,
  })
  return state
}
